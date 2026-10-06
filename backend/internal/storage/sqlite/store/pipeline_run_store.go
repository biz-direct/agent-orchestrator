package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// CreatePipelineRun atomically inserts a run and its first stage attempt. A
// session may own only one unfinished run.
func (s *Store) CreatePipelineRun(ctx context.Context, run domain.PipelineRun, first domain.PipelineStageAttempt) (domain.PipelineRun, domain.PipelineStageAttempt, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var outRun gen.PipelineRun
	var outAttempt gen.PipelineStageAttempt
	err := s.inTx(ctx, "create pipeline run", func(q *gen.Queries) error {
		var err error
		outRun, err = q.CreatePipelineRun(ctx, gen.CreatePipelineRunParams{
			ID: run.ID, SessionID: string(run.SessionID), ProjectID: string(run.ProjectID),
			WorkflowID: run.WorkflowID, State: string(run.State), CurrentStageID: run.CurrentStageID,
			RequestedBy: string(run.RequestedBy), ExpectedBranch: run.ExpectedBranch,
			RepairBudget: int64(run.RepairBudget), Snapshot: run.Snapshot, SnapshotSha256: run.SnapshotSHA256,
			CreatedAt: run.CreatedAt, UpdatedAt: run.UpdatedAt,
		})
		if err != nil {
			return err
		}
		outAttempt, err = q.CreatePipelineStageAttempt(ctx, gen.CreatePipelineStageAttemptParams{
			ID: first.ID, RunID: first.RunID, StageID: first.StageID, StageKind: first.StageKind,
			AttemptNo: int64(first.AttemptNo), ExecutorSessionID: string(first.ExecutorSessionID),
			ControllerGeneration: first.ControllerGeneration, InputCommit: first.InputCommit,
			StartedAt: first.StartedAt,
		})
		return err
	})
	if err != nil {
		if strings.Contains(err.Error(), "idx_pipeline_runs_active_session") || strings.Contains(err.Error(), "pipeline_runs.session_id") {
			return domain.PipelineRun{}, domain.PipelineStageAttempt{}, domain.ErrPipelineRunActive
		}
		return domain.PipelineRun{}, domain.PipelineStageAttempt{}, fmt.Errorf("create pipeline run: %w", err)
	}
	return pipelineRunFromGen(outRun), pipelineAttemptFromGen(outAttempt), nil
}

// GetPipelineRun returns one run by id.
func (s *Store) GetPipelineRun(ctx context.Context, id string) (domain.PipelineRun, bool, error) {
	row, err := s.qr.GetPipelineRun(ctx, id)
	return optionalPipelineRun(row, err, "get pipeline run")
}

// GetActivePipelineRunBySession returns the session's unfinished run, if any.
func (s *Store) GetActivePipelineRunBySession(ctx context.Context, id domain.SessionID) (domain.PipelineRun, bool, error) {
	row, err := s.qr.GetActivePipelineRunBySession(ctx, string(id))
	return optionalPipelineRun(row, err, "get active pipeline run")
}

// GetLatestPipelineRunBySession returns the session's newest run, finished or not.
func (s *Store) GetLatestPipelineRunBySession(ctx context.Context, id domain.SessionID) (domain.PipelineRun, bool, error) {
	row, err := s.qr.GetLatestPipelineRunBySession(ctx, string(id))
	return optionalPipelineRun(row, err, "get latest pipeline run")
}

// ListUnfinishedPipelineRuns returns every running or paused run.
func (s *Store) ListUnfinishedPipelineRuns(ctx context.Context) ([]domain.PipelineRun, error) {
	rows, err := s.qr.ListUnfinishedPipelineRuns(ctx)
	if err != nil {
		return nil, fmt.Errorf("list unfinished pipeline runs: %w", err)
	}
	out := make([]domain.PipelineRun, 0, len(rows))
	for _, row := range rows {
		out = append(out, pipelineRunFromGen(row))
	}
	return out, nil
}

// HasUnfinishedPipelineRun reports whether the session owns a running or
// paused run. Lifecycle shortcuts consult it to stay out of the way.
func (s *Store) HasUnfinishedPipelineRun(ctx context.Context, id domain.SessionID) (bool, error) {
	has, err := s.qr.HasUnfinishedPipelineRun(ctx, string(id))
	if err != nil {
		return false, fmt.Errorf("check pipeline run: %w", err)
	}
	return has, nil
}

// ListPipelineStageAttempts returns a run's attempts in start order.
func (s *Store) ListPipelineStageAttempts(ctx context.Context, runID string) ([]domain.PipelineStageAttempt, error) {
	rows, err := s.qr.ListPipelineStageAttempts(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list pipeline attempts: %w", err)
	}
	out := make([]domain.PipelineStageAttempt, 0, len(rows))
	for _, row := range rows {
		out = append(out, pipelineAttemptFromGen(row))
	}
	return out, nil
}

// GetPipelineStageAttempt returns one attempt by id.
func (s *Store) GetPipelineStageAttempt(ctx context.Context, id string) (domain.PipelineStageAttempt, bool, error) {
	row, err := s.qr.GetPipelineStageAttempt(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.PipelineStageAttempt{}, false, nil
	}
	if err != nil {
		return domain.PipelineStageAttempt{}, false, fmt.Errorf("get pipeline attempt: %w", err)
	}
	return pipelineAttemptFromGen(row), true, nil
}

// ListPipelineEvents returns the newest events first, bounded by limit.
func (s *Store) ListPipelineEvents(ctx context.Context, runID string, limit int) ([]domain.PipelineEvent, error) {
	rows, err := s.qr.ListPipelineEvents(ctx, gen.ListPipelineEventsParams{RunID: runID, Limit: int64(limit)})
	if err != nil {
		return nil, fmt.Errorf("list pipeline events: %w", err)
	}
	out := make([]domain.PipelineEvent, 0, len(rows))
	for _, row := range rows {
		out = append(out, domain.PipelineEvent{ID: row.ID, RunID: row.RunID, AttemptID: row.AttemptID, Kind: row.Kind, Detail: row.Detail, CreatedAt: row.CreatedAt})
	}
	return out, nil
}

// AddPipelineEvent appends an execution fact without changing run state.
func (s *Store) AddPipelineEvent(ctx context.Context, ev domain.PipelineEvent) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "add pipeline event", func(q *gen.Queries) error {
		return q.CreatePipelineEvent(ctx, gen.CreatePipelineEventParams{
			RunID: ev.RunID, AttemptID: ev.AttemptID, Kind: ev.Kind, Detail: detailOrEmpty(ev.Detail), CreatedAt: ev.CreatedAt,
		})
	})
}

// SetPipelineAttemptInstructionDelivery records whether the stage instructions
// reached the executor.
func (s *Store) SetPipelineAttemptInstructionDelivery(ctx context.Context, attemptID, state string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "set pipeline instruction delivery", func(q *gen.Queries) error {
		return q.SetPipelineAttemptInstructionDelivery(ctx, gen.SetPipelineAttemptInstructionDeliveryParams{InstructionDelivery: state, ID: attemptID})
	})
}

// CommitPipelineTransition applies one atomic run/attempt/event change. It
// returns ErrPipelineConflict when the run's revision moved or the attempt is
// no longer active, so a duplicate or stale writer can never double-apply.
func (s *Store) CommitPipelineTransition(ctx context.Context, t domain.PipelineTransition) (domain.PipelineRun, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var out gen.PipelineRun
	err := s.inTx(ctx, "commit pipeline transition", func(q *gen.Queries) error {
		if t.Attempt != nil {
			_, err := q.FinishPipelineStageAttempt(ctx, gen.FinishPipelineStageAttemptParams{
				State: string(t.Attempt.State), OutputCommit: t.Attempt.OutputCommit, NoChange: boolInt(t.Attempt.NoChange),
				Outcome: t.Attempt.Outcome, Summary: t.Attempt.Summary, ResultKey: t.Attempt.ResultKey,
				FinishedAt: nullTime(t.Attempt.FinishedAt), ID: t.Attempt.ID,
			})
			if errors.Is(err, sql.ErrNoRows) {
				return domain.ErrPipelineConflict
			}
			if err != nil {
				return err
			}
		}
		if t.Run != nil {
			var completed sql.NullTime
			if t.Run.CompletedAt != nil {
				completed = sql.NullTime{Time: *t.Run.CompletedAt, Valid: true}
			}
			row, err := q.SetPipelineRunState(ctx, gen.SetPipelineRunStateParams{
				State: string(t.Run.State), PauseReason: string(t.Run.PauseReason), PauseDetail: t.Run.PauseDetail,
				CurrentStageID: t.Run.CurrentStageID, UpdatedAt: t.At, CompletedAt: completed,
				ID: t.RunID, ExpectedRevision: t.ExpectedRevision,
			})
			if errors.Is(err, sql.ErrNoRows) {
				return domain.ErrPipelineConflict
			}
			if err != nil {
				return err
			}
			out = row
		}
		for _, ev := range t.Events {
			if err := q.CreatePipelineEvent(ctx, gen.CreatePipelineEventParams{
				RunID: t.RunID, AttemptID: ev.AttemptID, Kind: ev.Kind, Detail: detailOrEmpty(ev.Detail), CreatedAt: t.At,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, domain.ErrPipelineConflict) {
			return domain.PipelineRun{}, err
		}
		return domain.PipelineRun{}, fmt.Errorf("commit pipeline transition: %w", err)
	}
	return pipelineRunFromGen(out), nil
}

func detailOrEmpty(s string) string {
	if s == "" {
		return "{}"
	}
	return s
}

func optionalPipelineRun(row gen.PipelineRun, err error, what string) (domain.PipelineRun, bool, error) {
	if errors.Is(err, sql.ErrNoRows) {
		return domain.PipelineRun{}, false, nil
	}
	if err != nil {
		return domain.PipelineRun{}, false, fmt.Errorf("%s: %w", what, err)
	}
	return pipelineRunFromGen(row), true, nil
}

func pipelineRunFromGen(r gen.PipelineRun) domain.PipelineRun {
	out := domain.PipelineRun{
		ID: r.ID, SessionID: domain.SessionID(r.SessionID), ProjectID: domain.ProjectID(r.ProjectID),
		WorkflowID: r.WorkflowID, State: domain.PipelineRunState(r.State),
		PauseReason: domain.PipelinePauseReason(r.PauseReason), PauseDetail: r.PauseDetail,
		CurrentStageID: r.CurrentStageID, RequestedBy: domain.PipelineRequester(r.RequestedBy),
		ExpectedBranch: r.ExpectedBranch, RepairBudget: int(r.RepairBudget), RepairsUsed: int(r.RepairsUsed),
		Snapshot: r.Snapshot, SnapshotSHA256: r.SnapshotSha256, Revision: r.Revision,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
	if r.CompletedAt.Valid {
		t := r.CompletedAt.Time
		out.CompletedAt = &t
	}
	return out
}

func pipelineAttemptFromGen(a gen.PipelineStageAttempt) domain.PipelineStageAttempt {
	out := domain.PipelineStageAttempt{
		ID: a.ID, RunID: a.RunID, StageID: a.StageID, StageKind: a.StageKind, AttemptNo: int(a.AttemptNo),
		State: domain.PipelineAttemptState(a.State), ExecutorSessionID: domain.SessionID(a.ExecutorSessionID),
		ControllerGeneration: a.ControllerGeneration, InputCommit: a.InputCommit, OutputCommit: a.OutputCommit,
		NoChange: a.NoChange != 0, Outcome: a.Outcome, Summary: a.Summary, ResultKey: a.ResultKey,
		InstructionDelivery: a.InstructionDelivery, StartedAt: a.StartedAt,
	}
	if a.FinishedAt.Valid {
		t := a.FinishedAt.Time
		out.FinishedAt = &t
	}
	return out
}
