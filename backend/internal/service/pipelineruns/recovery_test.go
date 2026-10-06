package pipelineruns_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/pipelineruns"
)

// restart models a new daemon process over the same durable store: nothing in
// memory (driver slots, waits, cancel functions) survives, only what was written.
func (s *staged) restart() *pipelineruns.Service {
	s.t.Helper()
	svc := pipelineruns.New(pipelineruns.Deps{Store: s.store, Messenger: s.messenger, Executor: s.exec})
	if err := svc.ReconcileAll(context.Background()); err != nil {
		s.t.Fatal(err)
	}
	s.svc = svc
	return svc
}

func (s *staged) attachedCount() int {
	s.t.Helper()
	ids, err := s.store.ListAttachedSessionIDs(context.Background(), s.sessionID)
	if err != nil {
		s.t.Fatal(err)
	}
	return len(ids)
}

func TestCrashAfterAcceptanceBeforeTheHandoffRunsResumesExactlyOnce(t *testing.T) {
	s := newStaged(t)
	run := s.startBuildTest()
	s.buildAndSubmit(run) // accepted; the successor is only recorded
	if got := s.get().Attempts[1].State; got != "handoff" {
		t.Fatalf("setup: %s", got)
	}
	// Two daemons come up at once over the same store; only one handoff happens.
	a, b := s.restart(), s.restart()
	var wg sync.WaitGroup
	for _, svc := range []*pipelineruns.Service{a, b, a, b} {
		wg.Add(1)
		go func(svc *pipelineruns.Service) {
			defer wg.Done()
			_ = svc.DriveHandoffs(context.Background())
		}(svc)
	}
	wg.Wait()
	_ = a.DriveHandoffs(context.Background())
	v := s.get()
	if v.Attempts[1].State != "active" || len(s.exec.starts) > 2 || s.attachedCount() != 1 {
		t.Fatalf("one specialist conversation: state=%s starts=%d attached=%d", v.Attempts[1].State, len(s.exec.starts), s.attachedCount())
	}
	if s.exec.turns != 1 {
		t.Fatalf("the specialist received its brief exactly once: %d turns", s.exec.turns)
	}
}

func TestCrashAfterTheSpecialistStartedBeforeItWasConfirmedAdoptsItWithoutADuplicate(t *testing.T) {
	s := newStaged(t)
	run := s.startBuildTest()
	s.buildAndSubmit(run)
	// The daemon dies right after the stage session exists and got its prompt,
	// before the attempt was confirmed active.
	ctx, crash := context.WithCancel(context.Background())
	s.exec.afterStart = crash
	err := s.svc.DriveHandoffs(ctx)
	if err == nil {
		t.Log("the interrupted pass reported no error; the state below is what matters")
	}
	s.exec.afterStart = nil
	mid := s.get()
	if mid.State != "running" || mid.Attempts[1].State != "handoff" || s.attachedCount() != 1 {
		t.Fatalf("an interrupted handoff must not pause or lose the session: %s attempts=%+v attached=%d", brief(mid), mid.Attempts[1], s.attachedCount())
	}

	svc := s.restart()
	if err := svc.DriveHandoffs(context.Background()); err != nil {
		t.Fatal(err)
	}
	v := s.get()
	if v.Attempts[1].State != "active" || s.attachedCount() != 1 || s.exec.adopted != 1 {
		t.Fatalf("the surviving session is adopted: state=%s attached=%d adopted=%d", v.Attempts[1].State, s.attachedCount(), s.exec.adopted)
	}
	if s.exec.turns != 1 {
		t.Fatalf("the prompt is redelivered under the same key, so it is still one turn: %d", s.exec.turns)
	}
	if v.Control.LastRecovery == nil || v.Control.LastRecovery.Outcome != "retrying" {
		t.Fatalf("the restart outcome is visible in the stage view: %+v", v.Control.LastRecovery)
	}
}

func TestShutdownDuringTheFenceDoesNotPauseTheRun(t *testing.T) {
	s := newStaged(t)
	run := s.startBuildTest()
	s.buildAndSubmit(run)
	ctx, crash := context.WithCancel(context.Background())
	s.exec.onRelinquish = crash
	s.exec.relinquishErr = context.Canceled
	_ = s.svc.DriveHandoffs(ctx)
	s.exec.onRelinquish, s.exec.relinquishErr = nil, nil
	if v := s.get(); v.State != "running" || v.Attempts[1].State != "handoff" {
		t.Fatalf("a shutdown is not evidence about the run: %s", brief(v))
	}
	svc := s.restart()
	if err := svc.DriveHandoffs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.get().Attempts[1].State != "active" {
		t.Fatalf("the next process finishes the handoff: %s", brief(s.get()))
	}
}

func TestCrashAfterAResumedBuilderReceivedItsFeedbackNeverDeliversItTwice(t *testing.T) {
	s := newSpecialistStage(t)
	if _, _, err := s.reportDefect("d1", "defect one"); err != nil {
		t.Fatal(err)
	}
	turnsBefore := s.exec.turns
	ctx, crash := context.WithCancel(context.Background())
	s.exec.afterResume = crash
	_ = s.svc.DriveHandoffs(ctx) // the feedback reached the worker; the confirmation did not commit
	s.exec.afterResume = nil
	if v := s.get(); v.Attempts[2].State != "handoff" {
		t.Fatalf("the repair attempt is still unconfirmed: %s", brief(v))
	}
	svc := s.restart()
	if err := svc.DriveHandoffs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := s.get(); v.Attempts[2].State != "active" || v.RepairsUsed != 1 {
		t.Fatalf("recovered: %s", brief(v))
	}
	if got := s.exec.turns - turnsBefore; got != 1 {
		t.Fatalf("the worker must see the feedback as exactly one turn, saw %d", got)
	}
	if len(s.exec.resumed) != 2 {
		t.Fatalf("the redelivery used the same key: resumes=%d", len(s.exec.resumed))
	}
}

func TestRestartKeepsTheFrozenDefinitionNotTheEditedRepository(t *testing.T) {
	s := newStaged(t)
	run := s.startBuildTest()
	s.buildAndSubmit(run)
	// The edit stays out of git status so the handoff is not paused for a dirty tree.
	git(t, s.repo, "update-index", "--assume-unchanged", ".ao/pipelines/profiles/tester.yaml")
	writeFile(t, s.repo, ".ao/pipelines/profiles/tester.yaml", strings.Replace(testerProfileYAML, "Only change tests.", "Delete everything.", 1))
	svc := s.restart()
	if err := svc.DriveHandoffs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.exec.starts) == 0 {
		t.Fatalf("the handoff did not run: %s", brief(s.get()))
	}
	start := s.exec.starts[len(s.exec.starts)-1]
	if !strings.Contains(start.SystemPrompt, "Only change tests.") || strings.Contains(start.SystemPrompt, "Delete everything.") {
		t.Fatalf("a restarted handoff must use the snapshotted instructions:\n%s", start.SystemPrompt)
	}
}

func TestStaleEventsFromTheSourceControllerAreRejectedAfterTheHandoff(t *testing.T) {
	s := newStaged(t)
	run := s.startBuildTest()
	res, head := s.buildAndSubmit(run)
	_ = res
	buildAttempt := s.get().Attempts[0]
	if err := s.svc.DriveHandoffs(context.Background()); err != nil {
		t.Fatal(err)
	}
	view := s.get()
	spec := domain.SessionID(view.Attempts[1].ConversationSessionID)

	// The old Build controller replays its result, or claims the new stage's.
	if _, err := s.svc.Submit(context.Background(), pipelineruns.SubmitInput{
		SessionID: s.sessionID, RunID: run.ID, AttemptID: buildAttempt.ID, ControllerGeneration: buildAttempt.ControllerGeneration,
		IdempotencyKey: "different-key", Outcome: "succeeded", ExpectedInputCommit: buildAttempt.InputCommit, OutputCommit: head, Summary: "again",
	}); err == nil || code(t, err) != "PIPELINE_ATTEMPT_STALE" && code(t, err) != "PIPELINE_RESULT_CONFLICT" {
		t.Fatalf("a late result from the source stage is stale: %v", err)
	}
	if _, err := s.svc.Submit(context.Background(), pipelineruns.SubmitInput{
		SessionID: s.sessionID, RunID: run.ID, AttemptID: view.Attempts[1].ID, ControllerGeneration: view.Attempts[1].ControllerGeneration,
		IdempotencyKey: "steal", Outcome: "succeeded", ExpectedInputCommit: view.Attempts[1].InputCommit, OutputCommit: head, Report: passReport(),
	}); err == nil || code(t, err) != "PIPELINE_NOT_ATTEMPT_EXECUTOR" {
		t.Fatalf("the source worker cannot answer for the specialist: %v", err)
	}
	// After a restart and a controller change the same holds for the new executor.
	s.restart()
	sess, _, _ := s.store.GetSession(context.Background(), spec)
	sess.Metadata.ControllerGeneration = "spec-gen-restarted"
	if err := s.store.UpdateSession(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	s.restart()
	if v := s.get(); v.State != "paused" || v.PauseReason != "controller_changed" {
		t.Fatalf("a changed controller generation fences the old attempt: %s", brief(v))
	}
	if _, err := s.svc.Submit(context.Background(), pipelineruns.SubmitInput{
		SessionID: spec, RunID: run.ID, AttemptID: view.Attempts[1].ID, ControllerGeneration: view.Attempts[1].ControllerGeneration,
		IdempotencyKey: "late", Outcome: "succeeded", ExpectedInputCommit: view.Attempts[1].InputCommit, OutputCommit: head, Report: passReport(),
	}); err == nil {
		t.Fatal("a paused run takes no results from the stale controller")
	}
}

func TestUnknownLivenessNeverPausesReplaysOrLaunches(t *testing.T) {
	s := newStaged(t)
	run := s.startBuildTest()
	eventsBefore := len(s.exec.events)
	for i := 0; i < 3; i++ {
		s.restart()
		if err := s.svc.DriveHandoffs(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	v := s.get()
	if v.State != "running" || v.Attempts[0].State != "active" || v.Attempts[0].ID != run.Attempts[0].ID {
		t.Fatalf("restarts alone prove nothing about the executor: %s", brief(v))
	}
	for _, ev := range s.exec.events[eventsBefore:] {
		if !strings.HasPrefix(ev, "reconnect:") {
			t.Fatalf("reconciliation may only try to reconnect to a surviving host, never start, resume, or fence one: %v", s.exec.events[eventsBefore:])
		}
	}
	if v.Control.LastRecovery == nil || v.Control.LastRecovery.Outcome != "continued" {
		t.Fatalf("the continuation is reported: %+v", v.Control.LastRecovery)
	}
	// Ordinary reads re-check ownership but never report a recovery.
	n := len(v.Events)
	for i := 0; i < 3; i++ {
		_ = s.get()
	}
	if len(s.get().Events) != n {
		t.Fatal("reads must not append recovery events")
	}
}

func TestRestartReconnectsToSurvivingControllersAndNeverReadsAMissingOneAsDead(t *testing.T) {
	s := newStaged(t)
	run := s.startBuildTest()
	s.restart()
	v := s.get()
	if len(s.exec.reconnected) != 1 || s.exec.reconnected[0] != s.sessionID {
		t.Fatalf("the active executor is offered a reconnect: %v", s.exec.reconnected)
	}
	if rec := v.Control.LastRecovery; rec == nil || rec.Outcome != "continued" || !strings.Contains(rec.Message, "reconnected to the surviving controller of "+string(s.sessionID)) {
		t.Fatalf("a surviving controller is adopted and said so: %+v", rec)
	}

	// The host did not survive: not proof of death, so the run is not paused and
	// nothing is started in its place.
	s.exec.hostGone = map[domain.SessionID]bool{s.sessionID: true}
	s.restart()
	v = s.get()
	if v.State != "running" || v.Attempts[0].ID != run.Attempts[0].ID || len(s.exec.starts) != 0 {
		t.Fatalf("a missing host must not pause, replay, or launch: %s", brief(v))
	}
	if rec := v.Control.LastRecovery; rec == nil || !strings.Contains(rec.Message, "no surviving controller could be adopted") || !strings.Contains(rec.Message, "not proof") {
		t.Fatalf("the unknown is reported honestly: %+v", rec)
	}

	// An error while reconnecting is reported the same way, never fatal.
	s.exec.reconnectErr = errors.New("provider host probe failed")
	s.restart()
	if v := s.get(); v.State != "running" {
		t.Fatalf("a failed probe is unknown, not death: %s", brief(v))
	}
}

func TestPausedAndCancelledRunsAreLeftAloneAcrossRestart(t *testing.T) {
	s := newStaged(t)
	run := s.startBuildTest()
	s.mustControl(run, "pause", "user")
	cancelled := s.startAnother(run)
	before := len(s.exec.events)
	for i := 0; i < 2; i++ {
		s.restart()
		if err := s.svc.DriveHandoffs(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if v := s.get(); v.ID != cancelled.ID {
		t.Fatalf("setup: %s", v.ID)
	}
	if len(s.exec.events) != before {
		t.Fatalf("paused or cancelled runs never restart on their own: %v", s.exec.events[before:])
	}
}

// startAnother cancels the paused run and returns the (cancelled) latest view.
func (s *staged) startAnother(run pipelineruns.RunView) pipelineruns.RunView {
	s.t.Helper()
	s.mustControl(run, "cancel", "user")
	return s.get()
}

func TestRecoveryDecisionIsExplicitAndNeverStartsAFreshConversation(t *testing.T) {
	s := newStaged(t)
	run := s.startBuildTest()
	s.buildAndSubmit(run)
	if err := s.svc.DriveHandoffs(context.Background()); err != nil {
		t.Fatal(err)
	}
	view := s.get()
	spec := domain.SessionID(view.Attempts[1].ConversationSessionID)
	startsBefore := len(s.exec.starts)

	// The restarted daemon sees the Tester under a new controller generation.
	sess, _, _ := s.store.GetSession(context.Background(), spec)
	sess.Metadata.ControllerGeneration = "after-restart"
	if err := s.store.UpdateSession(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	s.restart()
	paused := s.get()
	if paused.State != "paused" || paused.PauseReason != "controller_changed" {
		t.Fatalf("%s", brief(paused))
	}

	// Resuming tries to continue the same conversation, whose controller is gone.
	s.exec.resumeErr = fmt.Errorf("%w: its controller is not running", ports.ErrPipelineResumeUnsafe)
	s.mustControl(paused, "resume", "user")
	if err := s.svc.DriveHandoffs(context.Background()); err != nil {
		t.Fatal(err)
	}
	decision := s.get()
	if decision.State != "paused" || decision.PauseReason != "recovery_decision_required" || len(decision.Control.RecoveryOptions) != 1 || decision.Control.RecoveryOptions[0] != "restore_conversation" {
		t.Fatalf("the unprovable resume is a recovery decision: %s %+v", brief(decision), decision.Control)
	}
	if len(s.exec.starts) != startsBefore {
		t.Fatal("no fresh conversation may be started")
	}

	// Without an explicit choice, or by an orchestrator, nothing happens.
	if _, err := s.control(decision, "resume", "user"); err == nil || code(t, err) != "PIPELINE_RECOVERY_CHOICE_REQUIRED" {
		t.Fatalf("a recovery needs an explicit choice: %v", err)
	}
	if _, err := s.control(decision, "resume", "orchestrator", func(in *pipelineruns.ControlInput) { in.Recovery = "restore_conversation" }); err == nil || code(t, err) != "PIPELINE_HUMAN_DECISION_REQUIRED" {
		t.Fatalf("recovery is human-only: %v", err)
	}
	if _, err := s.control(decision, "resume", "user", func(in *pipelineruns.ControlInput) { in.Recovery = "start_fresh" }); err == nil || code(t, err) != "INVALID_PIPELINE_CONTROL" {
		t.Fatalf("there is no fresh-conversation option: %v", err)
	}
	if len(s.exec.restored) != 0 {
		t.Fatal("nothing was restored yet")
	}

	// A failed restore leaves the run paused and starts nothing.
	s.exec.restoreErr = errors.New("provider history is unavailable")
	if _, err := s.control(decision, "resume", "user", func(in *pipelineruns.ControlInput) { in.Recovery = "restore_conversation" }); err == nil || code(t, err) != "PIPELINE_RECOVERY_FAILED" {
		t.Fatalf("a failed restore: %v", err)
	}
	if s.get().State != "paused" || len(s.exec.starts) != startsBefore {
		t.Fatal("a failed recovery changes nothing")
	}

	// A successful restore brings back the SAME conversation and continues.
	s.exec.restoreErr, s.exec.resumeErr = nil, nil
	res := s.mustControl(decision, "resume", "user", func(in *pipelineruns.ControlInput) { in.Recovery = "restore_conversation" })
	if res.Run.State != "running" || len(s.exec.restored) != 2 || s.exec.restored[1] != spec {
		t.Fatalf("restore: %s restored=%v", brief(res.Run), s.exec.restored)
	}
	if err := s.svc.DriveHandoffs(context.Background()); err != nil {
		t.Fatal(err)
	}
	final := s.get()
	last := final.Attempts[len(final.Attempts)-1]
	if last.State != "active" || last.ConversationSessionID != string(spec) || len(s.exec.starts) != startsBefore || s.attachedCount() != 1 {
		t.Fatalf("the original conversation continues: %+v starts=%d attached=%d", last, len(s.exec.starts)-startsBefore, s.attachedCount())
	}
}

func TestRecoveryChoiceOnlyAppliesToARecoveryDecision(t *testing.T) {
	s := newStaged(t)
	run := s.startBuildTest()
	s.mustControl(run, "pause", "user")
	if _, err := s.control(run, "resume", "user", func(in *pipelineruns.ControlInput) { in.Recovery = "restore_conversation" }); err == nil || code(t, err) != "INVALID_PIPELINE_CONTROL" {
		t.Fatalf("%v", err)
	}
}

func TestBudgetAndAttemptFactsSurviveRestartExactly(t *testing.T) {
	s := newSpecialistStage(t)
	for i := 1; i <= 2; i++ {
		if _, _, err := s.reportDefect(fmt.Sprintf("d%d", i), "defect"); err != nil {
			t.Fatal(err)
		}
		s.restart() // a restart between every step
		s.drive()
		s.restart()
		s.repairAsBuild(fmt.Sprintf("r%d", i), s.get())
		s.drive()
		s.nextTesterAttempt()
	}
	v := s.get()
	if v.RepairsUsed != 2 || len(v.Repairs) != 2 || v.RepairBudget != 3 {
		t.Fatalf("restarts neither spend nor refund the budget: %s repairs=%d", brief(v), len(v.Repairs))
	}
}
