package pipelineruns_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/pipelineruns"
)

func (s *staged) control(run pipelineruns.RunView, action, by string, mutate ...func(*pipelineruns.ControlInput)) (pipelineruns.ControlResult, error) {
	s.t.Helper()
	in := pipelineruns.ControlInput{SessionID: s.sessionID, RunID: run.ID, Action: action, RequestedBy: by}
	for _, m := range mutate {
		m(&in)
	}
	return s.svc.Control(context.Background(), in)
}

func (s *staged) mustControl(run pipelineruns.RunView, action, by string, mutate ...func(*pipelineruns.ControlInput)) pipelineruns.ControlResult {
	s.t.Helper()
	res, err := s.control(run, action, by, mutate...)
	if err != nil {
		s.t.Fatalf("%s: %v", action, err)
	}
	return res
}

func TestPauseStopsTheActiveStageFencesHandoffsAndIsIdempotent(t *testing.T) {
	s := newStaged(t)
	run := s.startBuildTest()
	writeFile(t, s.repo, "wip.txt", "unfinished work\n") // the worker's own uncommitted work

	res := s.mustControl(run, "pause", "user", func(in *pipelineruns.ControlInput) { in.Reason = "lunch" })
	if !res.Changed || res.Run.State != "paused" || res.Run.PauseReason != "paused_by_user" || !strings.Contains(res.Run.PauseDetail, "lunch") {
		t.Fatalf("pause: %+v", res.Run)
	}
	if res.Stop == nil || !res.Stop.Requested || !res.Stop.Confirmed {
		t.Fatalf("a proven stop is reported: %+v", res.Stop)
	}
	// The executor was interrupted, then reopened so a person can talk to it.
	if len(s.exec.interrupted) != 1 || s.exec.interrupted[0] != s.sessionID || len(s.exec.released) != 1 {
		t.Fatalf("interrupt=%v release=%v", s.exec.interrupted, s.exec.released)
	}
	if att := res.Run.Attempts[0]; att.State != "interrupted" {
		t.Fatalf("the active attempt is interrupted, not lost: %+v", att)
	}
	if res.Run.RepairsUsed != 0 || !res.Run.Control.CanResume || res.Run.Control.CanPause || res.Run.Control.LastStop == nil || !res.Run.Control.LastStop.Confirmed {
		t.Fatalf("control state: %+v", res.Run.Control)
	}
	if _, err := os.Stat(filepath.Join(s.repo, "wip.txt")); err != nil {
		t.Fatalf("a pause must preserve the worktree: %v", err)
	}
	// A late submission from the interrupted executor cannot advance a paused run.
	att := res.Run.Attempts[0]
	if _, err := s.svc.Submit(context.Background(), pipelineruns.SubmitInput{
		SessionID: s.sessionID, RunID: run.ID, AttemptID: att.ID, ControllerGeneration: att.ControllerGeneration,
		IdempotencyKey: "late", Outcome: "succeeded", ExpectedInputCommit: att.InputCommit, OutputCommit: att.InputCommit,
	}); err == nil {
		t.Fatal("a paused run must not accept results")
	}

	again := s.mustControl(run, "pause", "user")
	if again.Changed || len(s.exec.interrupted) != 1 {
		t.Fatalf("a repeated pause is a no-op: changed=%v interrupts=%d", again.Changed, len(s.exec.interrupted))
	}
}

func TestPauseNeverTreatsARequestedInterruptAsProof(t *testing.T) {
	s := newStaged(t)
	run := s.startBuildTest()
	s.exec.interruptErr = fmt.Errorf("%w: the agent is waiting on a permission or input request from the user", ports.ErrPipelineExecutionUncertain)

	res := s.mustControl(run, "pause", "user")
	if res.Run.State != "paused" {
		t.Fatalf("the pause itself is persisted even when the stop cannot be proven: %+v", res.Run)
	}
	if res.Stop == nil || !res.Stop.Requested || res.Stop.Confirmed || !strings.Contains(res.Stop.Detail, "permission") {
		t.Fatalf("an unprovable stop must be reported as unconfirmed: %+v", res.Stop)
	}
	if res.Run.Control.LastStop == nil || res.Run.Control.LastStop.Confirmed {
		t.Fatalf("the unconfirmed stop stays visible: %+v", res.Run.Control.LastStop)
	}
	// AO never answers the user's pending prompt: no turn was delivered.
	if len(s.messenger.sent) != 1 { // only the original stage instruction
		t.Fatalf("pause must not message the worker: %v", s.messenger.sent)
	}
}

func TestPauseDuringAHandoffKeepsItPendingAndResumeRetriesIt(t *testing.T) {
	s := newStaged(t)
	run := s.startBuildTest()
	s.buildAndSubmit(run)
	view := s.get()
	if view.Attempts[1].State != "handoff" {
		t.Fatalf("setup: %s", brief(view))
	}
	paused := s.mustControl(view, "pause", "orchestrator")
	if paused.Run.PauseReason != "paused_by_orchestrator" || paused.Run.Attempts[1].State != "handoff" {
		t.Fatalf("the pending handoff is retained: %+v", paused.Run)
	}
	if paused.Stop == nil || !paused.Stop.Confirmed {
		t.Fatalf("nothing was executing: %+v", paused.Stop)
	}
	if err := s.svc.DriveHandoffs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.exec.starts) != 0 {
		t.Fatal("a paused run must not start the next stage")
	}

	// An orchestrator may resume an operational pause.
	resumed := s.mustControl(view, "resume", "orchestrator")
	if !resumed.Changed || resumed.Run.State != "running" {
		t.Fatalf("resume: %+v", resumed.Run)
	}
	if err := s.svc.DriveHandoffs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.exec.starts) != 1 || s.get().Attempts[1].State != "active" {
		t.Fatalf("the handoff is retried after resume: starts=%d %s", len(s.exec.starts), brief(s.get()))
	}
	if again := s.mustControl(view, "resume", "user"); again.Changed {
		t.Fatal("resuming a running run is a no-op")
	}
}

func TestResumeContinuesTheInterruptedBuildWithoutReplayingItAndKeepsDirtyWork(t *testing.T) {
	s := newStaged(t)
	run := s.startBuildTest()
	writeFile(t, s.repo, "wip.txt", "half done\n")
	s.mustControl(run, "pause", "user")
	resumedBefore := len(s.exec.resumed)

	res := s.mustControl(run, "resume", "user")
	if res.Run.State != "running" || len(res.Run.Attempts) != 2 {
		t.Fatalf("resume creates a continuation attempt: %s attempts=%d", brief(res.Run), len(res.Run.Attempts))
	}
	retry := res.Run.Attempts[1]
	if retry.State != "handoff" || retry.StageID != "build" || retry.AttemptNo != 2 || retry.InputCommit != res.Run.Attempts[0].InputCommit {
		t.Fatalf("retry attempt keeps the stage's input revision: %+v", retry)
	}
	if err := s.svc.DriveHandoffs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.exec.resumed) != resumedBefore+1 || len(s.exec.starts) != 0 {
		t.Fatalf("the original conversation is resumed, never restarted: resumed=%d starts=%d", len(s.exec.resumed)-resumedBefore, len(s.exec.starts))
	}
	prompt := s.exec.resumed[len(s.exec.resumed)-1].Prompt
	for _, want := range []string{"Your earlier turn is not replayed", "git status", "ao pipeline submit", "attempt 2"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("resume prompt missing %q:\n%s", want, prompt)
		}
	}
	active := s.get().Attempts[1]
	if active.State != "active" || active.ExecutorSessionID != string(s.sessionID) {
		t.Fatalf("the worker is the active Build executor again: %+v", active)
	}
	if _, err := os.Stat(filepath.Join(s.repo, "wip.txt")); err != nil {
		t.Fatalf("uncommitted work must survive pause and resume: %v", err)
	}
	// The continuation can finish the stage and the pipeline moves on.
	head := s.commitWork("feature.txt")
	if _, err := s.svc.Submit(context.Background(), pipelineruns.SubmitInput{
		SessionID: s.sessionID, RunID: run.ID, AttemptID: active.ID, ControllerGeneration: active.ControllerGeneration,
		IdempotencyKey: "after-resume", Outcome: "succeeded", ExpectedInputCommit: active.InputCommit, OutputCommit: head, Summary: "done",
	}); err != nil {
		t.Fatal(err)
	}
	if v := s.get(); v.CurrentStageID != "test" || v.Attempts[2].State != "handoff" {
		t.Fatalf("after the resumed Build is accepted the run continues: %s", brief(v))
	}
}

func (s *staged) commitWork(name string) string {
	s.t.Helper()
	writeFile(s.t, s.repo, name, "work\n")
	git(s.t, s.repo, "add", "-A")
	git(s.t, s.repo, "commit", "-q", "-m", "work "+name)
	return git(s.t, s.repo, "rev-parse", "HEAD")
}

func TestResumeRefusesToRestartAConversationWhoseControllerIsGone(t *testing.T) {
	s := newStaged(t)
	run := s.startBuildTest()
	s.mustControl(run, "pause", "user")
	s.mustControl(run, "resume", "user")
	s.exec.resumeErr = fmt.Errorf("%w: its controller is not running", ports.ErrPipelineResumeUnsafe)
	if err := s.svc.DriveHandoffs(context.Background()); err != nil {
		t.Fatal(err)
	}
	v := s.get()
	if v.State != "paused" || v.PauseReason != "recovery_decision_required" {
		t.Fatalf("a dead controller asks for a decision instead of being replayed: %s", brief(v))
	}
	if _, err := s.control(v, "resume", "orchestrator"); err == nil || code(t, err) != "PIPELINE_HUMAN_DECISION_REQUIRED" {
		t.Fatalf("an orchestrator cannot bypass a human recovery decision: %v", err)
	}
	if len(s.exec.starts) != 0 {
		t.Fatal("no fresh conversation may be started")
	}
}

func TestResumeOfASpecialistReusesItsOwnConversationAndKeepsItsInput(t *testing.T) {
	s := newStaged(t)
	run := s.startBuildTest()
	s.buildAndSubmit(run)
	if err := s.svc.DriveHandoffs(context.Background()); err != nil {
		t.Fatal(err)
	}
	view := s.get()
	spec := domain.SessionID(view.Attempts[1].ConversationSessionID)
	// The Tester commits some in-scope work, then the run is paused mid-stage.
	s.commitWork("partial_test.go")
	res := s.mustControl(view, "pause", "user")
	if len(s.exec.interrupted) != 1 || s.exec.interrupted[0] != spec || res.Run.Attempts[1].State != "interrupted" {
		t.Fatalf("the Tester is the one interrupted: %v %+v", s.exec.interrupted, res.Run.Attempts[1])
	}
	s.mustControl(view, "resume", "user")
	if err := s.svc.DriveHandoffs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.exec.starts) != 1 {
		t.Fatalf("no second Tester conversation: starts=%d", len(s.exec.starts))
	}
	last := s.exec.resumed[len(s.exec.resumed)-1]
	if last.ID != spec || !strings.Contains(last.Prompt, "not replayed") {
		t.Fatalf("the same Tester conversation resumes with a continuation note: %+v", last)
	}
	again := s.get().Attempts[2]
	if again.State != "active" || again.ConversationSessionID != string(spec) || again.InputCommit != view.Attempts[1].InputCommit {
		t.Fatalf("the continuation keeps the stage's original input revision: %+v", again)
	}
}

func TestCancelKeepsEverythingAndHandsTheWorkerBack(t *testing.T) {
	s := newStaged(t)
	run := s.startBuildTest()
	s.buildAndSubmit(run)
	if err := s.svc.DriveHandoffs(context.Background()); err != nil {
		t.Fatal(err)
	}
	view := s.get()
	spec := domain.SessionID(view.Attempts[1].ConversationSessionID)
	writeFile(t, s.repo, "dirty.txt", "do not lose me\n")
	headBefore := git(t, s.repo, "rev-parse", "HEAD")

	res := s.mustControl(view, "cancel", "user")
	if !res.Changed || res.Run.State != "cancelled" || res.Run.CompletedAt == nil || res.Run.Attempts[1].State != "cancelled" {
		t.Fatalf("cancel: %+v", res.Run)
	}
	if len(s.exec.stopped) != 0 {
		t.Fatal("cancel must not stop or remove a specialist's conversation")
	}
	if _, ok, _ := s.store.GetSession(context.Background(), spec); !ok {
		t.Fatal("the specialist conversation is retained")
	}
	if git(t, s.repo, "rev-parse", "HEAD") != headBefore {
		t.Fatal("cancel must not reset the worktree")
	}
	if _, err := os.Stat(filepath.Join(s.repo, "dirty.txt")); err != nil {
		t.Fatalf("dirty work is preserved: %v", err)
	}
	owner, _, _ := s.store.GetSession(context.Background(), s.sessionID)
	if owner.IsTerminated {
		t.Fatal("the original worker stays the task owner")
	}
	if !s.admitted(s.sessionID) || s.admitted(spec) {
		t.Fatalf("after cancel the worker is admitted and the stage conversation is not: worker=%v spec=%v", s.admitted(s.sessionID), s.admitted(spec))
	}
	released := false
	for _, id := range s.exec.released {
		released = released || id == s.sessionID
	}
	if !released {
		t.Fatalf("the worker's fenced intake must be reopened: %v", s.exec.released)
	}
	if s.svc.SuppressesLifecycleShortcuts(context.Background(), s.sessionID) {
		t.Fatal("a cancelled run releases the worker's ordinary lifecycle")
	}

	// Repeats are idempotent; the cancelled run cannot be resumed or paused.
	if again := s.mustControl(view, "cancel", "user"); again.Changed {
		t.Fatal("a repeated cancel is a no-op")
	}
	for _, action := range []string{"resume", "pause"} {
		if _, err := s.control(view, action, "user"); err == nil || code(t, err) != "PIPELINE_RUN_FINISHED" {
			t.Fatalf("%s on a cancelled run: %v", action, err)
		}
	}
	// A new run can start afterwards.
	if _, err := s.svc.Start(context.Background(), pipelineruns.StartInput{SessionID: s.sessionID, WorkflowID: "build-test", RequestedBy: "user"}); err != nil {
		t.Fatalf("a cancelled run no longer blocks the task: %v", err)
	}
}

func TestCompletedRunHandsTheWorkerBackAndCannotBeCancelled(t *testing.T) {
	s := newStaged(t)
	run := s.startBuildTest()
	s.buildAndSubmit(run)
	if err := s.svc.DriveHandoffs(context.Background()); err != nil {
		t.Fatal(err)
	}
	view := s.get()
	spec := domain.SessionID(view.Attempts[1].ConversationSessionID)
	tests := s.commitWork("final_test.go")
	if _, err := s.svc.Submit(context.Background(), pipelineruns.SubmitInput{
		SessionID: spec, RunID: view.ID, AttemptID: view.Attempts[1].ID, ControllerGeneration: view.Attempts[1].ControllerGeneration,
		IdempotencyKey: "pass", Outcome: "succeeded", ExpectedInputCommit: view.Attempts[1].InputCommit, OutputCommit: tests, Report: passReport(),
	}); err != nil {
		t.Fatal(err)
	}
	done := s.get()
	if done.State != "completed" {
		t.Fatalf("setup: %s", brief(done))
	}
	released := false
	for _, id := range s.exec.released {
		released = released || id == s.sessionID
	}
	if !released {
		t.Fatalf("a finished run reopens the worker's intake: %v", s.exec.released)
	}
	if _, err := s.control(done, "cancel", "user"); err == nil || code(t, err) != "PIPELINE_RUN_FINISHED" {
		t.Fatalf("a completed run cannot be cancelled: %v", err)
	}
}

func TestControlRejectsStaleAndForeignRequests(t *testing.T) {
	s := newStaged(t)
	run := s.startBuildTest()
	if _, err := s.control(run, "pause", "user", func(in *pipelineruns.ControlInput) { in.RunID = "prun_other" }); err == nil || code(t, err) != "PIPELINE_RUN_NOT_FOUND" {
		t.Fatalf("unknown run: %v", err)
	}
	if _, err := s.control(run, "pause", "user", func(in *pipelineruns.ControlInput) { in.ExpectedRevision = run.Revision + 5 }); err == nil || code(t, err) != "PIPELINE_STALE_CONTROL" {
		t.Fatalf("stale revision: %v", err)
	}
	if s.get().State != "running" {
		t.Fatal("a stale request must change nothing")
	}
	if _, err := s.control(run, "explode", "user"); err == nil || code(t, err) != "INVALID_PIPELINE_CONTROL" {
		t.Fatalf("unknown action: %v", err)
	}
	if _, err := s.control(run, "pause", "robot"); err == nil || code(t, err) != "INVALID_PIPELINE_REQUESTER" {
		t.Fatalf("unknown requester: %v", err)
	}
	// An older run is stale once the task has a newer one.
	s.mustControl(run, "cancel", "user")
	next, err := s.svc.Start(context.Background(), pipelineruns.StartInput{SessionID: s.sessionID, WorkflowID: "build-test", RequestedBy: "user"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.control(run, "cancel", "user"); err == nil || code(t, err) != "PIPELINE_STALE_CONTROL" {
		t.Fatalf("a control for the previous run is stale: %v", err)
	}
	if s.get().ID != next.ID || s.get().State != "running" {
		t.Fatal("the current run is untouched")
	}
}

func TestNormalTasksAreUnaffectedByControls(t *testing.T) {
	s := newStaged(t)
	if _, err := s.svc.Control(context.Background(), pipelineruns.ControlInput{SessionID: s.sessionID, RunID: "prun_none", Action: "pause", RequestedBy: "user"}); err == nil || code(t, err) != "PIPELINE_RUN_NOT_FOUND" {
		t.Fatalf("a task with no run has nothing to control: %v", err)
	}
	if !s.admitted(s.sessionID) {
		t.Fatal("an ordinary worker is always admitted")
	}
}

func TestExhaustedBudgetNeedsADistinctHumanAuthorization(t *testing.T) {
	s := newSpecialistStage(t)
	for i := 1; i <= 3; i++ {
		if _, _, err := s.reportDefect(fmt.Sprintf("d%d", i), fmt.Sprintf("defect %d", i)); err != nil {
			t.Fatal(err)
		}
		s.drive()
		s.repairAsBuild(fmt.Sprintf("r%d", i), s.get())
		s.drive()
		s.nextTesterAttempt()
	}
	res, _, err := s.reportDefect("d4", "defect 4")
	if err != nil {
		t.Fatal(err)
	}
	exhausted := res.Run
	if exhausted.State != "paused" || exhausted.PauseReason != "repair_budget_exhausted" || !exhausted.Control.NeedsRepairAuthorization || exhausted.Control.CanResume || !exhausted.Control.ResumeNeedsUser {
		t.Fatalf("exhaustion: %s control=%+v", brief(exhausted), exhausted.Control)
	}
	failing := exhausted.Attempts[len(exhausted.Attempts)-1]
	if failing.State != "failed" {
		t.Fatalf("setup: %+v", failing)
	}

	// Ordinary resume never grants attempts.
	if _, err := s.control(exhausted, "resume", "user"); err == nil || code(t, err) != "PIPELINE_REPAIR_AUTHORIZATION_REQUIRED" {
		t.Fatalf("resume must not extend the budget: %v", err)
	}
	if _, err := s.control(exhausted, "resume", "orchestrator"); err == nil || code(t, err) != "PIPELINE_HUMAN_DECISION_REQUIRED" {
		t.Fatalf("an orchestrator cannot resume a human-only pause: %v", err)
	}
	if _, err := s.control(exhausted, "authorize_repairs", "orchestrator", func(in *pipelineruns.ControlInput) { in.AdditionalRepairs = 1; in.RequestKey = "k1" }); err == nil || code(t, err) != "PIPELINE_HUMAN_AUTHORIZATION_REQUIRED" {
		t.Fatalf("only a person may extend the budget: %v", err)
	}
	if _, err := s.control(exhausted, "authorize_repairs", "user", func(in *pipelineruns.ControlInput) { in.AdditionalRepairs = 0; in.RequestKey = "k1" }); err == nil || code(t, err) != "INVALID_PIPELINE_CONTROL" {
		t.Fatalf("an authorization needs a positive amount: %v", err)
	}
	if _, err := s.control(exhausted, "authorize_repairs", "user", func(in *pipelineruns.ControlInput) { in.AdditionalRepairs = 1 }); err == nil || code(t, err) != "INVALID_PIPELINE_CONTROL" {
		t.Fatalf("an authorization needs a request key: %v", err)
	}
	if v := s.get(); v.RepairBudget != 3 || v.RepairsUsed != 3 {
		t.Fatalf("nothing changed yet: %s budget=%d", brief(v), v.RepairBudget)
	}

	granted := s.mustControl(exhausted, "authorize_repairs", "user", func(in *pipelineruns.ControlInput) {
		in.AdditionalRepairs = 1
		in.RequestKey = "k1"
		in.Reason = "one more try"
	})
	if !granted.Changed || granted.Run.RepairBudget != 4 || granted.Run.RepairsUsed != 3 || granted.Run.State != "paused" || len(granted.Run.RepairGrants) != 1 || granted.Run.RepairGrants[0].AuthorizedBy != "user" || granted.Run.RepairGrants[0].Amount != 1 {
		t.Fatalf("grant: %+v", granted.Run)
	}
	if !granted.Run.Control.CanResume || granted.Run.Control.NeedsRepairAuthorization {
		t.Fatalf("after authorization the run can resume: %+v", granted.Run.Control)
	}
	// The same request key is not granted twice.
	if again := s.mustControl(exhausted, "authorize_repairs", "user", func(in *pipelineruns.ControlInput) { in.AdditionalRepairs = 1; in.RequestKey = "k1" }); again.Changed || again.Run.RepairBudget != 4 {
		t.Fatalf("a repeated authorization is a no-op: %+v", again.Run)
	}

	// Resume now applies the authorized repair: it carries the retained defect.
	resumed := s.mustControl(exhausted, "resume", "user")
	if resumed.Run.State != "running" || resumed.Run.RepairsUsed != 4 || resumed.Run.CurrentStageID != "build" || len(resumed.Run.Repairs) != 4 {
		t.Fatalf("resume applies exactly one authorized repair: %s", brief(resumed.Run))
	}
	build := resumed.Run.Attempts[len(resumed.Run.Attempts)-1]
	if build.StageID != "build" || build.State != "handoff" || build.Feedback == nil || !strings.Contains(fmt.Sprint(build.Feedback.Defects), "defect 4") {
		t.Fatalf("the authorized repair carries the original feedback: %+v", build)
	}
	s.drive()
	call := s.exec.resumed[len(s.exec.resumed)-1]
	if !strings.Contains(call.Prompt, "defect 4") || !strings.Contains(call.Prompt, "repair 4 of 4") {
		t.Fatalf("repair prompt:\n%s", call.Prompt)
	}
	// Exhausted again: a new authorization needs a new key.
	s.repairAsBuild("r4", s.get())
	s.drive()
	s.nextTesterAttempt()
	res5, _, err := s.reportDefect("d5", "defect 5")
	if err != nil {
		t.Fatal(err)
	}
	if res5.Run.State != "paused" || res5.Run.PauseReason != "repair_budget_exhausted" || res5.Run.RepairsUsed != 4 {
		t.Fatalf("the extended budget is also finite: %s", brief(res5.Run))
	}
	if _, err := s.control(res5.Run, "resume", "user"); err == nil || code(t, err) != "PIPELINE_REPAIR_AUTHORIZATION_REQUIRED" {
		t.Fatalf("a spent grant does not carry over: %v", err)
	}
}

func TestAuthorizationOnlyAppliesToAnExhaustedBudget(t *testing.T) {
	s := newStaged(t)
	run := s.startBuildTest()
	_, err := s.control(run, "authorize_repairs", "user", func(in *pipelineruns.ControlInput) { in.AdditionalRepairs = 2; in.RequestKey = "k" })
	if err == nil || code(t, err) != "PIPELINE_NO_AUTHORIZATION_NEEDED" {
		t.Fatalf("authorizing without need: %v", err)
	}
}

func TestResumeRevalidatesTheWorkspaceBeforeRestarting(t *testing.T) {
	t.Run("branch switched", func(t *testing.T) {
		s := newStaged(t)
		run := s.startBuildTest()
		s.mustControl(run, "pause", "user")
		git(t, s.repo, "checkout", "-q", "-b", "elsewhere")
		_, err := s.control(run, "resume", "user")
		if err == nil || code(t, err) != "PIPELINE_RESUME_BLOCKED" {
			t.Fatalf("a different branch must block resume: %v", err)
		}
		if s.get().State != "paused" {
			t.Fatal("a blocked resume leaves the run paused")
		}
		git(t, s.repo, "checkout", "-q", "task-branch")
		if _, err := s.control(run, "resume", "user"); err != nil {
			t.Fatalf("resume works once the prerequisite holds: %v", err)
		}
	})
	t.Run("rewritten history", func(t *testing.T) {
		s := newStaged(t)
		run := s.startBuildTest()
		s.commitWork("a.txt")
		s.mustControl(run, "pause", "user")
		git(t, s.repo, "checkout", "-q", "--orphan", "rewritten")
		git(t, s.repo, "commit", "-q", "--allow-empty", "-m", "rewritten")
		git(t, s.repo, "branch", "-M", "task-branch")
		_, err := s.control(run, "resume", "user")
		if err == nil || code(t, err) != "PIPELINE_RESUME_BLOCKED" || !strings.Contains(err.Error(), "descend") {
			t.Fatalf("rewritten history must block resume: %v", err)
		}
	})
	t.Run("task ended", func(t *testing.T) {
		s := newStaged(t)
		run := s.startBuildTest()
		s.mustControl(run, "pause", "user")
		sess, _, _ := s.store.GetSession(context.Background(), s.sessionID)
		sess.IsTerminated = true
		if err := s.store.UpdateSession(context.Background(), sess); err != nil {
			t.Fatal(err)
		}
		if _, err := s.control(run, "resume", "user"); err == nil || code(t, err) != "PIPELINE_SESSION_ENDED" {
			t.Fatalf("an ended task cannot resume: %v", err)
		}
		if _, err := s.control(run, "cancel", "user"); err != nil {
			t.Fatalf("an ended task can still be cancelled: %v", err)
		}
	})
}

func TestReviewStagePauseResumeKeepsTheCheckpoint(t *testing.T) {
	r := newReviewing(t, false)
	run, head := r.toReview()
	r.setPR(head)
	paused := r.mustControl(run, "pause", "user")
	if paused.Run.State != "paused" || paused.Stop == nil || !paused.Stop.Confirmed || len(r.exec.interrupted) != 0 {
		t.Fatalf("Review has no executor to interrupt: %+v stop=%+v", paused.Run, paused.Stop)
	}
	r.drive()
	if r.get().State != "paused" {
		t.Fatal("a paused review is not evaluated")
	}
	// Resume at the same checkpoint re-evaluates with a new Review attempt.
	resumed := r.mustControl(run, "resume", "user")
	if resumed.Run.State != "running" {
		t.Fatalf("%+v", resumed.Run)
	}
	r.drive()
	r.drive()
	v := r.get()
	last := v.Attempts[len(v.Attempts)-1]
	if last.StageID != "review" || last.State != "active" || last.AttemptNo != 2 || last.InputCommit != head {
		t.Fatalf("review continues as a new attempt at the same checkpoint: %+v", last)
	}
	// A moved head blocks resume instead of reviewing a different revision.
	r.mustControl(v, "pause", "user")
	r.commitFile("late.txt")
	if _, err := r.control(v, "resume", "user"); err == nil || code(t, err) != "PIPELINE_RESUME_BLOCKED" {
		t.Fatalf("a moved head must block the review resume: %v", err)
	}
}

func (r *reviewing) commitFile(name string) string {
	r.t.Helper()
	writeFile(r.t, r.repo, name, "x\n")
	git(r.t, r.repo, "add", "-A")
	git(r.t, r.repo, "commit", "-q", "-m", name)
	return git(r.t, r.repo, "rev-parse", "HEAD")
}

func TestPauseKillsAnInFlightValidationRound(t *testing.T) {
	skipOnWindows(t)
	marker := filepath.Join(t.TempDir(), "started")
	v := newValidatedStage(t, fmt.Sprintf("  - {id: unit, command: %q}\n", "touch "+marker+"; sleep 300"), true)
	v.submitPass()
	done := make(chan error, 1)
	go func() { done <- v.svc.DriveHandoffs(context.Background()) }()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the validation command never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	view := v.get()
	res := v.mustControl(view, "pause", "user")
	if res.Stop == nil || !res.Stop.Requested || !res.Stop.Confirmed || !strings.Contains(res.Stop.Detail, "killed") {
		t.Fatalf("the round is cancelled and its commands killed: %+v", res.Stop)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	after := v.get()
	if after.State != "paused" || after.PauseReason != "paused_by_user" || commandsByID(after.Attempts[1])["unit"].Status != "cancelled" {
		t.Fatalf("pause during validation: %s %+v", brief(after), commandsByID(after.Attempts[1])["unit"])
	}
	if after.RepairsUsed != 0 {
		t.Fatal("a pause never spends the repair budget")
	}
	// Resume hands the validating attempt back to the driver, which will run a
	// new round and keep the cancelled one as evidence.
	resumed := v.mustControl(after, "resume", "user")
	if resumed.Run.State != "running" || resumed.Run.Attempts[1].State != "validating" || len(commandsByID(resumed.Run.Attempts[1])) != 1 {
		t.Fatalf("resume keeps the validating attempt and its evidence: %+v", resumed.Run.Attempts[1])
	}
}
