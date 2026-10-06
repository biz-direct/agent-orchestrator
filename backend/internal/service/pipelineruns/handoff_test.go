package pipelineruns_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/pipelineruns"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

const buildTestWorkflow = `version: 1
id: build-test
description: Build then Test
instructions: Keep changes small.
stages:
  - {id: build, kind: build}
  - {id: test, kind: specialist, profile: tester, repairTo: build}
`

const testerProfileYAML = `version: 1
id: tester
description: Writes and improves tests
instructions: You are the Tester. Only change tests.
harness: claude-code
allowedPaths: ["**/*_test.go"]
`

// fakeExecutor stands in for the Chat-backed executor. StartStage creates a
// real hidden attached session so the store-level isolation is exercised too.
type fakeExecutor struct {
	mu    sync.Mutex
	store *sqlite.Store
	repo  string

	preflightErr  error
	relinquishErr error
	startErr      error
	onRelinquish  func()
	onStart       func()

	events      []string
	relinquish  []domain.SessionID
	starts      []ports.PipelineStageStart
	stopped     []domain.SessionID
	startedGens int
}

func (e *fakeExecutor) PreflightStage(_ context.Context, harness domain.AgentHarness) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, "preflight:"+string(harness))
	return e.preflightErr
}

func (e *fakeExecutor) RelinquishExecutor(_ context.Context, id domain.SessionID) error {
	e.mu.Lock()
	hook := e.onRelinquish
	e.mu.Unlock()
	if hook != nil {
		hook()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, "relinquish:"+string(id))
	e.relinquish = append(e.relinquish, id)
	return e.relinquishErr
}

func (e *fakeExecutor) StartStage(ctx context.Context, start ports.PipelineStageStart) (ports.PipelineStageStarted, error) {
	e.mu.Lock()
	hook := e.onStart
	e.mu.Unlock()
	if hook != nil {
		hook()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, "start:"+start.StageID)
	e.starts = append(e.starts, start)
	if e.startErr != nil {
		return ports.PipelineStageStarted{}, e.startErr
	}
	e.startedGens++
	now := time.Now().UTC().Truncate(time.Second)
	rec, err := e.store.CreateAttachedSession(ctx, domain.SessionRecord{
		ProjectID: "proj", Kind: domain.KindWorker, Harness: start.Harness, Mode: domain.SessionModeChat,
		Activity:  domain.Activity{State: domain.ActivityActive, LastActivityAt: now},
		Metadata:  domain.SessionMetadata{WorkspacePath: e.repo, ControllerGeneration: fmt.Sprintf("spec-gen-%d", e.startedGens)},
		CreatedAt: now, UpdatedAt: now,
	}, start.Owner)
	if err != nil {
		return ports.PipelineStageStarted{}, err
	}
	return ports.PipelineStageStarted{SessionID: rec.ID, ControllerGeneration: fmt.Sprintf("spec-gen-%d", e.startedGens)}, nil
}

func (e *fakeExecutor) StopStage(_ context.Context, id domain.SessionID) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, "stop:"+string(id))
	e.stopped = append(e.stopped, id)
	return nil
}

type staged struct {
	*fixture
	exec *fakeExecutor
	gate *pipelineruns.StoreGate
}

func newStaged(t *testing.T) *staged {
	t.Helper()
	f := newFixture(t, map[string]string{
		".ao/pipelines/workflows/build-test.yaml": buildTestWorkflow,
		".ao/pipelines/profiles/tester.yaml":      testerProfileYAML,
	})
	exec := &fakeExecutor{store: f.store, repo: f.repo}
	f.svc = pipelineruns.New(pipelineruns.Deps{Store: f.store, Messenger: f.messenger, Executor: exec})
	return &staged{fixture: f, exec: exec, gate: pipelineruns.NewStoreGate(f.store, nil)}
}

func (s *staged) startBuildTest() pipelineruns.RunView {
	s.t.Helper()
	run, err := s.svc.Start(context.Background(), pipelineruns.StartInput{SessionID: s.sessionID, WorkflowID: "build-test", RequestedBy: "user"})
	if err != nil {
		s.t.Fatal(err)
	}
	return run
}

// buildAndSubmit commits a change as the worker and submits the Build stage.
func (s *staged) buildAndSubmit(run pipelineruns.RunView) (pipelineruns.SubmitResult, string) {
	s.t.Helper()
	writeFile(s.t, s.repo, "feature.txt", "work\n")
	git(s.t, s.repo, "add", "-A")
	git(s.t, s.repo, "commit", "-q", "-m", "feature")
	head := git(s.t, s.repo, "rev-parse", "HEAD")
	res, err := s.submit(run, "succeeded", head, "build-key")
	if err != nil {
		s.t.Fatal(err)
	}
	return res, head
}

func (s *staged) get() pipelineruns.RunView {
	s.t.Helper()
	env, err := s.svc.Get(context.Background(), s.sessionID)
	if err != nil || env.Run == nil {
		s.t.Fatalf("get: %v %+v", err, env)
	}
	return *env.Run
}

func (s *staged) admitted(id domain.SessionID) bool {
	ok, _ := s.gate.AdmitSessionExecution(context.Background(), id)
	return ok
}

func TestBuildThenTestHandoffRunsInTheSameWorktreeWithItsOwnConversation(t *testing.T) {
	s := newStaged(t)
	ctx := context.Background()
	run := s.startBuildTest()
	if !s.admitted(s.sessionID) {
		t.Fatal("the worker executes the Build stage")
	}

	res, head := s.buildAndSubmit(run)
	if !res.Accepted || res.Run.State != "running" || res.Run.CurrentStageID != "test" {
		t.Fatalf("build accepted but run must continue to test: %+v", res.Run)
	}
	// Nothing executes until the handoff is driven: the successor is only recorded.
	if len(s.exec.starts) != 0 || res.Run.Stages[1].State != "handoff" || res.Run.Attempts[1].State != "handoff" {
		t.Fatalf("handoff must be asynchronous and durable: starts=%d %+v", len(s.exec.starts), res.Run.Stages)
	}
	if s.admitted(s.sessionID) {
		t.Fatal("once Build is accepted the worker may not wake: no stage is executing during the handoff")
	}

	if err := s.svc.DriveHandoffs(ctx); err != nil {
		t.Fatal(err)
	}
	// Order is the invariant: the source is proven stopped before the target starts.
	if got := strings.Join(s.exec.events, ","); got != "preflight:claude-code,preflight:claude-code,relinquish:"+string(s.sessionID)+",start:test" {
		t.Fatalf("handoff order: %s", got)
	}
	view := s.get()
	test := view.Attempts[1]
	if view.State != "running" || test.State != "active" || test.ConversationSessionID == "" || test.InputCommit != head || test.PredecessorAttemptID != view.Attempts[0].ID {
		t.Fatalf("test attempt: %+v", test)
	}
	if view.Stages[1].State != "active" {
		t.Fatalf("stage presentation: %+v", view.Stages)
	}

	// Same worktree identity; separate conversation; stage inputs, not inherited reasoning.
	spec := domain.SessionID(test.ConversationSessionID)
	rec, ok, _ := s.store.GetSession(ctx, spec)
	if !ok || rec.Metadata.WorkspacePath != s.repo || spec == s.sessionID {
		t.Fatalf("specialist must share the worker's worktree under its own session: %+v", rec)
	}
	start := s.exec.starts[0]
	if start.Owner != s.sessionID || start.Harness != domain.HarnessClaudeCode || start.ExistingSessionID != "" {
		t.Fatalf("start: %+v", start)
	}
	for _, want := range []string{"You are the Tester. Only change tests.", "**/*_test.go", "Keep changes small.", "do not push"} {
		if !strings.Contains(start.SystemPrompt, want) {
			t.Fatalf("system prompt missing %q:\n%s", want, start.SystemPrompt)
		}
	}
	for _, want := range []string{head, "task-branch", "built it"[:0] + "did the thing", "did not inherit", "ao pipeline submit"} {
		if !strings.Contains(start.Prompt, want) {
			t.Fatalf("stage prompt missing %q:\n%s", want, start.Prompt)
		}
	}

	// Exclusive execution: only the active stage's executor is admitted.
	if s.admitted(s.sessionID) || !s.admitted(spec) {
		t.Fatalf("worker admitted=%v specialist admitted=%v", s.admitted(s.sessionID), s.admitted(spec))
	}

	// The specialist finishes under its own session id and the run completes.
	writeFile(t, s.repo, "feature_test.go", "package x\n")
	git(t, s.repo, "add", "-A")
	git(t, s.repo, "commit", "-q", "-m", "tests")
	newHead := git(t, s.repo, "rev-parse", "HEAD")
	done, err := s.svc.Submit(ctx, pipelineruns.SubmitInput{
		SessionID: spec, RunID: view.ID, AttemptID: test.ID, ControllerGeneration: test.ControllerGeneration,
		IdempotencyKey: "test-key", Outcome: "succeeded", ExpectedInputCommit: test.InputCommit, OutputCommit: newHead, Summary: "added tests",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !done.Accepted || done.Run.State != "completed" || done.Run.Checkpoint == nil || done.Run.Checkpoint.OutputCommit != newHead || done.Run.Checkpoint.StageID != "test" {
		t.Fatalf("test stage result: %+v", done.Run)
	}
	if s.admitted(spec) {
		t.Fatal("a finished stage's conversation can no longer be woken")
	}
	if !s.admitted(s.sessionID) {
		t.Fatal("a completed run releases the worker")
	}
}

func TestAttachedSessionsStayOutOfSessionListings(t *testing.T) {
	s := newStaged(t)
	ctx := context.Background()
	run := s.startBuildTest()
	s.buildAndSubmit(run)
	if err := s.svc.DriveHandoffs(ctx); err != nil {
		t.Fatal(err)
	}
	all, _ := s.store.ListAllSessions(ctx)
	byProject, _ := s.store.ListSessions(ctx, "proj")
	if len(all) != 1 || len(byProject) != 1 || all[0].ID != s.sessionID {
		t.Fatalf("the attached specialist must not appear as an unrelated task: all=%+v project=%+v", all, byProject)
	}
	if !s.svc.SuppressesLifecycleShortcuts(ctx, s.sessionID) {
		t.Fatal("the owner worker's lifecycle stays owned by the run")
	}
}

func TestConcurrentHandoffRequestsStartTheSuccessorOnce(t *testing.T) {
	s := newStaged(t)
	run := s.startBuildTest()
	s.buildAndSubmit(run)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.svc.DriveHandoffs(context.Background())
		}()
	}
	wg.Wait()
	if len(s.exec.starts) != 1 || len(s.exec.relinquish) != 1 {
		t.Fatalf("overlapping handoff requests must collapse into one: starts=%d relinquish=%d", len(s.exec.starts), len(s.exec.relinquish))
	}
	attached, _ := s.store.ListAttachedSessionIDs(context.Background(), s.sessionID)
	if len(attached) != 1 {
		t.Fatalf("exactly one specialist conversation: %v", attached)
	}
}

func TestHandoffPausesWhenTheSourceCannotBeProvenQuiescent(t *testing.T) {
	for name, cause := range map[string]string{
		"unconfirmed shutdown": "controller did not confirm it stopped",
		"pending approval":     "a permission request is waiting for the user",
	} {
		t.Run(name, func(t *testing.T) {
			s := newStaged(t)
			run := s.startBuildTest()
			s.buildAndSubmit(run)
			s.exec.relinquishErr = fmt.Errorf("%w: %s", ports.ErrPipelineExecutionUncertain, cause)

			if err := s.svc.DriveHandoffs(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(s.exec.starts) != 0 {
				t.Fatal("the successor must not start while the source may still write")
			}
			view := s.get()
			if view.State != "paused" || view.PauseReason != "handoff_uncertain" || !strings.Contains(view.PauseDetail, cause) || view.RepairsUsed != 0 {
				t.Fatalf("run: %+v", view)
			}
			if view.Attempts[1].State != "handoff" {
				t.Fatalf("the successor stays recorded for a later retry: %+v", view.Attempts[1])
			}
			if s.admitted(s.sessionID) {
				t.Fatal("nobody executes while the handoff is unresolved")
			}
			// A repeated pass must not retry blindly behind a pause.
			if err := s.svc.DriveHandoffs(context.Background()); err != nil || len(s.exec.relinquish) != 1 {
				t.Fatalf("paused runs are not re-driven: %v relinquish=%d", err, len(s.exec.relinquish))
			}
		})
	}
}

func TestHandoffPreservesChangesMadeAfterAcceptance(t *testing.T) {
	s := newStaged(t)
	run := s.startBuildTest()
	s.buildAndSubmit(run)
	// The source slips an uncommitted edit in before it fully stops.
	s.exec.onRelinquish = func() { writeFile(t, s.repo, "late.txt", "sneaky\n") }

	if err := s.svc.DriveHandoffs(context.Background()); err != nil {
		t.Fatal(err)
	}
	view := s.get()
	if view.State != "paused" || view.PauseReason != "unexpected_changes" || len(s.exec.starts) != 0 {
		t.Fatalf("run: %+v starts=%d", view, len(s.exec.starts))
	}
	if _, err := os.Stat(filepath.Join(s.repo, "late.txt")); err != nil {
		t.Fatal("unexpected changes are preserved, never reset")
	}
}

func TestHandoffPausesWhenHeadMovedAfterAcceptance(t *testing.T) {
	s := newStaged(t)
	run := s.startBuildTest()
	s.buildAndSubmit(run)
	s.exec.onRelinquish = func() {
		writeFile(t, s.repo, "extra.txt", "x\n")
		git(t, s.repo, "add", "-A")
		git(t, s.repo, "commit", "-q", "-m", "extra")
	}
	if err := s.svc.DriveHandoffs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if view := s.get(); view.State != "paused" || view.PauseReason != "unexpected_changes" || !strings.Contains(view.PauseDetail, "HEAD moved") {
		t.Fatalf("run: %+v", view)
	}
}

func TestUnsupportedStageIsRejectedAtStartAndPausedAtHandoff(t *testing.T) {
	s := newStaged(t)
	s.exec.preflightErr = fmt.Errorf("%w: harness has no Chat driver", ports.ErrPipelineStageUnsupported)
	_, err := s.svc.Start(context.Background(), pipelineruns.StartInput{SessionID: s.sessionID, WorkflowID: "build-test", RequestedBy: "user"})
	if code(t, err) != "PIPELINE_STAGE_UNSUPPORTED" {
		t.Fatalf("unsupported harness must be refused up front, not fall back to a terminal: %v", err)
	}

	// If support disappears between start and handoff, the handoff pauses visibly.
	s2 := newStaged(t)
	run := s2.startBuildTest()
	s2.buildAndSubmit(run)
	s2.exec.preflightErr = errors.New("harness binary is gone")
	if err := s2.svc.DriveHandoffs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if view := s2.get(); view.State != "paused" || view.PauseReason != "stage_unsupported" || len(s2.exec.starts) != 0 || len(s2.exec.relinquish) != 0 {
		t.Fatalf("run: %+v", view)
	}
}

func TestSpecialistStartFailurePausesWithoutAnActiveStage(t *testing.T) {
	s := newStaged(t)
	run := s.startBuildTest()
	s.buildAndSubmit(run)
	s.exec.startErr = errors.New("provider refused to start")
	if err := s.svc.DriveHandoffs(context.Background()); err != nil {
		t.Fatal(err)
	}
	view := s.get()
	if view.State != "paused" || view.PauseReason != "stage_start_failed" || view.Attempts[1].State != "handoff" || !strings.Contains(view.PauseDetail, "provider refused") {
		t.Fatalf("run: %+v", view)
	}
}

func TestStaleAndForeignEventsCannotSatisfyAnotherStage(t *testing.T) {
	s := newStaged(t)
	ctx := context.Background()
	run := s.startBuildTest()
	res, head := s.buildAndSubmit(run)
	buildAttempt := res.Run.Attempts[0]
	testAttempt := res.Run.Attempts[1]

	// The old executor reporting again after acceptance is stale, not a replay of a new result.
	if _, err := s.submit(run, "succeeded", head, "another-key"); code(t, err) != "PIPELINE_ATTEMPT_STALE" {
		t.Fatalf("worker after handoff: %v", err)
	}
	// While the successor is still in handoff nobody can submit for it.
	input := pipelineruns.SubmitInput{SessionID: s.sessionID, RunID: run.ID, AttemptID: testAttempt.ID, ControllerGeneration: "x", IdempotencyKey: "k", Outcome: "succeeded", ExpectedInputCommit: head, OutputCommit: head}
	if _, err := s.svc.Submit(ctx, input); code(t, err) != "PIPELINE_ATTEMPT_STALE" {
		t.Fatalf("submit for a handoff attempt: %v", err)
	}
	if err := s.svc.DriveHandoffs(ctx); err != nil {
		t.Fatal(err)
	}
	// Once active, the worker still cannot answer for the specialist's attempt.
	if _, err := s.svc.Submit(ctx, input); code(t, err) != "PIPELINE_NOT_ATTEMPT_EXECUTOR" {
		t.Fatalf("worker answering for the specialist: %v", err)
	}
	spec := domain.SessionID(s.get().Attempts[1].ConversationSessionID)
	forged := pipelineruns.SubmitInput{SessionID: spec, RunID: run.ID, AttemptID: buildAttempt.ID, ControllerGeneration: buildAttempt.ControllerGeneration, IdempotencyKey: "k2", Outcome: "succeeded", ExpectedInputCommit: buildAttempt.InputCommit, OutputCommit: head}
	if _, err := s.svc.Submit(ctx, forged); code(t, err) != "PIPELINE_NOT_ATTEMPT_EXECUTOR" && code(t, err) != "PIPELINE_ATTEMPT_STALE" {
		t.Fatalf("specialist answering for Build: %v", err)
	}
}

func TestSpecialistControllerRestartPausesInsteadOfReplaying(t *testing.T) {
	s := newStaged(t)
	ctx := context.Background()
	run := s.startBuildTest()
	s.buildAndSubmit(run)
	if err := s.svc.DriveHandoffs(ctx); err != nil {
		t.Fatal(err)
	}
	spec := domain.SessionID(s.get().Attempts[1].ConversationSessionID)
	rec, _, _ := s.store.GetSession(ctx, spec)
	rec.Metadata.ControllerGeneration = "spec-gen-restarted"
	if err := s.store.UpdateSession(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err := s.svc.ReconcileAll(ctx); err != nil {
		t.Fatal(err)
	}
	view := s.get()
	if view.State != "paused" || view.PauseReason != "controller_changed" || view.Attempts[1].State != "interrupted" || len(s.exec.starts) != 1 {
		t.Fatalf("run: %+v starts=%d", view, len(s.exec.starts))
	}
}

func TestStageExecutorStartedAfterRunPausedIsStopped(t *testing.T) {
	s := newStaged(t)
	ctx := context.Background()
	run := s.startBuildTest()
	s.buildAndSubmit(run)
	// A human pauses the run while the specialist is still starting.
	s.exec.onStart = func() {
		cur, _, _ := s.store.GetPipelineRun(ctx, run.ID)
		_, err := s.store.CommitPipelineTransition(ctx, domain.PipelineTransition{
			RunID: run.ID, ExpectedRevision: cur.Revision, At: time.Now().UTC(),
			Run: &domain.PipelineRunUpdate{State: domain.PipelineRunPaused, PauseReason: "user_paused", CurrentStageID: cur.CurrentStageID},
		})
		if err != nil {
			t.Error(err)
		}
	}
	if err := s.svc.DriveHandoffs(ctx); err != nil {
		t.Fatal(err)
	}
	if len(s.exec.stopped) != 1 {
		t.Fatalf("an executor that lost the race must be stopped: %v", s.exec.stopped)
	}
	if _, err := os.Stat(filepath.Join(s.repo, "feature.txt")); err != nil {
		t.Fatal("stage cleanup never touches the shared workspace")
	}
	if view := s.get(); view.State != "paused" || view.Attempts[1].State != "handoff" {
		t.Fatalf("run: %+v", view)
	}
}

func TestGateIsInvisibleToOrdinaryWorkers(t *testing.T) {
	s := newStaged(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	other, err := s.store.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "proj", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat,
		Activity: domain.Activity{State: domain.ActivityActive, LastActivityAt: now},
		Metadata: domain.SessionMetadata{WorkspacePath: s.repo}, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	run := s.startBuildTest()
	s.buildAndSubmit(run)
	if !s.admitted(other.ID) {
		t.Fatal("an unrelated worker must be unaffected by another task's pipeline")
	}
	if ok, _ := s.gate.AdmitSessionExecution(ports.WithPipelineBypass(ctx), s.sessionID); !ok {
		t.Fatal("the coordinator's own deliveries bypass the gate")
	}
	if ok, reason := s.gate.AdmitSessionExecution(ctx, s.sessionID); ok || reason == "" {
		t.Fatalf("the worker is refused with a reason: %v %q", ok, reason)
	}
}

func TestStartRefusesAnAttachedSpecialistAsTheTarget(t *testing.T) {
	s := newStaged(t)
	ctx := context.Background()
	run := s.startBuildTest()
	s.buildAndSubmit(run)
	if err := s.svc.DriveHandoffs(ctx); err != nil {
		t.Fatal(err)
	}
	spec := domain.SessionID(s.get().Attempts[1].ConversationSessionID)
	_, err := s.svc.Start(ctx, pipelineruns.StartInput{SessionID: spec, WorkflowID: "build-test", RequestedBy: "user"})
	if code(t, err) != "PIPELINE_SESSION_UNSUPPORTED" {
		t.Fatalf("a stage session cannot start pipelines of its own: %v", err)
	}
}
