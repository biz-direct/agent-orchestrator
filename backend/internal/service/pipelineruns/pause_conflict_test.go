package pipelineruns_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/pipelineruns"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/store"
)

// racingStore lets another writer decide the run right after Submit read it,
// so Submit's own pause loses its compare-and-set.
type racingStore struct {
	*store.Store
	onGetSession func()
}

func (s *racingStore) GetSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	if hook := s.onGetSession; hook != nil {
		s.onGetSession = nil
		hook()
	}
	return s.Store.GetSession(ctx, id)
}

// A pause that loses its compare-and-set has not paused the run; Submit must not
// answer "the run is paused" when another writer (here, a cancel) won.
func TestSubmitDoesNotReportPausedWhenItsPauseLosesTheRace(t *testing.T) {
	f := newFixture(t, map[string]string{".ao/pipelines/workflows/build-only.yaml": buildOnlyWorkflow})
	run, err := f.start("user", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sess, _, _ := f.store.GetSession(ctx, f.sessionID)
	sess.Metadata.ControllerGeneration = "gen-2" // the controller restarted: Submit will try to pause
	if err := f.store.UpdateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	racing := &racingStore{Store: f.store}
	racing.onGetSession = func() {
		current, ok, _ := f.store.GetActivePipelineRunBySession(ctx, f.sessionID)
		if !ok {
			t.Error("the run should still be active")
			return
		}
		now := time.Now().UTC()
		if _, err := f.store.CommitPipelineTransition(ctx, domain.PipelineTransition{
			RunID: current.ID, ExpectedRevision: current.Revision, At: now,
			Run:     &domain.PipelineRunUpdate{State: domain.PipelineRunCancelled, CurrentStageID: current.CurrentStageID, CompletedAt: &now},
			Attempt: &domain.PipelineAttemptFinish{ID: run.Attempts[0].ID, State: domain.PipelineAttemptCancelled, Outcome: "cancelled", FinishedAt: now},
		}); err != nil {
			t.Errorf("cancel by the competing writer: %v", err)
		}
	}
	svc := pipelineruns.New(pipelineruns.Deps{Store: racing, Messenger: f.messenger})

	head := git(t, f.repo, "rev-parse", "HEAD")
	att := run.Attempts[0]
	_, err = svc.Submit(ctx, pipelineruns.SubmitInput{
		SessionID: f.sessionID, RunID: run.ID, AttemptID: att.ID, ControllerGeneration: att.ControllerGeneration,
		IdempotencyKey: "k1", Outcome: "succeeded", ExpectedInputCommit: att.InputCommit, OutputCommit: head, Summary: "done",
	})
	if got := code(t, err); got == "PIPELINE_RUN_PAUSED" || got == "" {
		t.Fatalf("a lost pause race must not be reported as a pause: %v", err)
	}
	final, _, _ := f.store.GetPipelineRun(ctx, run.ID)
	if final.State != domain.PipelineRunCancelled {
		t.Fatalf("the competing cancel stands: %s", final.State)
	}
}
