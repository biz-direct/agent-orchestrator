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

func TestPipelineCommandEvidenceLifecycle(t *testing.T) {
	ctx := context.Background()
	s, sid := seedPipelineSession(t, "pr5")
	at := time.Now().UTC().Truncate(time.Second)
	run, attempt := newRun("run-1", sid, "pr5", at)
	created, _, err := s.CreatePipelineRun(ctx, run, attempt)
	if err != nil {
		t.Fatal(err)
	}
	// An active attempt moves into validation exactly once.
	validate := domain.PipelineTransition{
		RunID: run.ID, ExpectedRevision: created.Revision, At: at,
		Validate: &domain.PipelineAttemptValidation{ID: attempt.ID, OutputCommit: "cafe", Outcome: "succeeded", ResultKey: "k", ResultJSON: "{}"},
	}
	if _, err := s.CommitPipelineTransition(ctx, validate); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.GetPipelineStageAttempt(ctx, attempt.ID); got.State != domain.PipelineAttemptValidating || got.OutputCommit != "cafe" || got.ResultKey != "k" {
		t.Fatalf("attempt: %+v", got)
	}
	if _, err := s.CommitPipelineTransition(ctx, validate); !errors.Is(err, domain.ErrPipelineConflict) {
		t.Fatalf("a second validation start must conflict, got %v", err)
	}

	id, err := s.CreatePipelineCommandResult(ctx, domain.PipelineCommandResult{AttemptID: attempt.ID, Round: 1, Ordinal: 0, CommandID: "unit", Command: "go test", Required: true, Revision: "cafe", Status: domain.PipelineCommandRunning, StartedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	if res, _ := s.ListPipelineCommandResults(ctx, attempt.ID); len(res) != 1 || res[0].Status != domain.PipelineCommandRunning {
		t.Fatalf("a command is recorded as running before it starts: %+v", res)
	}
	// A restart finds it still running and records it unknown; it is never inferred.
	n, err := s.MarkRunningPipelineCommandsUnknown(ctx, run.ID, at.Add(time.Second), "daemon stopped")
	if err != nil || n != 1 {
		t.Fatalf("mark unknown: n=%d err=%v", n, err)
	}
	res, _ := s.ListPipelineCommandResults(ctx, attempt.ID)
	if res[0].Status != domain.PipelineCommandUnknown || res[0].Detail != "daemon stopped" || res[0].FinishedAt == nil {
		t.Fatalf("result: %+v", res[0])
	}
	if n, _ := s.MarkRunningPipelineCommandsUnknown(ctx, run.ID, at, "again"); n != 0 {
		t.Fatal("settled commands are never re-marked")
	}
	done := at.Add(time.Minute)
	if err := s.FinishPipelineCommandResult(ctx, id, domain.PipelineCommandResult{Status: domain.PipelineCommandPassed, ExitCode: 0, FinishedAt: &done, Log: "ok", LogTruncated: true}); err != nil {
		t.Fatal(err)
	}
	res, _ = s.ListPipelineCommandResults(ctx, attempt.ID)
	if res[0].Status != domain.PipelineCommandPassed || res[0].Log != "ok" || !res[0].LogTruncated || !res[0].Required {
		t.Fatalf("finished: %+v", res[0])
	}
	// A validating attempt can still be finished by the validator.
	if _, err := s.CommitPipelineTransition(ctx, domain.PipelineTransition{
		RunID: run.ID, ExpectedRevision: created.Revision, At: done,
		Attempt: &domain.PipelineAttemptFinish{ID: attempt.ID, State: domain.PipelineAttemptAccepted, OutputCommit: "cafe", Outcome: "succeeded", ResultKey: "k", FinishedAt: done},
	}); err != nil {
		t.Fatalf("validating attempts must be finishable: %v", err)
	}
}

func TestProjectPipelineCommandTrustPreservesOtherSettings(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.UpsertProject(ctx, domain.ProjectRecord{ID: "trust", Path: "/tmp/trust", RegisteredAt: time.Now().UTC().Truncate(time.Second), Config: domain.ProjectConfig{AgentRules: "keep me"}}); err != nil {
		t.Fatal(err)
	}
	row, ok, err := s.SetProjectPipelineCommandTrust(ctx, "trust", true)
	if err != nil || !ok || !row.Config.TrustPipelineCommands || row.Config.AgentRules != "keep me" {
		t.Fatalf("grant: %+v ok=%v err=%v", row.Config, ok, err)
	}
	got, _, _ := s.GetProject(ctx, "trust")
	if !got.Config.TrustPipelineCommands || got.Config.AgentRules != "keep me" {
		t.Fatalf("persisted: %+v", got.Config)
	}
	if _, ok, _ := s.SetProjectPipelineCommandTrust(ctx, "ghost", true); ok {
		t.Fatal("unknown project")
	}
}

func TestPipelineRepairIsCountedExactlyOncePerSourceAttempt(t *testing.T) {
	ctx := context.Background()
	s, sid := seedPipelineSession(t, "pr6")
	at := time.Now().UTC().Truncate(time.Second)
	run, attempt := newRun("run-1", sid, "pr6", at)
	created, _, err := s.CreatePipelineRun(ctx, run, attempt)
	if err != nil {
		t.Fatal(err)
	}
	repair := func(id string, ordinal int, source string, rev int64) domain.PipelineTransition {
		return domain.PipelineTransition{
			RunID: run.ID, ExpectedRevision: rev, At: at,
			Run:    &domain.PipelineRunUpdate{State: domain.PipelineRunRunning, CurrentStageID: "build", RepairsDelta: 1},
			Repair: &domain.PipelineRepair{ID: id, Ordinal: ordinal, SourceAttemptID: source, SourceStageID: "test", Kind: domain.PipelineRepairProductionDefect, TargetStageID: "build", ReturnStageID: "test"},
			NewAttempt: &domain.PipelineStageAttempt{
				ID: id + "-build", RunID: run.ID, StageID: "build", StageKind: "build", AttemptNo: ordinal + 1, State: domain.PipelineAttemptHandoff,
				InputCommit: "abc", StartedAt: at, RepairSourceAttemptID: source, ReturnStageID: "test", FeedbackJSON: `{"summary":"x"}`,
			},
		}
	}
	updated, err := s.CommitPipelineTransition(ctx, repair("rep-1", 1, "src-attempt", created.Revision))
	if err != nil || updated.RepairsUsed != 1 {
		t.Fatalf("first repair: %+v err=%v", updated, err)
	}
	// The same failing attempt cannot be counted again, even with a fresh revision.
	if _, err := s.CommitPipelineTransition(ctx, repair("rep-dup", 2, "src-attempt", updated.Revision)); !errors.Is(err, domain.ErrPipelineConflict) {
		t.Fatalf("a duplicate source attempt must lose, got %v", err)
	}
	got, _, _ := s.GetPipelineRun(ctx, run.ID)
	if got.RepairsUsed != 1 {
		t.Fatalf("a rejected duplicate must roll back its budget spend: %d", got.RepairsUsed)
	}
	if _, ok, _ := s.GetPipelineStageAttempt(ctx, "rep-dup-build"); ok {
		t.Fatal("a rejected duplicate must not leave a Build attempt behind")
	}
	repairs, _ := s.ListPipelineRepairs(ctx, run.ID)
	if len(repairs) != 1 || repairs[0].Ordinal != 1 || repairs[0].SourceAttemptID != "src-attempt" || repairs[0].ReturnStageID != "test" {
		t.Fatalf("repairs: %+v", repairs)
	}
	atts, _ := s.ListPipelineStageAttempts(ctx, run.ID)
	last := atts[len(atts)-1]
	if last.RepairSourceAttemptID != "src-attempt" || last.ReturnStageID != "test" || last.FeedbackJSON == "" {
		t.Fatalf("the repair attempt keeps its source, return stage and feedback: %+v", last)
	}
}

func TestPipelineReviewLinkIsBoundToOneHead(t *testing.T) {
	ctx := context.Background()
	s, sid := seedPipelineSession(t, "prl")
	at := time.Now().UTC().Truncate(time.Second)
	run, attempt := newRun("run-l", sid, "prl", at)
	if _, _, err := s.CreatePipelineRun(ctx, run, attempt); err != nil {
		t.Fatal(err)
	}
	link := domain.PipelineReviewLink{AttemptID: attempt.ID, RunID: run.ID, PRURL: "https://x/pr/1", HeadSHA: "aaa111", LinkedAt: at, UpdatedAt: at}
	got, err := s.LinkPipelineReview(ctx, link)
	if err != nil || got.HeadSHA != "aaa111" || got.ReviewRunID != "" {
		t.Fatalf("first link = %+v, %v", got, err)
	}
	// Filling in the review run id is allowed for the same head.
	link.ReviewRunID, link.UpdatedAt = "rrun-1", at.Add(time.Second)
	got, err = s.LinkPipelineReview(ctx, link)
	if err != nil || got.ReviewRunID != "rrun-1" {
		t.Fatalf("fill run id = %+v, %v", got, err)
	}
	// A different head can never replace the recorded one.
	other := link
	other.HeadSHA, other.ReviewRunID, other.UpdatedAt = "bbb222", "rrun-2", at.Add(2*time.Second)
	got, err = s.LinkPipelineReview(ctx, other)
	if err != nil || got.HeadSHA != "aaa111" || got.ReviewRunID != "rrun-1" {
		t.Fatalf("a link must not move to another head: %+v, %v", got, err)
	}
	if links, err := s.ListPipelineReviewLinks(ctx, run.ID); err != nil || len(links) != 1 {
		t.Fatalf("links = %+v, %v", links, err)
	}
	if _, ok, err := s.GetPipelineReviewLink(ctx, "missing"); err != nil || ok {
		t.Fatalf("missing link ok=%v err=%v", ok, err)
	}
}

func TestPipelineRepairGrantsAreIdempotentAndGrowTheBudgetAtomically(t *testing.T) {
	ctx := context.Background()
	s, sid := seedPipelineSession(t, "prg")
	at := time.Now().UTC().Truncate(time.Second)
	run, attempt := newRun("run-g", sid, "prg", at)
	created, _, err := s.CreatePipelineRun(ctx, run, attempt)
	if err != nil {
		t.Fatal(err)
	}
	grant := domain.PipelineRepairGrant{ID: "g1", RunID: run.ID, Amount: 2, AuthorizedBy: domain.PipelineRequestedByUser, RequestKey: "k1", Note: "more", CreatedAt: at}
	updated, err := s.CommitPipelineTransition(ctx, domain.PipelineTransition{
		RunID: run.ID, ExpectedRevision: created.Revision, At: at, Grant: &grant,
		Run: &domain.PipelineRunUpdate{State: domain.PipelineRunPaused, PauseReason: "repair_budget_exhausted", CurrentStageID: "build", BudgetDelta: 2},
	})
	if err != nil || updated.RepairBudget != 5 {
		t.Fatalf("budget = %d err=%v", updated.RepairBudget, err)
	}
	// The same request key cannot grant again, and the budget must not move.
	dup := grant
	dup.ID = "g2"
	_, err = s.CommitPipelineTransition(ctx, domain.PipelineTransition{
		RunID: run.ID, ExpectedRevision: updated.Revision, At: at, Grant: &dup,
		Run: &domain.PipelineRunUpdate{State: domain.PipelineRunPaused, PauseReason: "repair_budget_exhausted", CurrentStageID: "build", BudgetDelta: 2},
	})
	if !errors.Is(err, domain.ErrPipelineGrantDuplicate) {
		t.Fatalf("duplicate grant: %v", err)
	}
	got, _, _ := s.GetPipelineRun(ctx, run.ID)
	grants, lerr := s.ListPipelineRepairGrants(ctx, run.ID)
	if lerr != nil || got.RepairBudget != 5 || len(grants) != 1 || grants[0].Amount != 2 || grants[0].AuthorizedBy != domain.PipelineRequestedByUser {
		t.Fatalf("budget=%d grants=%+v err=%v", got.RepairBudget, grants, lerr)
	}
}

func TestAttachedSessionIsFoundByTheAttemptItWasCreatedFor(t *testing.T) {
	ctx := context.Background()
	s, owner := seedPipelineSession(t, "pra")
	at := time.Now().UTC().Truncate(time.Second)
	rec := func() domain.SessionRecord {
		r := sampleRecord("pra")
		r.CreatedAt, r.UpdatedAt = at, at
		return r
	}
	first, err := s.CreateAttachedSessionForAttempt(ctx, rec(), owner, "attempt-1")
	if err != nil {
		t.Fatal(err)
	}
	id, ok, err := s.FindAttachedSessionForAttempt(ctx, "attempt-1")
	if err != nil || !ok || id != first.ID {
		t.Fatalf("lookup by attempt: %v %v %v", id, ok, err)
	}
	if _, ok, _ := s.FindAttachedSessionForAttempt(ctx, "attempt-2"); ok {
		t.Fatal("another attempt has no session yet")
	}
	if _, ok, _ := s.FindAttachedSessionForAttempt(ctx, ""); ok {
		t.Fatal("an empty attempt id never matches")
	}
	// One session per attempt: a second creation for the same attempt must fail
	// rather than create a duplicate.
	if _, err := s.CreateAttachedSessionForAttempt(ctx, rec(), owner, "attempt-1"); err == nil {
		t.Fatal("the unique index must refuse a second session for the same attempt")
	}
	// The plain creator (no attempt) is unchanged and unindexed.
	if _, err := s.CreateAttachedSession(ctx, rec(), owner); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAttachedSession(ctx, rec(), owner); err != nil {
		t.Fatalf("sessions without an attempt never collide: %v", err)
	}
}
