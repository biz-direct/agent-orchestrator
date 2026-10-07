package pipelineruns_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/pipelineruns"
)

func defectReport(desc string) *pipelineruns.StageReport {
	return &pipelineruns.StageReport{
		Findings: []pipelineruns.ReportFinding{{Criterion: "feature works", Status: "unmet", Evidence: "TestFeature fails"}},
		Commands: []pipelineruns.ReportCommand{{Command: "go test ./...", ExitCode: 1, Summary: "1 failure"}},
		Defects:  []pipelineruns.ReportDefect{{Description: desc, Paths: []string{"src/main.go"}}},
	}
}

// reportDefect makes the active Tester report a production defect at a fresh
// test commit and returns the result.
func (s *specialistStage) reportDefect(key, desc string) (pipelineruns.SubmitResult, string, error) {
	s.t.Helper()
	head := s.commit("failing test "+key, func() { writeFile(s.t, s.repo, "src/"+key+"_test.go", "package src\n") })
	res, err := s.submitSpec("production_defect", head, key, defectReport(desc))
	return res, head, err
}

// repairAsBuild makes the resumed original worker fix production code and
// submit; it returns the accepted result.
func (s *specialistStage) repairAsBuild(key string, run pipelineruns.RunView) pipelineruns.SubmitResult {
	s.t.Helper()
	build := run.Attempts[len(run.Attempts)-1]
	if build.StageID != "build" || build.State != "active" {
		s.t.Fatalf("the repair attempt must be active in Build: %+v", build)
	}
	head := s.commit("fix "+key, func() { writeFile(s.t, s.repo, "src/fix_"+key+".go", "package src\n") })
	res, err := s.svc.Submit(context.Background(), pipelineruns.SubmitInput{
		SessionID: s.sessionID, RunID: run.ID, AttemptID: build.ID, ControllerGeneration: build.ControllerGeneration,
		IdempotencyKey: "fix-" + key, Outcome: "succeeded", ExpectedInputCommit: build.InputCommit, OutputCommit: head, Summary: "fixed " + key,
	})
	if err != nil {
		s.t.Fatal(err)
	}
	return res
}

// nextTesterAttempt refreshes the fixture's view of the active Tester attempt.
func (s *specialistStage) nextTesterAttempt() {
	s.t.Helper()
	s.view = s.get()
	for _, a := range s.view.Attempts {
		if a.StageID == "test" && a.State == "active" {
			s.attempt = a
			return
		}
	}
	s.t.Fatalf("no active test attempt: %s", brief(s.view))
}

func (s *specialistStage) drive() {
	s.t.Helper()
	if err := s.svc.DriveHandoffs(context.Background()); err != nil {
		s.t.Fatal(err)
	}
}

func TestProductionDefectReturnsToTheOriginalBuilderThenRetestsInTheSameConversation(t *testing.T) {
	s := newSpecialistStage(t)
	res, defectHead, err := s.reportDefect("one", "Add overflows on large input")
	if err != nil {
		t.Fatal(err)
	}
	if res.Run.State != "running" || res.Run.CurrentStageID != "build" || res.Run.RepairsUsed != 1 || res.Run.RepairsRemaining != 2 {
		t.Fatalf("the defect must be routed back to Build and counted once: %s", brief(res.Run))
	}
	build := res.Run.Attempts[2]
	if build.State != "handoff" || build.StageID != "build" || build.RepairSourceAttemptID != res.Run.Attempts[1].ID || build.Feedback == nil || build.Feedback.Revision != defectHead || len(build.Feedback.Defects) != 1 {
		t.Fatalf("the repair attempt carries revision-bound feedback: %+v", build)
	}
	if len(res.Run.Repairs) != 1 || res.Run.Repairs[0].Kind != "production_defect" || res.Run.Repairs[0].Ordinal != 1 || res.Run.Repairs[0].ReturnStageID != "test" {
		t.Fatalf("repairs: %+v", res.Run.Repairs)
	}
	if s.admitted(s.sessionID) || s.admitted(s.spec) {
		t.Fatal("nobody executes until the handoff is proven")
	}

	s.drive()
	// The Tester is fenced first, then the ORIGINAL worker conversation resumes.
	last := s.exec.events[len(s.exec.events)-2:]
	if last[0] != "relinquish:"+string(s.spec) || last[1] != "resume:"+string(s.sessionID) {
		t.Fatalf("order: %v", s.exec.events)
	}
	call := s.exec.resumed[0]
	for _, want := range []string{"repair 1 of 3", "2 remaining", "Add overflows on large input", defectHead, "src/main.go", "ao pipeline submit"} {
		if !strings.Contains(call.Prompt, want) {
			t.Fatalf("repair prompt missing %q:\n%s", want, call.Prompt)
		}
	}
	view := s.get()
	if view.State != "running" || view.Attempts[2].State != "active" || view.Attempts[2].ExecutorSessionID != string(s.sessionID) {
		t.Fatalf("the original worker is the active Builder again: %s %+v", brief(view), view.Attempts[2])
	}
	if !s.admitted(s.sessionID) || s.admitted(s.spec) {
		t.Fatal("exclusive execution moved back to the worker")
	}

	// Build fixes and submits; the SAME Tester conversation resumes for a re-run.
	fixed := s.repairAsBuild("one", view)
	if !fixed.Accepted || fixed.Run.CurrentStageID != "test" || fixed.Run.Attempts[3].StageID != "test" || fixed.Run.Attempts[3].AttemptNo != 2 || fixed.Run.Attempts[3].State != "handoff" {
		t.Fatalf("after the repair the run returns to the stage that failed: %s", brief(fixed.Run))
	}
	s.drive()
	if len(s.exec.starts) != 1 {
		t.Fatalf("no fresh Tester conversation may be started: %d starts", len(s.exec.starts))
	}
	rerun := s.exec.resumed[1]
	if rerun.ID != s.spec || !strings.Contains(rerun.Prompt, "attempt 2") || !strings.Contains(rerun.Prompt, "does NOT cover") {
		t.Fatalf("re-test must resume the same conversation and say earlier results do not cover the new revision:\n%s", rerun.Prompt)
	}
	attached, _ := s.store.ListAttachedSessionIDs(context.Background(), s.sessionID)
	if len(attached) != 1 {
		t.Fatalf("conversations are preserved, not duplicated: %v", attached)
	}

	s.nextTesterAttempt()
	if s.attempt.ConversationSessionID != string(s.spec) {
		t.Fatalf("attempt 2 runs in the original Tester conversation: %+v", s.attempt)
	}
	// Tester passes against the repaired revision.
	pass := s.commit("tests pass", func() { writeFile(t, s.repo, "src/ok_test.go", "package src\n") })
	done, err := s.submitSpec("succeeded", pass, "pass", passReport())
	if err != nil || !done.Accepted || done.Run.State != "completed" || done.Run.RepairsUsed != 1 {
		t.Fatalf("final: %v %s", err, brief(done.Run))
	}

	// Evidence is revision-scoped: the defect result no longer covers the head.
	var current, stale int
	for _, ev := range done.Run.Evidence {
		if ev.Current {
			current++
			if ev.Outcome != "succeeded" || ev.Revision != pass {
				t.Fatalf("current evidence must be the passing result at the latest checkpoint: %+v", ev)
			}
		} else {
			stale++
			if ev.Revision != defectHead || ev.Outcome != "production_defect" {
				t.Fatalf("stale evidence is retained with its own revision: %+v", ev)
			}
		}
	}
	if current != 1 || stale != 1 {
		t.Fatalf("evidence: %+v", done.Run.Evidence)
	}
}

func TestValidationFailureRoutesToBuildAndPassesOnTheRerun(t *testing.T) {
	v := newValidatedStage(t, "  - {id: unit, command: \"test -f fixed.marker\"}\n", true)
	v.submitPass()
	view := v.validate()
	if view.State != "running" || view.CurrentStageID != "build" || view.RepairsUsed != 1 || view.Repairs[0].Kind != "validation_failed" {
		t.Fatalf("a genuine failed check is a repair, not a pause: %s", brief(view))
	}
	fb := view.Attempts[len(view.Attempts)-1].Feedback
	if fb == nil || len(fb.FailedChecks) != 1 || fb.FailedChecks[0].CommandID != "unit" || fb.FailedChecks[0].ExitCode != 1 {
		t.Fatalf("feedback must carry the failed check as evidence: %+v", fb)
	}
	v.drive()
	if p := v.exec.resumed[0].Prompt; !strings.Contains(p, `Failed check "unit"`) || !strings.Contains(p, "test -f fixed.marker") {
		t.Fatalf("the Builder is told which check failed and on what revision:\n%s", p)
	}
	view = v.get()
	build := view.Attempts[len(view.Attempts)-1]
	head := v.commit("fix", func() { writeFile(t, v.repo, "fixed.marker", "ok\n") })
	if _, err := v.svc.Submit(context.Background(), pipelineruns.SubmitInput{
		SessionID: v.sessionID, RunID: view.ID, AttemptID: build.ID, ControllerGeneration: build.ControllerGeneration,
		IdempotencyKey: "fix", Outcome: "succeeded", ExpectedInputCommit: build.InputCommit, OutputCommit: head,
	}); err != nil {
		t.Fatal(err)
	}
	v.drive() // hand back to the Tester
	v.nextTesterAttempt()
	pass := v.commit("tests again", func() { writeFile(t, v.repo, "src/again_test.go", "package src\n") })
	if _, err := v.submitSpec("succeeded", pass, "again", passReport()); err != nil {
		t.Fatal(err)
	}
	final := v.validate() // AO re-runs the checks against the repaired revision
	if final.State != "completed" {
		t.Fatalf("after the repair the independent checks pass: %s", brief(final))
	}
	last := final.Attempts[len(final.Attempts)-1]
	if got := commandsByID(last)["unit"]; got.Status != "passed" || got.Revision != pass {
		t.Fatalf("fresh evidence for the repaired revision: %+v", got)
	}
}

func TestRepairBudgetIsSharedAndPausesBeforeAFourthReturn(t *testing.T) {
	s := newSpecialistStage(t)
	for i := 1; i <= 3; i++ {
		res, _, err := s.reportDefect(fmt.Sprintf("d%d", i), fmt.Sprintf("defect %d", i))
		if err != nil {
			t.Fatalf("defect %d: %v", i, err)
		}
		if res.Run.RepairsUsed != i || res.Run.RepairsRemaining != 3-i || res.Run.State != "running" {
			t.Fatalf("repair %d: %s", i, brief(res.Run))
		}
		s.drive() // resume the Builder
		s.repairAsBuild(fmt.Sprintf("r%d", i), s.get())
		s.drive() // resume the Tester
		s.nextTesterAttempt()
	}
	res, _, err := s.reportDefect("d4", "defect 4")
	if err != nil {
		t.Fatal(err)
	}
	if res.Run.State != "paused" || res.Run.PauseReason != "repair_budget_exhausted" || res.Run.RepairsUsed != 3 || res.Run.RepairsRemaining != 0 {
		t.Fatalf("the fourth return must not happen: %s", brief(res.Run))
	}
	if len(res.Run.Repairs) != 3 {
		t.Fatalf("exactly three counted repairs: %+v", res.Run.Repairs)
	}
	if !strings.Contains(res.Run.PauseDetail, "defect 4") || !strings.Contains(res.Run.PauseDetail, "Nothing was reset") {
		t.Fatalf("pause detail: %q", res.Run.PauseDetail)
	}
	for _, a := range res.Run.Attempts {
		if a.StageID == "build" && a.State == "handoff" {
			t.Fatal("no fourth Build attempt may exist")
		}
	}
	// A paused, exhausted run does nothing on its own.
	before := len(s.exec.resumed)
	s.drive()
	if len(s.exec.resumed) != before || s.get().RepairsUsed != 3 {
		t.Fatal("exhaustion is never silently extended")
	}
}

func TestDuplicateAndConcurrentFeedbackCountOneRepair(t *testing.T) {
	s := newSpecialistStage(t)
	head := s.commit("failing test", func() { writeFile(t, s.repo, "src/dup_test.go", "package src\n") })
	var wg sync.WaitGroup
	results := make([]pipelineruns.SubmitResult, 8)
	errs := make([]error, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = s.submitSpec("production_defect", head, "same-key", defectReport("overflow"))
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("duplicate %d: %v", i, err)
		}
	}
	view := s.get()
	if view.RepairsUsed != 1 || len(view.Repairs) != 1 {
		t.Fatalf("duplicate delivery must not spend the budget twice: %s repairs=%d", brief(view), len(view.Repairs))
	}
	builds := 0
	for _, a := range view.Attempts {
		if a.StageID == "build" {
			builds++
		}
	}
	if builds != 2 { // the original Build plus exactly one repair attempt
		t.Fatalf("builds: %d", builds)
	}
	// Racing drivers resume the Builder once.
	var drivers sync.WaitGroup
	for i := 0; i < 6; i++ {
		drivers.Add(1)
		go func() {
			defer drivers.Done()
			_ = s.svc.DriveHandoffs(context.Background())
		}()
	}
	drivers.Wait()
	if len(s.exec.resumed) != 1 {
		t.Fatalf("concurrent transitions must collapse into one resume: %d", len(s.exec.resumed))
	}
}

func TestRepairAccountingSurvivesRestart(t *testing.T) {
	s := newSpecialistStage(t)
	for i := 1; i <= 2; i++ {
		if _, _, err := s.reportDefect(fmt.Sprintf("d%d", i), "defect"); err != nil {
			t.Fatal(err)
		}
		s.drive()
		s.repairAsBuild(fmt.Sprintf("r%d", i), s.get())
		s.drive()
		s.nextTesterAttempt()
	}
	// A new service over the same durable store (a restart) sees the same budget.
	restarted := pipelineruns.New(pipelineruns.Deps{Store: s.store, Messenger: s.messenger, Executor: s.exec})
	env, err := restarted.Get(context.Background(), s.sessionID)
	if err != nil || env.Run.RepairsUsed != 2 || env.Run.RepairsRemaining != 1 || len(env.Run.Repairs) != 2 {
		t.Fatalf("restored accounting: %v %+v", err, env.Run)
	}
	s.svc = restarted
	if res, _, err := s.reportDefect("d3", "defect"); err != nil || res.Run.RepairsUsed != 3 {
		t.Fatalf("the third repair uses the restored budget: %v %+v", err, res.Run)
	}
}

func TestRepairIsNotUsedForOperationalFailuresPolicyViolationsOrUnroutedStages(t *testing.T) {
	// A stage without a repair route pauses; it never silently loops.
	f := newFixture(t, map[string]string{
		".ao/pipelines/workflows/build-test.yaml": strings.Replace(buildTestWorkflow, ", repairTo: build", "", 1),
		".ao/pipelines/profiles/tester.yaml":      scopedTesterProfile,
		"src/main.go":                             "package src\n",
	})
	exec := &fakeExecutor{store: f.store, repo: f.repo}
	f.svc = pipelineruns.New(pipelineruns.Deps{Store: f.store, Messenger: f.messenger, Executor: exec})
	s := &staged{fixture: f, exec: exec, gate: pipelineruns.NewStoreGate(f.store, nil)}
	run := s.startBuildTest()
	s.buildAndSubmit(run)
	if err := s.svc.DriveHandoffs(context.Background()); err != nil {
		t.Fatal(err)
	}
	view := s.get()
	att := view.Attempts[1]
	spec := &specialistStage{staged: s, spec: domain.SessionID(att.ConversationSessionID), view: view, attempt: att}
	res, _, err := spec.reportDefect("x", "a defect")
	if err != nil {
		t.Fatal(err)
	}
	if res.Run.State != "paused" || res.Run.PauseReason != "production_defect" || res.Run.RepairsUsed != 0 || len(res.Run.Repairs) != 0 {
		t.Fatalf("without a repairTo the stage pauses for a person: %s", brief(res.Run))
	}

	// A policy violation (scope) is a rejection, not a repair.
	s2 := newSpecialistStage(t)
	head := s2.commit("prod edit", func() { writeFile(t, s2.repo, "src/main.go", "package src\n// x\n") })
	if _, err := s2.submitSpec("production_defect", head, "k", defectReport("d")); code(t, err) != "PIPELINE_SCOPE_VIOLATION" {
		t.Fatalf("scope: %v", err)
	}
	if v := s2.get(); v.RepairsUsed != 0 || len(v.Repairs) != 0 {
		t.Fatalf("policy violations never consume the budget: %s", brief(v))
	}
}

func TestUnsafeResumeAsksForARecoveryDecisionInsteadOfStartingFresh(t *testing.T) {
	s := newSpecialistStage(t)
	if _, _, err := s.reportDefect("x", "a defect"); err != nil {
		t.Fatal(err)
	}
	s.exec.resumeErr = fmt.Errorf("%w: its controller is not running", ports.ErrPipelineResumeUnsafe)
	startsBefore := len(s.exec.starts)
	s.drive()
	view := s.get()
	if view.State != "paused" || view.PauseReason != "recovery_decision_required" || !strings.Contains(view.PauseDetail, "will not start a fresh conversation") {
		t.Fatalf("run: %s", brief(view))
	}
	if len(s.exec.starts) != startsBefore {
		t.Fatal("a fresh conversation must never replace the original one")
	}
	if view.RepairsUsed != 1 || view.Attempts[2].State != "handoff" {
		t.Fatalf("the counted repair and its attempt are retained: %s", brief(view))
	}
}

func TestRepairWorkOnlyWakesTheRightExecutorAtTheRightTime(t *testing.T) {
	s := newSpecialistStage(t)
	if _, _, err := s.reportDefect("x", "a defect"); err != nil {
		t.Fatal(err)
	}
	// During the handoff nobody is admitted, so a nudge cannot wake the worker early.
	if ok, _ := s.gate.AdmitSessionExecution(context.Background(), s.sessionID); ok {
		t.Fatal("the worker must not be woken before the Tester is proven stopped")
	}
	s.drive()
	if ok, _ := s.gate.AdmitSessionExecution(context.Background(), s.spec); ok {
		t.Fatal("the Tester may not act while Build repairs")
	}
}
