package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

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
			AttemptNo: int64(first.AttemptNo), State: attemptStateOrActive(first.State), ExecutorSessionID: string(first.ExecutorSessionID),
			ControllerGeneration: first.ControllerGeneration, InputCommit: first.InputCommit,
			StartedAt: first.StartedAt, PredecessorAttemptID: first.PredecessorAttemptID,
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
				Outcome: t.Attempt.Outcome, Summary: t.Attempt.Summary, ResultKey: t.Attempt.ResultKey, ResultJson: t.Attempt.ResultJSON, FeedbackJson: t.Attempt.FeedbackJSON,
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
				RepairsDelta: int64(t.Run.RepairsDelta), BudgetDelta: int64(t.Run.BudgetDelta), ID: t.RunID, ExpectedRevision: t.ExpectedRevision,
			})
			if errors.Is(err, sql.ErrNoRows) {
				return domain.ErrPipelineConflict
			}
			if err != nil {
				return err
			}
			out = row
		}
		if t.Validate != nil {
			v := t.Validate
			_, err := q.StartPipelineAttemptValidation(ctx, gen.StartPipelineAttemptValidationParams{
				OutputCommit: v.OutputCommit, NoChange: boolInt(v.NoChange), Outcome: v.Outcome, Summary: v.Summary,
				ResultKey: v.ResultKey, ResultJson: v.ResultJSON, ID: v.ID,
			})
			if errors.Is(err, sql.ErrNoRows) {
				return domain.ErrPipelineConflict
			}
			if err != nil {
				return err
			}
		}
		if t.NewAttempt != nil {
			na := t.NewAttempt
			if _, err := q.CreatePipelineStageAttempt(ctx, gen.CreatePipelineStageAttemptParams{
				ID: na.ID, RunID: na.RunID, StageID: na.StageID, StageKind: na.StageKind, AttemptNo: int64(na.AttemptNo),
				State: attemptStateOrActive(na.State), ExecutorSessionID: string(na.ExecutorSessionID),
				ControllerGeneration: na.ControllerGeneration, InputCommit: na.InputCommit, StartedAt: na.StartedAt,
				PredecessorAttemptID: na.PredecessorAttemptID, RepairSourceAttemptID: na.RepairSourceAttemptID,
				ReturnStageID: na.ReturnStageID, FeedbackJson: na.FeedbackJSON, RetryOfAttemptID: na.RetryOfAttemptID,
			}); err != nil {
				return err
			}
		}
		if t.Activate != nil {
			_, err := q.ActivatePipelineStageAttempt(ctx, gen.ActivatePipelineStageAttemptParams{
				ExecutorSessionID: string(t.Activate.ExecutorSessionID), ControllerGeneration: t.Activate.ControllerGeneration,
				StartedAt: t.Activate.At, ID: t.Activate.ID,
			})
			if errors.Is(err, sql.ErrNoRows) {
				return domain.ErrPipelineConflict
			}
			if err != nil {
				return err
			}
		}
		if t.Repair != nil {
			rp := t.Repair
			if err := q.CreatePipelineRepair(ctx, gen.CreatePipelineRepairParams{
				ID: rp.ID, RunID: t.RunID, Ordinal: int64(rp.Ordinal), SourceAttemptID: rp.SourceAttemptID,
				SourceStageID: rp.SourceStageID, Kind: string(rp.Kind), TargetStageID: rp.TargetStageID,
				ReturnStageID: rp.ReturnStageID, CreatedAt: t.At,
			}); err != nil {
				// A repeated source attempt or ordinal means this return was
				// already counted: the second writer must lose, not double-spend.
				if strings.Contains(err.Error(), "UNIQUE constraint failed") {
					return domain.ErrPipelineConflict
				}
				return err
			}
		}
		if t.Grant != nil {
			g := t.Grant
			if err := q.CreatePipelineRepairGrant(ctx, gen.CreatePipelineRepairGrantParams{
				ID: g.ID, RunID: t.RunID, Amount: int64(g.Amount), AuthorizedBy: string(g.AuthorizedBy),
				RequestKey: g.RequestKey, Note: g.Note, CreatedAt: t.At,
			}); err != nil {
				if strings.Contains(err.Error(), "UNIQUE constraint failed") {
					return domain.ErrPipelineGrantDuplicate
				}
				return err
			}
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
		if errors.Is(err, domain.ErrPipelineConflict) || errors.Is(err, domain.ErrPipelineGrantDuplicate) {
			return domain.PipelineRun{}, err
		}
		return domain.PipelineRun{}, fmt.Errorf("commit pipeline transition: %w", err)
	}
	return pipelineRunFromGen(out), nil
}

func attemptStateOrActive(s domain.PipelineAttemptState) string {
	if s == "" {
		return string(domain.PipelineAttemptActive)
	}
	return string(s)
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
		PredecessorAttemptID: a.PredecessorAttemptID, ResultJSON: a.ResultJson,
		RepairSourceAttemptID: a.RepairSourceAttemptID, ReturnStageID: a.ReturnStageID, FeedbackJSON: a.FeedbackJson, RetryOfAttemptID: a.RetryOfAttemptID,
	}
	if a.FinishedAt.Valid {
		t := a.FinishedAt.Time
		out.FinishedAt = &t
	}
	return out
}

// CreateAttachedSession atomically inserts a hidden session row that executes a
// pipeline stage for ownerID. Because the attached marker is written in the
// same transaction, no listing can ever observe the row as an ordinary session.
func (s *Store) CreateAttachedSession(ctx context.Context, rec domain.SessionRecord, ownerID domain.SessionID) (domain.SessionRecord, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	err := s.inTx(ctx, "create attached session", func(q *gen.Queries) error {
		num, err := q.NextSessionNum(ctx, optionalProjectID(rec.ProjectID))
		if err != nil {
			return fmt.Errorf("next session num for %s: %w", rec.ProjectID, err)
		}
		for {
			rec.ID = domain.SessionID(fmt.Sprintf("%s-%d", rec.ProjectID, num))
			exists, err := q.SessionIDExists(ctx, rec.ID)
			if err != nil {
				return fmt.Errorf("check session id %s: %w", rec.ID, err)
			}
			if !exists {
				break
			}
			num++
		}
		if err := q.InsertSession(ctx, recordToInsert(rec, num)); err != nil {
			return fmt.Errorf("insert attached session %s: %w", rec.ID, err)
		}
		return q.SetSessionAttachedTo(ctx, gen.SetSessionAttachedToParams{AttachedToSessionID: string(ownerID), ID: rec.ID})
	})
	if err != nil {
		return domain.SessionRecord{}, err
	}
	return rec, nil
}

// GetSessionAttachedTo returns the owner worker of an attached specialist
// session, or "" for an ordinary session.
func (s *Store) GetSessionAttachedTo(ctx context.Context, id domain.SessionID) (domain.SessionID, error) {
	owner, err := s.qr.GetSessionAttachedTo(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get attached owner %s: %w", id, err)
	}
	return domain.SessionID(owner), nil
}

// ListAttachedSessionIDs returns the attached specialist sessions of a worker.
func (s *Store) ListAttachedSessionIDs(ctx context.Context, owner domain.SessionID) ([]domain.SessionID, error) {
	ids, err := s.qr.ListAttachedSessionIDs(ctx, string(owner))
	if err != nil {
		return nil, fmt.Errorf("list attached sessions: %w", err)
	}
	return append(make([]domain.SessionID, 0, len(ids)), ids...), nil
}

// CreatePipelineCommandResult records a command as running before its process
// starts, so a crash leaves evidence that it may have executed.
func (s *Store) CreatePipelineCommandResult(ctx context.Context, r domain.PipelineCommandResult) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var id int64
	err := s.inTx(ctx, "create pipeline command result", func(q *gen.Queries) error {
		row, err := q.CreatePipelineCommandResult(ctx, gen.CreatePipelineCommandResultParams{
			AttemptID: r.AttemptID, Round: int64(r.Round), Ordinal: int64(r.Ordinal), CommandID: r.CommandID, Command: r.Command,
			Required: boolInt(r.Required), Revision: r.Revision, Status: string(r.Status), StartedAt: r.StartedAt,
		})
		id = row.ID
		return err
	})
	return id, err
}

// FinishPipelineCommandResult settles a command's evidence.
func (s *Store) FinishPipelineCommandResult(ctx context.Context, id int64, r domain.PipelineCommandResult) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "finish pipeline command result", func(q *gen.Queries) error {
		return q.FinishPipelineCommandResult(ctx, gen.FinishPipelineCommandResultParams{
			Status: string(r.Status), ExitCode: int64(r.ExitCode), FinishedAt: nullTime(derefTime(r.FinishedAt)),
			Log: r.Log, LogTruncated: boolInt(r.LogTruncated), Detail: r.Detail, ID: id,
		})
	})
}

// ListPipelineCommandResults returns an attempt's command evidence in order.
func (s *Store) ListPipelineCommandResults(ctx context.Context, attemptID string) ([]domain.PipelineCommandResult, error) {
	rows, err := s.qr.ListPipelineCommandResults(ctx, attemptID)
	if err != nil {
		return nil, fmt.Errorf("list pipeline command results: %w", err)
	}
	out := make([]domain.PipelineCommandResult, 0, len(rows))
	for _, r := range rows {
		res := domain.PipelineCommandResult{
			ID: r.ID, AttemptID: r.AttemptID, Round: int(r.Round), Ordinal: int(r.Ordinal), CommandID: r.CommandID,
			Command: r.Command, Required: r.Required != 0, Revision: r.Revision, Status: domain.PipelineCommandStatus(r.Status),
			ExitCode: int(r.ExitCode), StartedAt: r.StartedAt, Log: r.Log, LogTruncated: r.LogTruncated != 0, Detail: r.Detail,
		}
		if r.FinishedAt.Valid {
			t := r.FinishedAt.Time
			res.FinishedAt = &t
		}
		out = append(out, res)
	}
	return out, nil
}

// MarkRunningPipelineCommandsUnknown reconciles commands that were running when
// the daemon stopped. Their outcome is genuinely unknown: it is recorded as
// such and never inferred passed, failed, or retried.
func (s *Store) MarkRunningPipelineCommandsUnknown(ctx context.Context, runID string, at time.Time, detail string) (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n := 0
	err := s.inTx(ctx, "mark running pipeline commands unknown", func(q *gen.Queries) error {
		rows, err := q.ListRunningPipelineCommandResults(ctx, runID)
		if err != nil {
			return err
		}
		for _, r := range rows {
			if err := q.MarkPipelineCommandResultUnknown(ctx, gen.MarkPipelineCommandResultUnknownParams{
				FinishedAt: sql.NullTime{Time: at, Valid: true}, Detail: detail, ID: r.ID,
			}); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// ListPipelineRepairs returns a run's counted returns to Build in order.
func (s *Store) ListPipelineRepairs(ctx context.Context, runID string) ([]domain.PipelineRepair, error) {
	rows, err := s.qr.ListPipelineRepairs(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list pipeline repairs: %w", err)
	}
	out := make([]domain.PipelineRepair, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.PipelineRepair{
			ID: r.ID, RunID: r.RunID, Ordinal: int(r.Ordinal), SourceAttemptID: r.SourceAttemptID, SourceStageID: r.SourceStageID,
			Kind: domain.PipelineRepairKind(r.Kind), TargetStageID: r.TargetStageID, ReturnStageID: r.ReturnStageID, CreatedAt: r.CreatedAt,
		})
	}
	return out, nil
}

// LinkPipelineReview records which pull request head a Review attempt evaluates
// and, when known, the AO review run for it. It is idempotent: a link can gain a
// review run id but never moves to another pull request or head, and the stored
// link is returned so callers can detect a conflicting head.
func (s *Store) LinkPipelineReview(ctx context.Context, l domain.PipelineReviewLink) (domain.PipelineReviewLink, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var out gen.PipelineReviewLink
	err := s.inTx(ctx, "link pipeline review", func(q *gen.Queries) error {
		if err := q.UpsertPipelineReviewLink(ctx, gen.UpsertPipelineReviewLinkParams{
			AttemptID: l.AttemptID, RunID: l.RunID, PRURL: l.PRURL, HeadSha: l.HeadSHA,
			ReviewRunID: l.ReviewRunID, LinkedAt: l.LinkedAt, UpdatedAt: l.UpdatedAt,
		}); err != nil {
			return err
		}
		var err error
		out, err = q.GetPipelineReviewLink(ctx, l.AttemptID)
		return err
	})
	if err != nil {
		return domain.PipelineReviewLink{}, fmt.Errorf("link pipeline review: %w", err)
	}
	return reviewLinkFromRow(out), nil
}

// GetPipelineReviewLink returns the link for a Review attempt, if any.
func (s *Store) GetPipelineReviewLink(ctx context.Context, attemptID string) (domain.PipelineReviewLink, bool, error) {
	row, err := s.qr.GetPipelineReviewLink(ctx, attemptID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.PipelineReviewLink{}, false, nil
	}
	if err != nil {
		return domain.PipelineReviewLink{}, false, fmt.Errorf("get pipeline review link: %w", err)
	}
	return reviewLinkFromRow(row), true, nil
}

// ListPipelineReviewLinks returns a run's Review links in link order.
func (s *Store) ListPipelineReviewLinks(ctx context.Context, runID string) ([]domain.PipelineReviewLink, error) {
	rows, err := s.qr.ListPipelineReviewLinks(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list pipeline review links: %w", err)
	}
	out := make([]domain.PipelineReviewLink, 0, len(rows))
	for _, r := range rows {
		out = append(out, reviewLinkFromRow(r))
	}
	return out, nil
}

func reviewLinkFromRow(r gen.PipelineReviewLink) domain.PipelineReviewLink {
	return domain.PipelineReviewLink{
		AttemptID: r.AttemptID, RunID: r.RunID, PRURL: r.PRURL, HeadSHA: r.HeadSha,
		ReviewRunID: r.ReviewRunID, LinkedAt: r.LinkedAt, UpdatedAt: r.UpdatedAt,
	}
}

// ListPipelineRepairGrants returns a run's human authorizations of extra repairs.
func (s *Store) ListPipelineRepairGrants(ctx context.Context, runID string) ([]domain.PipelineRepairGrant, error) {
	rows, err := s.qr.ListPipelineRepairGrants(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list pipeline repair grants: %w", err)
	}
	out := make([]domain.PipelineRepairGrant, 0, len(rows))
	for _, r := range rows {
		out = append(out, domain.PipelineRepairGrant{
			ID: r.ID, RunID: r.RunID, Amount: int(r.Amount), AuthorizedBy: domain.PipelineRequester(r.AuthorizedBy),
			RequestKey: r.RequestKey, Note: r.Note, CreatedAt: r.CreatedAt,
		})
	}
	return out, nil
}
