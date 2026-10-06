package pipelineruns

import (
	"context"
	"errors"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/pipeline"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Review stage execution facts recorded in pipeline_events.
const (
	eventReviewTriggered = "review_triggered"
	eventReviewWaiting   = "review_waiting"
	eventReviewLinked    = "review_linked"

	eventReviewChangesRequested = "review_changes_requested"
)

// activeReviewAttempt returns the executing Review attempt of the run's current
// stage, if there is one.
func activeReviewAttempt(run domain.PipelineRun, attempts []domain.PipelineStageAttempt) *domain.PipelineStageAttempt {
	for i := range attempts {
		a := &attempts[i]
		if a.State == domain.PipelineAttemptActive && a.StageKind == string(pipeline.StageReview) && a.StageID == run.CurrentStageID {
			return a
		}
	}
	return nil
}

// driveReviewAttempt advances one active Review attempt. Review has no executor
// of its own: it is AO's built-in reviewer plus facts the daemon reads, so each
// pass re-derives the decision from durable state instead of trusting anything
// remembered. It returns nil whenever the right move is simply to wait.
func (s *Service) driveReviewAttempt(ctx context.Context, runID string) error {
	run, ok, err := s.store.GetPipelineRun(ctx, runID)
	if err != nil || !ok || run.State != domain.PipelineRunRunning {
		return err
	}
	unlock := s.lock(run.SessionID)
	defer unlock()
	// Re-read under the task lock: a pause, cancel, or submit may have landed.
	run, ok, err = s.store.GetPipelineRun(ctx, runID)
	if err != nil || !ok || run.State != domain.PipelineRunRunning {
		return err
	}
	attempts, err := s.store.ListPipelineStageAttempts(ctx, runID)
	if err != nil {
		return err
	}
	attemptPtr := activeReviewAttempt(run, attempts)
	if attemptPtr == nil {
		return nil
	}
	attempt := *attemptPtr
	snap, err := parseSnapshot(run.Snapshot)
	if err != nil {
		return err
	}
	owner, ok, err := s.store.GetSession(ctx, run.SessionID)
	if err != nil {
		return err
	}
	if !ok || owner.IsTerminated {
		return s.pause(ctx, run, &attempt, domain.PipelinePauseSessionTerminated, "The task ended while its review was being evaluated")
	}
	if s.reviews == nil {
		return s.pause(ctx, run, &attempt, PauseStageUnsupported, "This build cannot evaluate Review stages")
	}

	dec, link, err := s.assessReview(ctx, run, attempt, owner)
	if err != nil {
		return err
	}

	// Bind the attempt to the exact head (and review run) the decision is about
	// before acting on it, so the evidence survives a restart.
	if dec.Linked && dec.PR != nil {
		runID := ""
		if dec.Run != nil {
			runID = dec.Run.ID
		}
		if link == nil || (runID != "" && link.ReviewRunID != runID) {
			now := s.clock()
			stored, lerr := s.store.LinkPipelineReview(ctx, domain.PipelineReviewLink{
				AttemptID: attempt.ID, RunID: run.ID, PRURL: dec.PR.URL, HeadSHA: dec.PR.HeadSHA, ReviewRunID: runID, LinkedAt: now, UpdatedAt: now,
			})
			if lerr != nil {
				return lerr
			}
			if link == nil {
				_ = s.store.AddPipelineEvent(ctx, domain.PipelineEvent{RunID: run.ID, AttemptID: attempt.ID, Kind: eventReviewLinked, CreatedAt: now, Detail: eventDetail{Message: fmt.Sprintf("Review is bound to %s at head %s", dec.PR.URL, shortCommit(stored.HeadSHA))}.marshal()})
			}
		}
	}

	switch dec.Kind {
	case gateTrigger:
		return s.triggerReview(ctx, run, attempt, dec)
	case gateWait:
		s.noteWaiting(ctx, run, attempt, dec)
		return nil
	case gatePause:
		if dec.FinishFailed {
			return s.routeReviewFeedback(ctx, run, snap, attempt, dec)
		}
		return s.pause(ctx, run, &attempt, dec.Pause, dec.Detail)
	case gateComplete:
		now := s.clock()
		err := s.commitAcceptance(ctx, run, attempt, snap, &domain.PipelineAttemptFinish{
			ID: attempt.ID, State: domain.PipelineAttemptAccepted, OutputCommit: attempt.InputCommit, NoChange: true,
			Outcome: OutcomeSucceeded, Summary: dec.Detail, FinishedAt: now,
		})
		if errors.Is(err, domain.ErrPipelineConflict) {
			return nil // another writer decided first; the next pass sees the result
		}
		return err
	}
	return nil
}

// assessReview gathers the facts for one Review attempt and decides. It never
// writes: the API view and the driver both call it.
func (s *Service) assessReview(ctx context.Context, run domain.PipelineRun, attempt domain.PipelineStageAttempt, owner domain.SessionRecord) (gateDecision, *domain.PipelineReviewLink, error) {
	in := gateInput{Checkpoint: attempt.InputCommit, Branch: run.ExpectedBranch, AutoEnabled: owner.AutoReviewEnabled}
	in.Git, in.GitErr = s.git.Inspect(ctx, owner.Metadata.WorkspacePath)
	link, ok, err := s.store.GetPipelineReviewLink(ctx, attempt.ID)
	if err != nil {
		return gateDecision{}, nil, err
	}
	var linkPtr *domain.PipelineReviewLink
	if ok {
		in.Link = &link
		linkPtr = &link
	}
	facts, err := s.reviews.Facts(ctx, run.SessionID)
	if err != nil {
		return gateDecision{}, nil, fmt.Errorf("load review facts: %w", err)
	}
	in.Facts = facts
	return evaluateReview(in), linkPtr, nil
}

// triggerReview starts the built-in review the same way AO's auto-review would,
// but on the run's behalf: the pipeline marks its own call so only the Review
// stage can start a pass while the run owns the task. A task that is not yet
// eligible (not idle long enough, for example) simply waits; an error pauses
// instead of being retried by the review subsystem's own retry policy.
func (s *Service) triggerReview(ctx context.Context, run domain.PipelineRun, attempt domain.PipelineStageAttempt, dec gateDecision) error {
	skip, err := s.reviews.TriggerAuto(ports.WithPipelineBypass(ctx), run.SessionID)
	if err != nil {
		return s.pause(ctx, run, &attempt, PauseReviewOperational, fmt.Sprintf("The built-in review could not be started for revision %s: %v", shortCommit(attempt.InputCommit), err))
	}
	if skip != "" {
		dec.Code = WaitAwaitingAutoReview
		dec.Detail = fmt.Sprintf("Auto review is on but the task is not eligible yet (%s); AO will try again", skip)
		s.noteWaiting(ctx, run, attempt, dec)
		return nil
	}
	s.waits.Delete(attempt.ID)
	_ = s.store.AddPipelineEvent(ctx, domain.PipelineEvent{RunID: run.ID, AttemptID: attempt.ID, Kind: eventReviewTriggered, CreatedAt: s.clock(), Detail: eventDetail{Code: "auto", Message: fmt.Sprintf("Started the built-in review for revision %s", shortCommit(attempt.InputCommit))}.marshal()})
	s.wake()
	return nil
}

// routeReviewFeedback closes a Review attempt whose verdict was changes
// requested. When the stage declares a repair route and the shared budget has
// room, the same atomic change counts the return and creates the Build attempt
// that carries the revision-bound feedback to the original worker; Build then
// goes straight back to Review, never through Test. Otherwise the run pauses on
// an actionable reason with the verdict kept as the attempt's result.
func (s *Service) routeReviewFeedback(ctx context.Context, run domain.PipelineRun, snap Snapshot, attempt domain.PipelineStageAttempt, dec gateDecision) error {
	now := s.clock()
	t := domain.PipelineTransition{
		RunID: run.ID, ExpectedRevision: run.Revision, At: now,
		Attempt: &domain.PipelineAttemptFinish{ID: attempt.ID, State: domain.PipelineAttemptFailed, OutputCommit: attempt.InputCommit, NoChange: true, Outcome: dec.Outcome, Summary: dec.Detail, FinishedAt: now},
		Events:  []domain.PipelineEvent{{AttemptID: attempt.ID, Kind: eventReviewChangesRequested, Detail: eventDetail{Code: dec.Outcome, Message: dec.Detail}.marshal()}},
	}
	var fb Feedback
	if dec.Run != nil {
		fb = reviewFeedback(attempt, attempt.InputCommit, *dec.Run)
	}
	plan, exhausted := s.planRepair(ctx, run, snap, attempt, domain.PipelineRepairReviewFeedback, fb, attempt.InputCommit)
	s.applyRepairOrPause(&t, run, plan, exhausted, dec.Pause, dec.Detail, fb)
	_, err := s.store.CommitPipelineTransition(ctx, t)
	if errors.Is(err, domain.ErrPipelineConflict) {
		return nil // another writer decided first; the next pass sees the result
	}
	if err == nil && plan != nil {
		s.wake()
	}
	return err
}

// waitNote is the most recent reason a Review attempt is waiting.
type waitNote struct{ Code, Detail string }

// noteWaiting records why a Review attempt is waiting, once per reason, so the
// history explains the delay without a row per poll.
func (s *Service) noteWaiting(ctx context.Context, run domain.PipelineRun, attempt domain.PipelineStageAttempt, dec gateDecision) {
	if prev, ok := s.waits.Load(attempt.ID); ok {
		if note, isNote := prev.(waitNote); isNote && note.Code == dec.Code {
			s.waits.Store(attempt.ID, waitNote{Code: dec.Code, Detail: dec.Detail})
			return
		}
	}
	s.waits.Store(attempt.ID, waitNote{Code: dec.Code, Detail: dec.Detail})
	if err := s.store.AddPipelineEvent(ctx, domain.PipelineEvent{RunID: run.ID, AttemptID: attempt.ID, Kind: eventReviewWaiting, CreatedAt: s.clock(), Detail: eventDetail{Code: dec.Code, Message: dec.Detail}.marshal()}); err != nil {
		s.logger.Error("pipeline: record review wait failed", "run_id", run.ID, "err", err)
	}
}

// reviewGateFor assesses the run's Review stage for the API without changing
// anything. It returns nil when the run is not at a Review stage.
func (s *Service) reviewGateFor(ctx context.Context, run domain.PipelineRun, attempts []domain.PipelineStageAttempt) *ReviewGateView {
	if !run.State.Unfinished() || s.reviews == nil {
		return nil
	}
	var attempt *domain.PipelineStageAttempt
	for i := range attempts {
		if attempts[i].StageID == run.CurrentStageID && attempts[i].StageKind == string(pipeline.StageReview) {
			attempt = &attempts[i]
		}
	}
	if attempt == nil || attempt.State == domain.PipelineAttemptHandoff {
		return nil
	}
	owner, ok, err := s.store.GetSession(ctx, run.SessionID)
	if err != nil || !ok {
		return nil
	}
	dec, _, err := s.assessReview(ctx, run, *attempt, owner)
	if err != nil {
		return &ReviewGateView{State: GateStateWaiting, Code: WaitAwaitingCIStatus, Message: "Review facts could not be read; AO will retry", Checkpoint: attempt.InputCommit, AutoReview: owner.AutoReviewEnabled}
	}
	if dec.Kind == gateTrigger {
		// The live decision only knows a review is about to start; the driver
		// knows why it has not (for example the task is not idle long enough).
		if prev, ok := s.waits.Load(attempt.ID); ok {
			if note, isNote := prev.(waitNote); isNote && note.Code == WaitAwaitingAutoReview && note.Detail != "" {
				dec.Detail = note.Detail
			}
		}
	}
	v := &ReviewGateView{Code: dec.Code, Message: dec.Detail, Checkpoint: attempt.InputCommit, AutoReview: owner.AutoReviewEnabled, Manual: dec.Code == WaitAwaitingManual}
	switch dec.Kind {
	case gateComplete:
		v.State = GateStateReady
	case gatePause:
		v.State = GateStateBlocked
	default:
		v.State = GateStateWaiting
	}
	if dec.PR != nil {
		v.PRURL, v.PRNumber, v.HeadSHA = dec.PR.URL, dec.PR.Number, dec.PR.HeadSHA
	}
	if dec.Run != nil {
		v.ReviewRunID, v.ReviewStatus, v.Verdict = dec.Run.ID, string(dec.Run.Status), string(dec.Run.Verdict)
	}
	v.CI, v.CIDetail = dec.CI.State, dec.CI.Detail
	return v
}

// reviewEvidence lists, per Review attempt, the head and review run it is
// bound to together with its result.
func reviewEvidence(attempts []domain.PipelineStageAttempt, links []domain.PipelineReviewLink, checkpoint *CheckpointView) []ReviewEvidenceView {
	byAttempt := map[string]domain.PipelineReviewLink{}
	for _, l := range links {
		byAttempt[l.AttemptID] = l
	}
	out := []ReviewEvidenceView{}
	for _, a := range attempts {
		if a.StageKind != string(pipeline.StageReview) {
			continue
		}
		l, linked := byAttempt[a.ID]
		if !linked && a.Outcome == "" {
			continue // nothing to show until it is bound to a head or has a result
		}
		ev := ReviewEvidenceView{StageID: a.StageID, AttemptID: a.ID, Revision: a.InputCommit, Outcome: a.Outcome, Summary: a.Summary}
		if linked {
			ev.PRURL, ev.HeadSHA, ev.ReviewRunID = l.PRURL, l.HeadSHA, l.ReviewRunID
		}
		ev.Current = checkpoint != nil && a.InputCommit == checkpoint.OutputCommit
		out = append(out, ev)
	}
	return out
}

// ReviewTriggerAllowed implements ports.PipelineReviewGuard: a review pass may
// start only while the task's pipeline run is executing its Review stage.
func (s *Service) ReviewTriggerAllowed(ctx context.Context, id domain.SessionID) (bool, string) {
	return reviewTriggerAllowed(ctx, s.store, id, s.logger.Error)
}

type reviewGuardStore interface {
	GetActivePipelineRunBySession(ctx context.Context, id domain.SessionID) (domain.PipelineRun, bool, error)
	ListPipelineStageAttempts(ctx context.Context, runID string) ([]domain.PipelineStageAttempt, error)
}

func reviewTriggerAllowed(ctx context.Context, store reviewGuardStore, id domain.SessionID, logf func(msg string, args ...any)) (bool, string) {
	run, ok, err := store.GetActivePipelineRunBySession(ctx, id)
	if err != nil {
		logf("pipeline: cannot determine run state; refusing review", "session_id", id, "err", err)
		return false, "the task's pipeline state could not be determined"
	}
	if !ok {
		return true, ""
	}
	attempts, err := store.ListPipelineStageAttempts(ctx, run.ID)
	if err != nil {
		logf("pipeline: cannot load attempts; refusing review", "session_id", id, "err", err)
		return false, "the task's pipeline state could not be determined"
	}
	if run.State == domain.PipelineRunRunning && activeReviewAttempt(run, attempts) != nil {
		return true, ""
	}
	stage := run.CurrentStageID
	if run.State == domain.PipelineRunPaused {
		return false, fmt.Sprintf("the task's pipeline is paused at stage %q; resume it before reviewing", stage)
	}
	return false, fmt.Sprintf("the task's pipeline is at stage %q and has not reached Review; the review starts automatically when it does", stage)
}

var _ ports.PipelineReviewGuard = (*Service)(nil)
var _ ports.PipelineReviewGuard = (*StoreGuard)(nil)
