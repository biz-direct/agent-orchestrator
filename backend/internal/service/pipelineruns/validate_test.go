package pipelineruns_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/pipeline"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/pipelineruns"
)

func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("validation tests use /bin/sh")
	}
}

// validatedStage is a specialist stage whose profile declares validation
// commands, driven to the point where the specialist is about to submit.
type validatedStage struct {
	*specialistStage
}

func profileWithCommands(commands string) string {
	return scopedTesterProfile + "validation:\n" + commands
}

func newValidatedStage(t *testing.T, commands string, trusted bool) *validatedStage {
	t.Helper()
	skipOnWindows(t)
	f := newFixture(t, map[string]string{
		".ao/pipelines/workflows/build-test.yaml": buildTestWorkflow,
		".ao/pipelines/profiles/tester.yaml":      profileWithCommands(commands),
		"src/main.go":                             "package src\n",
	})
	exec := &fakeExecutor{store: f.store, repo: f.repo}
	f.svc = pipelineruns.New(pipelineruns.Deps{Store: f.store, Messenger: f.messenger, Executor: exec})
	if trusted {
		if _, _, err := f.store.SetProjectPipelineCommandTrust(context.Background(), "proj", true); err != nil {
			t.Fatal(err)
		}
	}
	s := &staged{fixture: f, exec: exec, gate: pipelineruns.NewStoreGate(f.store, nil)}
	run := s.startBuildTest()
	s.buildAndSubmit(run)
	if err := s.svc.DriveHandoffs(context.Background()); err != nil {
		t.Fatal(err)
	}
	view := s.get()
	att := view.Attempts[1]
	return &validatedStage{&specialistStage{staged: s, spec: domain.SessionID(att.ConversationSessionID), view: view, attempt: att}}
}

// submitPass submits a passing report for a clean checkpoint and returns the
// submit result, which is pending until validation has run.
func (v *validatedStage) submitPass() pipelineruns.SubmitResult {
	v.t.Helper()
	head := v.commit("tests", func() { writeFile(v.t, v.repo, "src/feature_test.go", "package src\n") })
	res, err := v.submitSpec("succeeded", head, "k", passReport())
	if err != nil {
		v.t.Fatal(err)
	}
	return res
}

func (v *validatedStage) validate() pipelineruns.RunView {
	v.t.Helper()
	if err := v.svc.DriveHandoffs(context.Background()); err != nil {
		v.t.Fatal(err)
	}
	return v.get()
}

// brief renders a run compactly so failures stay readable.
func brief(v pipelineruns.RunView) string {
	var stages []string
	for _, st := range v.Stages {
		stages = append(stages, st.ID+"="+st.State)
	}
	return fmt.Sprintf("state=%s pause=%s/%q stages=[%s] repairsUsed=%d", v.State, v.PauseReason, v.PauseDetail, strings.Join(stages, " "), v.RepairsUsed)
}

func commandsByID(att pipelineruns.AttemptView) map[string]pipelineruns.CommandResultView {
	out := map[string]pipelineruns.CommandResultView{}
	for _, c := range att.Validation {
		out[c.CommandID] = c
	}
	return out
}

func TestPassingAgentReportIsNotEnoughUntilAOsOwnChecksPass(t *testing.T) {
	v := newValidatedStage(t, "  - {id: unit, command: \"echo ran-unit\"}\n  - {id: lint, command: \"echo ran-lint\", required: false}\n", true)
	res := v.submitPass()
	if res.Accepted || res.Run.State != "running" || res.Run.Stages[1].State != "validating" || res.Run.Attempts[1].State != "validating" {
		t.Fatalf("the agent's success is a claim: the stage must wait for AO's checks: %+v", res.Run.Stages)
	}
	if len(res.Run.Attempts[1].Validation) != 0 {
		t.Fatal("no check has run yet")
	}
	// Validation reserves the worktree: nobody may write while AO's runner owns it.
	if v.admitted(v.spec) || v.admitted(v.sessionID) {
		t.Fatal("no agent may write the shared worktree while commands run")
	}

	view := v.validate()
	if view.State != "completed" || view.Attempts[1].State != "accepted" {
		t.Fatalf("after passing checks: %s", brief(view))
	}
	cmds := commandsByID(view.Attempts[1])
	unit, lint := cmds["unit"], cmds["lint"]
	if unit.Status != "passed" || unit.ExitCode != 0 || !strings.Contains(unit.Log, "ran-unit") || unit.Revision != view.Attempts[1].OutputCommit || unit.Round != 1 || !unit.Required || unit.Command != "echo ran-unit" {
		t.Fatalf("evidence must identify the command, its revision, and its output: %+v", unit)
	}
	if lint.Status != "passed" || lint.Required {
		t.Fatalf("optional check: %+v", lint)
	}
	if unit.FinishedAt == nil || unit.StartedAt.IsZero() {
		t.Fatalf("timing: %+v", unit)
	}
	// The stage ran its relinquish before the checks.
	if len(v.exec.relinquish) < 2 {
		t.Fatalf("the specialist executor must be proven stopped before AO runs commands: %v", v.exec.relinquish)
	}
}

func TestFalseAgentSuccessCannotBypassAFailedMandatoryCheck(t *testing.T) {
	v := newValidatedStage(t, "  - {id: unit, command: \"echo broken; exit 3\"}\n  - {id: after, command: \"echo never\"}\n", true)
	v.submitPass()
	view := v.validate()
	if view.State != "running" || view.CurrentStageID != "build" || view.RepairsUsed != 1 {
		t.Fatalf("a genuine failure is routed to Build and counted once: %s", brief(view))
	}
	att := view.Attempts[1]
	if att.State != "failed" || att.Outcome != "validation_failed" || att.Report == nil {
		t.Fatalf("the agent's report is retained next to the failing evidence: %+v", att)
	}
	cmds := commandsByID(att)
	if cmds["unit"].Status != "failed" || cmds["unit"].ExitCode != 3 || !strings.Contains(cmds["unit"].Log, "broken") {
		t.Fatalf("unit: %+v", cmds["unit"])
	}
	if cmds["after"].Status != "skipped" {
		t.Fatalf("checks after a mandatory failure are recorded as skipped, never guessed: %+v", cmds["after"])
	}
	if len(view.Evidence) != 1 || view.Evidence[0].Outcome != "validation_failed" {
		t.Fatalf("evidence: %+v", view.Evidence)
	}
}

func TestAnOptionalFailureDoesNotBlockAdvancement(t *testing.T) {
	v := newValidatedStage(t, "  - {id: unit, command: \"true\"}\n  - {id: style, command: \"exit 9\", required: false}\n", true)
	v.submitPass()
	view := v.validate()
	if view.State != "completed" || commandsByID(view.Attempts[1])["style"].Status != "failed" {
		t.Fatalf("only mandatory checks gate the stage: %s", brief(view))
	}
}

func TestRepositoryCommandsNeedExplicitAuthorization(t *testing.T) {
	v := newValidatedStage(t, "  - {id: unit, command: \"touch ran.marker\"}\n", false)
	v.submitPass()
	view := v.validate()
	if view.State != "paused" || view.PauseReason != "commands_not_authorized" || view.RepairsUsed != 0 {
		t.Fatalf("run: %s", brief(view))
	}
	if _, err := os.Stat(filepath.Join(v.repo, "ran.marker")); err == nil {
		t.Fatal("an unauthorized repository command must never run")
	}
	if len(view.Attempts[1].Validation) != 0 || view.Attempts[1].State != "validating" {
		t.Fatalf("nothing is recorded as run: %+v", view.Attempts[1])
	}
}

func TestSnapshottedCommandsCannotBeReplacedDuringTheRun(t *testing.T) {
	v := newValidatedStage(t, "  - {id: unit, command: \"echo original\"}\n", true)
	// The repository (or the agent) rewrites the profile after the run started.
	writeFile(t, v.repo, ".ao/pipelines/profiles/tester.yaml", profileWithCommands("  - {id: unit, command: \"echo replaced\"}\n"))
	git(t, v.repo, "add", "-A")
	git(t, v.repo, "commit", "-q", "-m", "tamper")
	head := git(t, v.repo, "rev-parse", "HEAD")
	if _, err := v.submitSpec("succeeded", head, "k", passReport()); err == nil {
		// The edit is in .ao/pipelines, outside the tester's scope, so it is also refused.
		t.Fatal("editing the pipeline definition is outside the tester's scope")
	}
	// Revert the tamper and finish normally.
	fixed := v.commit("revert", func() {
		writeFile(t, v.repo, ".ao/pipelines/profiles/tester.yaml", profileWithCommands("  - {id: unit, command: \"echo original\"}\n"))
	})
	if _, err := v.submitSpec("succeeded", fixed, "k2", passReport()); err != nil {
		t.Fatal(err)
	}
	view := v.validate()
	if got := commandsByID(view.Attempts[1])["unit"].Command; got != "echo original" {
		t.Fatalf("the snapshotted command must run, got %q", got)
	}
}

func TestOperationalProblemsPauseWithoutBeingReadAsCodeDefects(t *testing.T) {
	v := newValidatedStage(t, "  - {id: unit, command: \"ao-definitely-not-a-real-binary-xyz\"}\n", true)
	v.submitPass()
	view := v.validate()
	if view.State != "paused" || view.PauseReason != "validation_operational" || view.RepairsUsed != 0 {
		t.Fatalf("a missing tool is a setup problem and pauses; it is never read as a code defect: %s", brief(view))
	}
	if got := commandsByID(view.Attempts[1])["unit"]; got.Status != "operational" || got.ExitCode != 127 {
		t.Fatalf("status: %+v", got)
	}
	if view.Attempts[1].State != "validating" {
		t.Fatalf("the attempt stays recoverable, not failed: %s", view.Attempts[1].State)
	}
}

func TestTimeoutKillsTheWholeProcessGroup(t *testing.T) {
	skipOnWindows(t)
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	v := newValidatedStage(t, fmt.Sprintf("  - {id: unit, command: %q, timeoutSeconds: 1}\n", "sleep 300 & echo $! > "+pidFile+"; wait"), true)
	v.submitPass()
	started := time.Now()
	view := v.validate()
	if time.Since(started) > 30*time.Second {
		t.Fatal("the timeout must be enforced")
	}
	if view.State != "paused" || view.PauseReason != "validation_operational" || commandsByID(view.Attempts[1])["unit"].Status != "timeout" {
		t.Fatalf("a timeout is operational, not a verdict: %s", brief(view))
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the command should have recorded its child's pid: %v", err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("child process %d outlived the validation round", pid)
}

func TestCancellationStopsTheCommandAndPausesAsInterrupted(t *testing.T) {
	skipOnWindows(t)
	marker := filepath.Join(t.TempDir(), "started")
	v := newValidatedStage(t, fmt.Sprintf("  - {id: unit, command: %q}\n", "touch "+marker+"; sleep 300"), true)
	v.submitPass()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			if _, err := os.Stat(marker); err == nil {
				cancel()
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	if err := v.svc.DriveHandoffs(ctx); err != nil {
		t.Fatal(err)
	}
	view := v.get()
	if view.State != "paused" || view.PauseReason != "validation_interrupted" || commandsByID(view.Attempts[1])["unit"].Status != "cancelled" {
		t.Fatalf("cancelled validation: %s", brief(view))
	}
}

func TestLogsAreBoundedSanitizedAndKeepTheVerdict(t *testing.T) {
	script := `printf '\033[31mred\033[0m\n'; printf 'token=ghp_abcdefghijklmnopqrstuvwxyz0123456789\n'; i=0; while [ $i -lt 4000 ]; do echo "line $i padding padding padding padding padding"; i=$((i+1)); done; echo FINAL-VERDICT; exit 0`
	v := newValidatedStage(t, fmt.Sprintf("  - {id: unit, command: %q}\n", script), true)
	v.submitPass()
	view := v.validate()
	got := commandsByID(view.Attempts[1])["unit"]
	if got.Status != "passed" || !got.LogTruncated || len(got.Log) > 40<<10 {
		t.Fatalf("logs must be bounded: truncated=%v len=%d", got.LogTruncated, len(got.Log))
	}
	if !strings.Contains(got.Log, "FINAL-VERDICT") || !strings.Contains(got.Log, "bytes omitted") {
		t.Fatal("the head and tail survive and the cut is marked")
	}
	if strings.Contains(got.Log, "\x1b") || strings.Contains(got.Log, "ghp_abcdefghijklmnopqrstuvwxyz") || !strings.Contains(got.Log, "[redacted]") {
		t.Fatalf("logs must be sanitized and redacted: %q", got.Log[:200])
	}
}

func TestCommandsDoNotInheritAOInternalEnvironment(t *testing.T) {
	t.Setenv("AO_SESSION_ID", "leak-me")
	t.Setenv("AO_BROWSER_CAPABILITY", "leak-me-too")
	t.Setenv("SOME_API_TOKEN", "super-secret-token-value")
	v := newValidatedStage(t, "  - {id: unit, command: \"echo [$AO_SESSION_ID][$AO_BROWSER_CAPABILITY][$AO_PIPELINE_VALIDATION][$SOME_API_TOKEN]\"}\n", true)
	v.submitPass()
	log := commandsByID(v.validate().Attempts[1])["unit"].Log
	if !strings.Contains(log, "[][][1]") && !strings.Contains(log, "[][][1][[redacted]]") {
		t.Fatalf("AO internals must not reach repository commands, and secrets must be redacted: %q", log)
	}
	if strings.Contains(log, "super-secret-token-value") || strings.Contains(log, "leak-me") {
		t.Fatalf("leaked: %q", log)
	}
}

func TestChecksThatModifyTrackedFilesBlockAdvancementAndArePreserved(t *testing.T) {
	v := newValidatedStage(t, "  - {id: unit, command: \"echo mutated >> src/main.go\"}\n", true)
	v.submitPass()
	view := v.validate()
	if view.State != "paused" || view.PauseReason != "validation_mutated_workspace" || view.Attempts[1].State != "validating" {
		t.Fatalf("run: %s", brief(view))
	}
	body, _ := os.ReadFile(filepath.Join(v.repo, "src", "main.go"))
	if !strings.Contains(string(body), "mutated") {
		t.Fatal("unexpected changes are preserved, never reset")
	}
}

func TestUntrackedBuildOutputIsTolerated(t *testing.T) {
	v := newValidatedStage(t, "  - {id: unit, command: \"echo built > build-output.bin\"}\n", true)
	v.submitPass()
	if view := v.validate(); view.State != "completed" {
		t.Fatalf("untracked artifacts are not unexpected tracked changes: %s", brief(view))
	}
}

func TestValidationNeverStartsFromAMovedCheckpoint(t *testing.T) {
	v := newValidatedStage(t, "  - {id: unit, command: \"touch ran.marker\"}\n", true)
	v.submitPass()
	v.exec.onRelinquish = func() {
		writeFile(t, v.repo, "late.txt", "x\n")
		git(t, v.repo, "add", "-A")
		git(t, v.repo, "commit", "-q", "-m", "late")
	}
	view := v.validate()
	if view.State != "paused" || view.PauseReason != "unexpected_changes" {
		t.Fatalf("run: %s", brief(view))
	}
	if _, err := os.Stat(filepath.Join(v.repo, "ran.marker")); err == nil {
		t.Fatal("checks must not run against a revision the stage did not submit")
	}
}

func TestUnprovableRelinquishmentBlocksValidation(t *testing.T) {
	v := newValidatedStage(t, "  - {id: unit, command: \"touch ran.marker\"}\n", true)
	v.submitPass()
	v.exec.relinquishErr = fmt.Errorf("agent still active")
	view := v.validate()
	if view.State != "paused" || view.PauseReason != "handoff_uncertain" {
		t.Fatalf("run: %s", brief(view))
	}
	if _, err := os.Stat(filepath.Join(v.repo, "ran.marker")); err == nil {
		t.Fatal("no process may write the worktree while the agent may still be running")
	}
}

func TestConcurrentValidationRequestsRunTheChecksOnce(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "count")
	v := newValidatedStage(t, fmt.Sprintf("  - {id: unit, command: %q}\n", "echo x >> "+counter+"; sleep 1"), true)
	v.submitPass()
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = v.svc.DriveHandoffs(context.Background())
		}()
	}
	wg.Wait()
	raw, _ := os.ReadFile(counter)
	if lines := strings.Count(string(raw), "x"); lines != 1 {
		t.Fatalf("the exclusive validation slot must collapse overlapping requests; command ran %d times", lines)
	}
}

func TestInterruptedCommandsStayUnknownAndAreNeverInferredOrRetried(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "count")
	v := newValidatedStage(t, fmt.Sprintf("  - {id: unit, command: %q}\n", "echo x >> "+counter), true)
	v.submitPass()
	ctx := context.Background()
	// A previous daemon died with this command marked running.
	att := v.get().Attempts[1]
	if _, err := v.store.CreatePipelineCommandResult(ctx, domain.PipelineCommandResult{
		AttemptID: att.ID, Round: 1, Ordinal: 0, CommandID: "unit", Command: "echo x", Required: true, Revision: att.OutputCommit,
		Status: domain.PipelineCommandRunning, StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := v.svc.ReconcileAll(ctx); err != nil {
		t.Fatal(err)
	}
	view := v.get()
	if view.State != "paused" || view.PauseReason != "validation_interrupted" {
		t.Fatalf("run: %s", brief(view))
	}
	if got := commandsByID(view.Attempts[1])["unit"]; got.Status != "unknown" || !strings.Contains(got.Detail, "unknown") {
		t.Fatalf("an ambiguous command must be recorded as unknown, not passed: %+v", got)
	}
	if err := v.svc.DriveHandoffs(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(counter); err == nil {
		t.Fatal("a possibly-executed command must not be blindly retried")
	}
}

func TestProfileWithoutCommandsStillAcceptsImmediately(t *testing.T) {
	// The parent suite's specialist profile declares no validation.
	s := newSpecialistStage(t)
	head := s.commit("tests", func() { writeFile(t, s.repo, "src/feature_test.go", "package src\n") })
	res, err := s.submitSpec("succeeded", head, "k", passReport())
	if err != nil || !res.Accepted || res.Run.State != "completed" {
		t.Fatalf("no commands means nothing to validate: %+v err=%v", res.Run, err)
	}
	_ = pipeline.StageSpecialist
}

func TestCommandsReceiveAnAllowlistedEnvironmentNotTheDaemonsCredentials(t *testing.T) {
	for k, v := range map[string]string{
		"AWS_SECRET_ACCESS_KEY": "aws-secret-value", "GITHUB_TOKEN": "gh-token-value", "NPM_TOKEN": "npm-token-value",
		"ANTHROPIC_API_KEY": "anthropic-key-value", "SSH_AUTH_SOCK": "/tmp/agent-sock", "AO_SESSION_ID": "leak-me",
		"GOPATH": "/allowed/gopath", "LC_ALL": "C",
	} {
		t.Setenv(k, v)
	}
	v := newValidatedStage(t, "  - {id: unit, command: \"echo path=[$PATH] home=[$HOME] gopath=[$GOPATH] lc=[$LC_ALL] aws=[$AWS_SECRET_ACCESS_KEY] gh=[$GITHUB_TOKEN] npm=[$NPM_TOKEN] llm=[$ANTHROPIC_API_KEY] ssh=[$SSH_AUTH_SOCK] ao=[$AO_SESSION_ID]\"}\n", true)
	v.submitPass()
	log := commandsByID(v.validate().Attempts[1])["unit"].Log
	for _, want := range []string{"gopath=[/allowed/gopath]", "lc=[C]", "aws=[]", "gh=[]", "npm=[]", "llm=[]", "ssh=[]", "ao=[]"} {
		if !strings.Contains(log, want) {
			t.Errorf("want %q in %q", want, log)
		}
	}
	if strings.Contains(log, "path=[]") || strings.Contains(log, "home=[]") {
		t.Errorf("PATH and HOME must still be inherited: %q", log)
	}
}
