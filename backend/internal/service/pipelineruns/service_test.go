package pipelineruns_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/pipelineruns"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

const buildOnlyWorkflow = `version: 1
id: build-only
description: Single Build stage
instructions: Follow the repository contribution rules.
stages:
  - {id: build, kind: build}
`

const reviewWorkflow = `version: 1
id: build-review
description: Build then review
stages:
  - {id: build, kind: build}
  - {id: review, kind: review, repairTo: build}
`

const testerProfile = `version: 1
id: tester
description: Tester
instructions: write tests
allowedPaths: ["**/*_test.go"]
`

type fakeMessenger struct {
	mu   sync.Mutex
	sent []string
	err  error
}

func (m *fakeMessenger) Send(_ context.Context, _ domain.SessionID, msg string, _ *ports.SpawnAttachment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.sent = append(m.sent, msg)
	return nil
}

type fixture struct {
	t         *testing.T
	store     *sqlite.Store
	svc       *pipelineruns.Service
	messenger *fakeMessenger
	repo      string // project root, also the worker workspace
	sessionID domain.SessionID
	gen       string
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newFixture(t *testing.T, files map[string]string) *fixture {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "task-branch")
	writeFile(t, repo, "README.md", "hello\n")
	for rel, body := range files {
		writeFile(t, repo, rel, body)
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", "init")

	store := sqlitetest.MustOpen(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "proj", Path: repo, RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	rec, err := store.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "proj", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat,
		Activity:  domain.Activity{State: domain.ActivityActive, LastActivityAt: now},
		Metadata:  domain.SessionMetadata{Branch: "task-branch", WorkspacePath: repo, ControllerGeneration: "gen-1", Model: "opus"},
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	msgr := &fakeMessenger{}
	return &fixture{
		t: t, store: store, messenger: msgr, repo: repo, sessionID: rec.ID, gen: "gen-1",
		svc: pipelineruns.New(pipelineruns.Deps{Store: store, Messenger: msgr}),
	}
}

func (f *fixture) start(requestedBy string, overrides map[string]pipelineruns.StageOverride) (pipelineruns.RunView, error) {
	return f.svc.Start(context.Background(), pipelineruns.StartInput{SessionID: f.sessionID, WorkflowID: "build-only", RequestedBy: requestedBy, Overrides: overrides})
}

func (f *fixture) submit(run pipelineruns.RunView, outcome, outputCommit, key string) (pipelineruns.SubmitResult, error) {
	att := run.Attempts[len(run.Attempts)-1]
	return f.svc.Submit(context.Background(), pipelineruns.SubmitInput{
		SessionID: f.sessionID, RunID: run.ID, AttemptID: att.ID, ControllerGeneration: att.ControllerGeneration,
		IdempotencyKey: key, Outcome: outcome, ExpectedInputCommit: att.InputCommit, OutputCommit: outputCommit, Summary: "did the thing",
	})
}

func code(t *testing.T, err error) string {
	t.Helper()
	var ae *apierr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("want apierr, got %v", err)
	}
	return ae.Code
}

func TestStartSnapshotsDefinitionAndInstructions(t *testing.T) {
	f := newFixture(t, map[string]string{".ao/pipelines/workflows/build-only.yaml": buildOnlyWorkflow})
	run, err := f.start("user", nil)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != "running" || run.CurrentStageID != "build" || run.ExpectedBranch != "task-branch" || run.RepairBudget != 3 {
		t.Fatalf("run: %+v", run)
	}
	if len(run.Attempts) != 1 || run.Attempts[0].InputCommit == "" || run.Attempts[0].ControllerGeneration != "gen-1" || run.Attempts[0].InstructionDelivery != "delivered" {
		t.Fatalf("attempt: %+v", run.Attempts)
	}
	if run.Stages[0].Harness != "claude-code" || run.Stages[0].Model != "opus" || run.Stages[0].SettingsSource != pipelineruns.SourceWorker {
		t.Fatalf("stage settings: %+v", run.Stages[0])
	}
	if len(f.messenger.sent) != 1 || !strings.Contains(f.messenger.sent[0], "Follow the repository contribution rules.") || !strings.Contains(f.messenger.sent[0], "ao pipeline submit") {
		t.Fatalf("worker instruction: %v", f.messenger.sent)
	}

	// Repository edits after start never change the active run's snapshot.
	writeFile(t, f.repo, ".ao/pipelines/workflows/build-only.yaml", strings.Replace(buildOnlyWorkflow, "Follow the repository contribution rules.", "CHANGED", 1))
	stored, ok, err := f.store.GetPipelineRun(context.Background(), run.ID)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if !strings.Contains(stored.Snapshot, "Follow the repository contribution rules.") || strings.Contains(stored.Snapshot, "CHANGED") {
		t.Fatalf("snapshot must be frozen: %s", stored.Snapshot)
	}
	if len(stored.SnapshotSHA256) != 64 || stored.SnapshotSHA256 != run.SnapshotSHA256 {
		t.Fatalf("snapshot provenance hash: %q", stored.SnapshotSHA256)
	}
}

func TestStartRejectsUnsupportedAndUnsafeRequests(t *testing.T) {
	f := newFixture(t, map[string]string{
		".ao/pipelines/workflows/build-only.yaml":   buildOnlyWorkflow,
		".ao/pipelines/workflows/build-review.yaml": reviewWorkflow,
		".ao/pipelines/profiles/tester.yaml":        testerProfile,
	})
	ctx := context.Background()
	if _, err := f.svc.Start(ctx, pipelineruns.StartInput{SessionID: f.sessionID, RequestedBy: "user"}); code(t, err) != "PIPELINE_WORKFLOW_REQUIRED" {
		t.Fatalf("missing workflow must not fall back to a normal worker: %v", err)
	}
	if _, err := f.svc.Start(ctx, pipelineruns.StartInput{SessionID: f.sessionID, WorkflowID: "build-review", RequestedBy: "user"}); code(t, err) != "PIPELINE_WORKFLOW_UNAVAILABLE" {
		t.Fatalf("a workflow with a review stage must be rejected until it can run: %v", err)
	}
	if _, err := f.svc.Start(ctx, pipelineruns.StartInput{SessionID: f.sessionID, WorkflowID: "ghost", RequestedBy: "user"}); code(t, err) != "PIPELINE_WORKFLOW_NOT_FOUND" {
		t.Fatalf("unknown workflow: %v", err)
	}
	// Orchestrators cannot supply overrides; users cannot override the regular worker.
	if _, err := f.start("orchestrator", map[string]pipelineruns.StageOverride{"build": {Model: "x"}}); code(t, err) != "PIPELINE_OVERRIDE_USER_ONLY" {
		t.Fatalf("orchestrator override: %v", err)
	}
	if _, err := f.start("", map[string]pipelineruns.StageOverride{"build": {Model: "x"}}); code(t, err) != "PIPELINE_OVERRIDE_USER_ONLY" {
		t.Fatalf("an unspecified requester is treated as an orchestrator: %v", err)
	}
	if _, err := f.start("user", map[string]pipelineruns.StageOverride{"build": {Model: "x"}}); code(t, err) != "INVALID_PIPELINE_OVERRIDE" {
		t.Fatalf("build override: %v", err)
	}
	if _, err := f.svc.Start(ctx, pipelineruns.StartInput{SessionID: "ghost", WorkflowID: "build-only", RequestedBy: "user"}); code(t, err) != "SESSION_NOT_FOUND" {
		t.Fatalf("unknown session: %v", err)
	}
	if has, _ := f.store.HasUnfinishedPipelineRun(ctx, f.sessionID); has {
		t.Fatal("rejected starts must not create runs")
	}
}

func TestStartRequiresRunnableChatWorker(t *testing.T) {
	f := newFixture(t, map[string]string{".ao/pipelines/workflows/build-only.yaml": buildOnlyWorkflow})
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	create := func(mutate func(*domain.SessionRecord)) domain.SessionID {
		rec := domain.SessionRecord{
			ProjectID: "proj", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat,
			Activity: domain.Activity{State: domain.ActivityActive, LastActivityAt: now},
			Metadata: domain.SessionMetadata{WorkspacePath: f.repo, ControllerGeneration: "gen-1"}, CreatedAt: now, UpdatedAt: now,
		}
		mutate(&rec)
		created, err := f.store.CreateSession(ctx, rec)
		if err != nil {
			t.Fatal(err)
		}
		return created.ID
	}
	for name, mutate := range map[string]func(*domain.SessionRecord){
		"terminal session":  func(r *domain.SessionRecord) { r.Mode = domain.SessionModeTUI },
		"orchestrator":      func(r *domain.SessionRecord) { r.Kind = domain.KindOrchestrator },
		"no controller yet": func(r *domain.SessionRecord) { r.Metadata.ControllerGeneration = "" },
		"no workspace":      func(r *domain.SessionRecord) { r.Metadata.WorkspacePath = "" },
	} {
		id := create(mutate)
		_, err := f.svc.Start(ctx, pipelineruns.StartInput{SessionID: id, WorkflowID: "build-only", RequestedBy: "user"})
		if code(t, err) != "PIPELINE_SESSION_UNSUPPORTED" {
			t.Fatalf("%s must be rejected visibly, got %v", name, err)
		}
	}
}

func TestStartRejectsSecondUnfinishedRun(t *testing.T) {
	f := newFixture(t, map[string]string{".ao/pipelines/workflows/build-only.yaml": buildOnlyWorkflow})
	if _, err := f.start("user", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.start("user", nil); code(t, err) != "PIPELINE_RUN_ACTIVE" {
		t.Fatalf("second run: %v", err)
	}
}

func TestSubmitAcceptsCleanCommittedCheckpointAndCompletes(t *testing.T) {
	f := newFixture(t, map[string]string{".ao/pipelines/workflows/build-only.yaml": buildOnlyWorkflow})
	run, err := f.start("user", nil)
	if err != nil {
		t.Fatal(err)
	}
	input := run.Attempts[0].InputCommit
	writeFile(t, f.repo, "feature.txt", "work\n")
	git(t, f.repo, "add", "-A")
	git(t, f.repo, "commit", "-q", "-m", "feature")
	head := git(t, f.repo, "rev-parse", "HEAD")

	res, err := f.submit(run, "succeeded", head, "key-1")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Accepted || res.Replayed || res.Run.State != "completed" || res.Run.Checkpoint == nil || res.Run.Checkpoint.OutputCommit != head || res.Run.Checkpoint.InputCommit != input || res.Run.Checkpoint.NoChange {
		t.Fatalf("result: %+v", res)
	}
	if res.Run.Stages[0].State != "accepted" || res.Run.CompletedAt == nil {
		t.Fatalf("stage presentation: %+v", res.Run.Stages)
	}
	if has, _ := f.store.HasUnfinishedPipelineRun(context.Background(), f.sessionID); has {
		t.Fatal("a completed run no longer suppresses ordinary lifecycle shortcuts")
	}
}

func TestSubmitNoChangeRetainsInputCommit(t *testing.T) {
	f := newFixture(t, map[string]string{".ao/pipelines/workflows/build-only.yaml": buildOnlyWorkflow})
	run, _ := f.start("user", nil)
	head := git(t, f.repo, "rev-parse", "HEAD")
	res, err := f.submit(run, "succeeded", head, "key-1")
	if err != nil || !res.Accepted || !res.Run.Checkpoint.NoChange || res.Run.Checkpoint.OutputCommit != res.Run.Checkpoint.InputCommit {
		t.Fatalf("no-change stage: %+v err=%v", res, err)
	}
}

func TestSubmitDuplicateIsIdempotentAndConflictingResultIsRejected(t *testing.T) {
	f := newFixture(t, map[string]string{".ao/pipelines/workflows/build-only.yaml": buildOnlyWorkflow})
	run, _ := f.start("user", nil)
	head := git(t, f.repo, "rev-parse", "HEAD")
	if _, err := f.submit(run, "succeeded", head, "key-1"); err != nil {
		t.Fatal(err)
	}
	again, err := f.submit(run, "succeeded", head, "key-1")
	if err != nil || !again.Replayed || !again.Accepted {
		t.Fatalf("exact replay must be idempotent: %+v err=%v", again, err)
	}
	if _, err := f.submit(run, "succeeded", head, "key-2"); code(t, err) != "PIPELINE_ATTEMPT_STALE" {
		t.Fatalf("a different result for a finished attempt is stale: %v", err)
	}
}

func TestSubmitConcurrentDuplicatesApplyOnce(t *testing.T) {
	f := newFixture(t, map[string]string{".ao/pipelines/workflows/build-only.yaml": buildOnlyWorkflow})
	run, _ := f.start("user", nil)
	head := git(t, f.repo, "rev-parse", "HEAD")
	var wg sync.WaitGroup
	results := make([]pipelineruns.SubmitResult, 8)
	errs := make([]error, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = f.submit(run, "succeeded", head, "same-key")
		}()
	}
	wg.Wait()
	fresh := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("duplicate %d: %v", i, errs[i])
		}
		if !results[i].Replayed {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("exactly one submission may apply; got %d", fresh)
	}
	attempts, _ := f.store.ListPipelineStageAttempts(context.Background(), run.ID)
	if len(attempts) != 1 || attempts[0].State != domain.PipelineAttemptAccepted {
		t.Fatalf("attempts: %+v", attempts)
	}
}

func TestSubmitDirtyWorkspaceIsPreservedAndExplained(t *testing.T) {
	f := newFixture(t, map[string]string{".ao/pipelines/workflows/build-only.yaml": buildOnlyWorkflow})
	run, _ := f.start("user", nil)
	writeFile(t, f.repo, "wip.txt", "uncommitted\n")
	head := git(t, f.repo, "rev-parse", "HEAD")

	_, err := f.submit(run, "succeeded", head, "key-1")
	if code(t, err) != "PIPELINE_DIRTY_WORKSPACE" {
		t.Fatalf("dirty submission: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(f.repo, "wip.txt")); statErr != nil {
		t.Fatal("dirty work must be preserved, never reset")
	}
	env, _ := f.svc.Get(context.Background(), f.sessionID)
	if env.Run.State != "running" || env.Run.LastRejection == nil || env.Run.LastRejection.Code != "PIPELINE_DIRTY_WORKSPACE" || len(env.Run.LastRejection.Paths) != 1 || !strings.Contains(env.Run.LastRejection.Paths[0], "wip.txt") {
		t.Fatalf("rejection not explained: %+v", env.Run.LastRejection)
	}

	// After the worker commits, the same attempt can still complete.
	git(t, f.repo, "add", "-A")
	git(t, f.repo, "commit", "-q", "-m", "wip")
	head = git(t, f.repo, "rev-parse", "HEAD")
	if res, err := f.submit(run, "succeeded", head, "key-2"); err != nil || !res.Accepted {
		t.Fatalf("clean resubmission: %+v err=%v", res, err)
	}
}

func TestSubmitStaleRevisionsAreRejected(t *testing.T) {
	f := newFixture(t, map[string]string{".ao/pipelines/workflows/build-only.yaml": buildOnlyWorkflow})
	run, _ := f.start("user", nil)
	att := run.Attempts[0]
	ctx := context.Background()
	base := pipelineruns.SubmitInput{SessionID: f.sessionID, RunID: run.ID, AttemptID: att.ID, ControllerGeneration: att.ControllerGeneration, IdempotencyKey: "k", Outcome: "succeeded", ExpectedInputCommit: att.InputCommit, OutputCommit: att.InputCommit}

	wrongInput := base
	wrongInput.ExpectedInputCommit = strings.Repeat("a", 40)
	if _, err := f.svc.Submit(ctx, wrongInput); code(t, err) != "PIPELINE_STALE_REVISION" {
		t.Fatalf("wrong input revision: %v", err)
	}
	wrongOutput := base
	wrongOutput.OutputCommit = strings.Repeat("b", 40)
	if _, err := f.svc.Submit(ctx, wrongOutput); code(t, err) != "PIPELINE_STALE_REVISION" {
		t.Fatalf("output commit that is not HEAD: %v", err)
	}
	wrongGen := base
	wrongGen.ControllerGeneration = "gen-0"
	if _, err := f.svc.Submit(ctx, wrongGen); code(t, err) != "PIPELINE_STALE_GENERATION" {
		t.Fatalf("stale generation: %v", err)
	}
	wrongAttempt := base
	wrongAttempt.AttemptID = "pstg_nope"
	if _, err := f.svc.Submit(ctx, wrongAttempt); code(t, err) != "PIPELINE_ATTEMPT_NOT_FOUND" {
		t.Fatalf("unknown attempt: %v", err)
	}
	wrongRun := base
	wrongRun.RunID = "prun_nope"
	if _, err := f.svc.Submit(ctx, wrongRun); code(t, err) != "PIPELINE_RUN_NOT_FOUND" {
		t.Fatalf("unknown run: %v", err)
	}
	if got, _ := f.svc.Get(ctx, f.sessionID); got.Run.State != "running" {
		t.Fatalf("rejections must not change run state: %+v", got.Run)
	}
}

func TestSubmitRejectsBranchChangeAndRewrittenHistory(t *testing.T) {
	f := newFixture(t, map[string]string{".ao/pipelines/workflows/build-only.yaml": buildOnlyWorkflow})
	run, _ := f.start("user", nil)

	git(t, f.repo, "checkout", "-q", "-b", "other")
	head := git(t, f.repo, "rev-parse", "HEAD")
	if _, err := f.submit(run, "succeeded", head, "k1"); code(t, err) != "PIPELINE_BRANCH_CHANGED" {
		t.Fatalf("branch change: %v", err)
	}
	git(t, f.repo, "checkout", "-q", "task-branch")

	// Rewrite history so the new HEAD no longer descends from the input commit.
	git(t, f.repo, "checkout", "-q", "--orphan", "orphan")
	git(t, f.repo, "commit", "-q", "--allow-empty", "-m", "unrelated")
	git(t, f.repo, "branch", "-q", "-f", "task-branch", "HEAD")
	git(t, f.repo, "checkout", "-q", "task-branch")
	head = git(t, f.repo, "rev-parse", "HEAD")
	if _, err := f.submit(run, "succeeded", head, "k2"); code(t, err) != "PIPELINE_NON_LINEAR_HANDOFF" {
		t.Fatalf("rewritten history: %v", err)
	}
}

func TestSubmitFailedOutcomePausesWithoutConsumingBudget(t *testing.T) {
	f := newFixture(t, map[string]string{".ao/pipelines/workflows/build-only.yaml": buildOnlyWorkflow})
	run, _ := f.start("user", nil)
	res, err := f.submit(run, "failed", "", "k1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted || res.Run.State != "paused" || res.Run.PauseReason != "stage_failed" || res.Run.RepairsUsed != 0 || res.Run.Stages[0].State != "failed" {
		t.Fatalf("failed outcome: %+v", res.Run)
	}
	if has, _ := f.store.HasUnfinishedPipelineRun(context.Background(), f.sessionID); !has {
		t.Fatal("a paused run still owns the lifecycle")
	}
}

func TestInterruptedExecutionPausesInsteadOfReplaying(t *testing.T) {
	f := newFixture(t, map[string]string{".ao/pipelines/workflows/build-only.yaml": buildOnlyWorkflow})
	run, _ := f.start("user", nil)
	ctx := context.Background()
	sess, _, _ := f.store.GetSession(ctx, f.sessionID)
	sess.Metadata.ControllerGeneration = "gen-2" // controller restarted
	if err := f.store.UpdateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}

	env, err := f.svc.Get(ctx, f.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if env.Run.State != "paused" || env.Run.PauseReason != "controller_changed" || env.Run.RepairsUsed != 0 || env.Run.Attempts[0].State != "interrupted" {
		t.Fatalf("interrupted run: %+v", env.Run)
	}
	head := git(t, f.repo, "rev-parse", "HEAD")
	if _, err := f.submit(run, "succeeded", head, "k1"); code(t, err) != "PIPELINE_ATTEMPT_STALE" {
		t.Fatalf("results for an interrupted attempt are stale: %v", err)
	}
	attempts, _ := f.store.ListPipelineStageAttempts(ctx, run.ID)
	if len(attempts) != 1 {
		t.Fatalf("interruption must not start another attempt: %+v", attempts)
	}
}

func TestReconcileAllPausesRunsOfTerminatedSessions(t *testing.T) {
	f := newFixture(t, map[string]string{".ao/pipelines/workflows/build-only.yaml": buildOnlyWorkflow})
	run, _ := f.start("user", nil)
	ctx := context.Background()
	sess, _, _ := f.store.GetSession(ctx, f.sessionID)
	sess.IsTerminated = true
	if err := f.store.UpdateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.ReconcileAll(ctx); err != nil {
		t.Fatal(err)
	}
	got, _, _ := f.store.GetPipelineRun(ctx, run.ID)
	if got.State != domain.PipelineRunPaused || got.PauseReason != domain.PipelinePauseSessionTerminated {
		t.Fatalf("run: %+v", got)
	}
}

func TestInstructionDeliveryFailureIsRecordedNotFatal(t *testing.T) {
	f := newFixture(t, map[string]string{".ao/pipelines/workflows/build-only.yaml": buildOnlyWorkflow})
	f.messenger.err = errors.New("controller offline")
	run, err := f.start("user", nil)
	if err != nil {
		t.Fatal(err)
	}
	if run.Attempts[0].InstructionDelivery != "failed" || run.State != "running" {
		t.Fatalf("run: %+v", run)
	}
	found := false
	for _, e := range run.Events {
		if e.Kind == "instruction_delivery_failed" && strings.Contains(e.Message, "controller offline") {
			found = true
		}
	}
	if !found {
		t.Fatalf("delivery failure must be a visible fact: %+v", run.Events)
	}
}

func TestSuppressesLifecycleShortcutsOnlyWhileUnfinished(t *testing.T) {
	f := newFixture(t, map[string]string{".ao/pipelines/workflows/build-only.yaml": buildOnlyWorkflow})
	ctx := context.Background()
	if f.svc.SuppressesLifecycleShortcuts(ctx, f.sessionID) {
		t.Fatal("a session without a run is an ordinary worker")
	}
	run, _ := f.start("user", nil)
	if !f.svc.SuppressesLifecycleShortcuts(ctx, f.sessionID) {
		t.Fatal("an unfinished run suppresses automatic review and merge cleanup")
	}
	head := git(t, f.repo, "rev-parse", "HEAD")
	if _, err := f.submit(run, "succeeded", head, "k1"); err != nil {
		t.Fatal(err)
	}
	if f.svc.SuppressesLifecycleShortcuts(ctx, f.sessionID) {
		t.Fatal("a completed run releases the lifecycle")
	}
}

func TestSubmitInputValidation(t *testing.T) {
	f := newFixture(t, map[string]string{".ao/pipelines/workflows/build-only.yaml": buildOnlyWorkflow})
	run, _ := f.start("user", nil)
	att := run.Attempts[0]
	ctx := context.Background()
	good := pipelineruns.SubmitInput{SessionID: f.sessionID, RunID: run.ID, AttemptID: att.ID, ControllerGeneration: att.ControllerGeneration, IdempotencyKey: "k", Outcome: "succeeded", ExpectedInputCommit: att.InputCommit, OutputCommit: att.InputCommit}
	for name, mutate := range map[string]func(*pipelineruns.SubmitInput){
		"no key":       func(i *pipelineruns.SubmitInput) { i.IdempotencyKey = "" },
		"bad outcome":  func(i *pipelineruns.SubmitInput) { i.Outcome = "done" },
		"no output":    func(i *pipelineruns.SubmitInput) { i.OutputCommit = "" },
		"bad commit":   func(i *pipelineruns.SubmitInput) { i.ExpectedInputCommit = "HEAD" },
		"long summary": func(i *pipelineruns.SubmitInput) { i.Summary = strings.Repeat("x", 2001) },
		"no run id":    func(i *pipelineruns.SubmitInput) { i.RunID = "" },
	} {
		in := good
		mutate(&in)
		if _, err := f.svc.Submit(ctx, in); code(t, err) != "INVALID_PIPELINE_RESULT" {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
