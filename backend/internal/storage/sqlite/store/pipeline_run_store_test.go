package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func seedPipelineSession(t *testing.T, project string) (*sqlite.Store, domain.SessionID) {
	t.Helper()
	s := newTestStore(t)
	seedProject(t, s, project)
	rec, err := s.CreateSession(context.Background(), sampleRecord(project))
	if err != nil {
		t.Fatal(err)
	}
	return s, rec.ID
}

func newRun(id string, session domain.SessionID, project string, at time.Time) (domain.PipelineRun, domain.PipelineStageAttempt) {
	return domain.PipelineRun{
		ID: id, SessionID: session, ProjectID: domain.ProjectID(project), WorkflowID: "wf", State: domain.PipelineRunRunning,
		CurrentStageID: "build", RequestedBy: domain.PipelineRequestedByUser, ExpectedBranch: "feat/x", RepairBudget: 3,
		Snapshot: `{"schemaVersion":1}`, SnapshotSHA256: "abc", CreatedAt: at, UpdatedAt: at,
	}, domain.PipelineStageAttempt{
		ID: id + "-a1", RunID: id, StageID: "build", StageKind: "build", AttemptNo: 1, ExecutorSessionID: session,
		ControllerGeneration: "g1", InputCommit: "deadbeef", StartedAt: at,
	}
}

func TestPipelineRunOneUnfinishedRunPerSession(t *testing.T) {
	ctx := context.Background()
	s, sid := seedPipelineSession(t, "pr1")
	at := time.Now().UTC().Truncate(time.Second)
	run, attempt := newRun("run-1", sid, "pr1", at)
	if _, _, err := s.CreatePipelineRun(ctx, run, attempt); err != nil {
		t.Fatal(err)
	}
	run2, attempt2 := newRun("run-2", sid, "pr1", at.Add(time.Second))
	if _, _, err := s.CreatePipelineRun(ctx, run2, attempt2); !errors.Is(err, domain.ErrPipelineRunActive) {
		t.Fatalf("second unfinished run must be refused, got %v", err)
	}
	if has, err := s.HasUnfinishedPipelineRun(ctx, sid); err != nil || !has {
		t.Fatalf("has=%v err=%v", has, err)
	}
	// The rejected create must not leave a half-written attempt behind.
	if _, ok, _ := s.GetPipelineStageAttempt(ctx, "run-2-a1"); ok {
		t.Fatal("failed create must roll back")
	}
}

func TestPipelineTransitionCompareAndSet(t *testing.T) {
	ctx := context.Background()
	s, sid := seedPipelineSession(t, "pr2")
	at := time.Now().UTC().Truncate(time.Second)
	run, attempt := newRun("run-1", sid, "pr2", at)
	created, _, err := s.CreatePipelineRun(ctx, run, attempt)
	if err != nil {
		t.Fatal(err)
	}

	done := at.Add(time.Minute)
	accept := domain.PipelineTransition{
		RunID: run.ID, ExpectedRevision: created.Revision, At: done,
		Attempt: &domain.PipelineAttemptFinish{ID: attempt.ID, State: domain.PipelineAttemptAccepted, OutputCommit: "cafe", Outcome: "succeeded", ResultKey: "k", FinishedAt: done},
		Run:     &domain.PipelineRunUpdate{State: domain.PipelineRunCompleted, CompletedAt: &done},
		Events:  []domain.PipelineEvent{{AttemptID: attempt.ID, Kind: "stage_accepted"}},
	}
	updated, err := s.CommitPipelineTransition(ctx, accept)
	if err != nil {
		t.Fatal(err)
	}
	if updated.State != domain.PipelineRunCompleted || updated.Revision != created.Revision+1 || updated.CompletedAt == nil {
		t.Fatalf("updated: %+v", updated)
	}
	if has, _ := s.HasUnfinishedPipelineRun(ctx, sid); has {
		t.Fatal("completed run is finished")
	}

	// A duplicate or stale writer loses the compare-and-set and changes nothing.
	if _, err := s.CommitPipelineTransition(ctx, accept); !errors.Is(err, domain.ErrPipelineConflict) {
		t.Fatalf("stale transition must conflict, got %v", err)
	}
	events, _ := s.ListPipelineEvents(ctx, run.ID, 10)
	if len(events) != 1 {
		t.Fatalf("a lost race must not append events: %+v", events)
	}
	got, _, _ := s.GetPipelineStageAttempt(ctx, attempt.ID)
	if got.State != domain.PipelineAttemptAccepted || got.OutputCommit != "cafe" || got.ResultKey != "k" {
		t.Fatalf("attempt: %+v", got)
	}

	// A finished run frees the session for a new one.
	next, nextAttempt := newRun("run-2", sid, "pr2", done.Add(time.Second))
	if _, _, err := s.CreatePipelineRun(ctx, next, nextAttempt); err != nil {
		t.Fatalf("new run after completion: %v", err)
	}
	latest, ok, _ := s.GetLatestPipelineRunBySession(ctx, sid)
	if !ok || latest.ID != "run-2" {
		t.Fatalf("latest: %+v", latest)
	}
}

func TestPipelinePauseRequiresReason(t *testing.T) {
	ctx := context.Background()
	s, sid := seedPipelineSession(t, "pr3")
	run, attempt := newRun("run-1", sid, "pr3", time.Now().UTC().Truncate(time.Second))
	created, _, _ := s.CreatePipelineRun(ctx, run, attempt)
	_, err := s.CommitPipelineTransition(ctx, domain.PipelineTransition{
		RunID: run.ID, ExpectedRevision: created.Revision, At: time.Now().UTC(),
		Run: &domain.PipelineRunUpdate{State: domain.PipelineRunPaused},
	})
	if err == nil {
		t.Fatal("a paused run without a reason violates the schema invariant")
	}
}

// Pipeline changes surface through the existing session CDC stream, so the
// desktop refreshes the task without a pipeline-specific event type.
func TestPipelineChangesEmitSessionUpdatedCDC(t *testing.T) {
	ctx := context.Background()
	s, sid := seedPipelineSession(t, "pr4")
	before, _ := s.EventsAfter(ctx, 0, 1000)
	run, attempt := newRun("run-1", sid, "pr4", time.Now().UTC().Truncate(time.Second))
	created, _, err := s.CreatePipelineRun(ctx, run, attempt)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddPipelineEvent(ctx, domain.PipelineEvent{RunID: run.ID, Kind: "submission_rejected", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	done := time.Now().UTC()
	if _, err := s.CommitPipelineTransition(ctx, domain.PipelineTransition{
		RunID: run.ID, ExpectedRevision: created.Revision, At: done,
		Attempt: &domain.PipelineAttemptFinish{ID: attempt.ID, State: domain.PipelineAttemptAccepted, FinishedAt: done},
		Run:     &domain.PipelineRunUpdate{State: domain.PipelineRunCompleted, CompletedAt: &done},
	}); err != nil {
		t.Fatal(err)
	}
	after, err := s.EventsAfter(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	updates := 0
	for _, e := range after[len(before):] {
		if e.Type == "session_updated" && e.SessionID == string(sid) {
			updates++
		}
	}
	if updates < 4 { // run insert, attempt insert, event insert, attempt/run updates
		t.Fatalf("expected session_updated events for pipeline writes, got %d", updates)
	}
}
