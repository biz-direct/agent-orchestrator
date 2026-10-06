package pipelineruns

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/pipeline"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Run controls.
const (
	ControlPause            = "pause"
	ControlResume           = "resume"
	ControlCancel           = "cancel"
	ControlAuthorizeRepairs = "authorize_repairs"
)

// Pause reasons set by a control request. Both are operational: neither
// spends the repair budget.
const (
	PauseByUser         domain.PipelinePauseReason = "paused_by_user"
	PauseByOrchestrator domain.PipelinePauseReason = "paused_by_orchestrator"
)

// Control execution facts recorded in pipeline_events.
const (
	eventExecutionStop     = "execution_stop"
	eventRunResumed        = "run_resumed"
	eventRunCancelled      = "run_cancelled"
	eventRepairsAuthorized = "repairs_authorized"

	// stopSettleWait bounds how long a pause or cancel waits for an in-flight
	// validation round to report that its processes were killed.
	stopSettleWait = 5 * time.Second
)

// ControlInput is one pause, resume, cancel, or repair-authorization request.
type ControlInput struct {
	SessionID domain.SessionID `json:"-"`
	// RunID names the run the caller is looking at; a request for any run other
	// than the task's current one is refused as stale.
	RunID  string `json:"runId"`
	Action string `json:"action" enum:"pause,resume,cancel,authorize_repairs"`
	// RequestedBy is "user" or "orchestrator". Recovery decisions and extra
	// repairs are human-only.
	RequestedBy string `json:"requestedBy" enum:"user,orchestrator"`
	// ExpectedRevision, when set, makes the request conditional on the run not
	// having changed since the caller read it.
	ExpectedRevision int64  `json:"expectedRevision,omitempty"`
	Reason           string `json:"reason,omitempty"`
	// AdditionalRepairs and RequestKey apply to authorize_repairs only. The key
	// makes a repeated request a no-op instead of a second grant.
	AdditionalRepairs int    `json:"additionalRepairs,omitempty"`
	RequestKey        string `json:"requestKey,omitempty"`
	// Recovery is the explicit choice a person makes to resume a run paused for a
	// recovery decision. "restore_conversation" restores the same native
	// conversation (it never starts a fresh one in its place) and then resumes.
	Recovery string `json:"recovery,omitempty" enum:"restore_conversation"`
}

// RecoveryRestoreConversation is the only recovery choice: bring the stage's
// existing conversation back, then continue.
const RecoveryRestoreConversation = "restore_conversation"

// StopView says what AO knows about the executor it asked to stop. A requested
// interrupt is not proof: Confirmed is true only when quiescence was shown.
type StopView struct {
	Requested bool   `json:"requested"`
	Confirmed bool   `json:"confirmed"`
	Detail    string `json:"detail,omitempty"`
}

// ControlResult is the outcome of a control request.
type ControlResult struct {
	Run RunView `json:"run"`
	// Changed is false when the request was already satisfied (a repeat).
	Changed bool      `json:"changed"`
	Stop    *StopView `json:"stop,omitempty"`
}

// humanOnlyPause lists the pauses that are decisions for a person: an
// orchestrator may not resume them, so it cannot bypass them.
func humanOnlyPause(reason domain.PipelinePauseReason) bool {
	switch reason {
	case PauseRecoveryDecision, PauseRepairBudgetExhausted, PauseValidationInterrupted:
		return true
	}
	return false
}

// Control implements Manager.
func (s *Service) Control(ctx context.Context, in ControlInput) (ControlResult, error) {
	requester := domain.PipelineRequester(strings.TrimSpace(in.RequestedBy))
	switch requester {
	case "":
		requester = domain.PipelineRequestedByOrchestrator
	case domain.PipelineRequestedByUser, domain.PipelineRequestedByOrchestrator:
	default:
		return ControlResult{}, apierr.Invalid("INVALID_PIPELINE_REQUESTER", `requestedBy must be "user" or "orchestrator"`, nil)
	}
	switch in.Action {
	case ControlPause, ControlResume, ControlCancel, ControlAuthorizeRepairs:
	default:
		return ControlResult{}, apierr.Invalid("INVALID_PIPELINE_CONTROL", `action must be "pause", "resume", "cancel", or "authorize_repairs"`, nil)
	}
	if strings.TrimSpace(in.RunID) == "" {
		return ControlResult{}, apierr.Invalid("INVALID_PIPELINE_CONTROL", "runId is required", nil)
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if len([]rune(in.Reason)) > maxSummaryLen {
		return ControlResult{}, apierr.Invalid("INVALID_PIPELINE_CONTROL", fmt.Sprintf("reason must be at most %d characters", maxSummaryLen), nil)
	}

	owner, err := s.ownerOf(ctx, in.SessionID)
	if err != nil {
		return ControlResult{}, err
	}
	unlock := s.lock(owner)
	defer unlock()

	run, ok, err := s.store.GetPipelineRun(ctx, in.RunID)
	if err != nil {
		return ControlResult{}, apierr.Internal("PIPELINE_LOAD_FAILED", "Failed to load pipeline run")
	}
	if !ok || run.SessionID != owner {
		return ControlResult{}, apierr.NotFound("PIPELINE_RUN_NOT_FOUND", "Unknown pipeline run for this task")
	}
	if latest, found, lerr := s.store.GetLatestPipelineRunBySession(ctx, owner); lerr == nil && found && latest.ID != run.ID {
		return ControlResult{}, apierr.Conflict("PIPELINE_STALE_CONTROL", "This request is for an older pipeline run; read the task's current run and try again", nil)
	}

	var res ControlResult
	switch in.Action {
	case ControlPause:
		res, err = s.pauseByControl(ctx, run, requester, in)
	case ControlResume:
		res, err = s.resumeByControl(ctx, run, requester, in)
	case ControlCancel:
		res, err = s.cancelByControl(ctx, run, requester, in)
	default:
		res, err = s.authorizeRepairs(ctx, run, requester, in)
	}
	if err != nil {
		return ControlResult{}, err
	}
	view, verr := s.view(ctx, run.ID)
	if verr != nil {
		return ControlResult{}, verr
	}
	res.Run = view
	return res, nil
}

func finishedConflict(run domain.PipelineRun) error {
	return apierr.Conflict("PIPELINE_RUN_FINISHED", fmt.Sprintf("The pipeline run is already %s", run.State), nil)
}

func staleRevision(run domain.PipelineRun, in ControlInput) error {
	if in.ExpectedRevision != 0 && in.ExpectedRevision != run.Revision {
		return apierr.Conflict("PIPELINE_STALE_CONTROL", fmt.Sprintf("The run changed since you read it (revision %d, now %d); read it and decide again", in.ExpectedRevision, run.Revision), nil)
	}
	return nil
}

// ---- pause ----

func (s *Service) pauseByControl(ctx context.Context, run domain.PipelineRun, requester domain.PipelineRequester, in ControlInput) (ControlResult, error) {
	switch run.State {
	case domain.PipelineRunPaused:
		return ControlResult{Changed: false}, nil // already paused: a repeat is a no-op
	case domain.PipelineRunRunning:
	default:
		return ControlResult{}, finishedConflict(run)
	}
	if err := staleRevision(run, in); err != nil {
		return ControlResult{}, err
	}
	attempts, err := s.store.ListPipelineStageAttempts(ctx, run.ID)
	if err != nil {
		return ControlResult{}, apierr.Internal("PIPELINE_LOAD_FAILED", "Failed to load pipeline attempts")
	}
	reason, who := PauseByUser, "the user"
	if requester == domain.PipelineRequestedByOrchestrator {
		reason, who = PauseByOrchestrator, "an orchestrator"
	}
	detail := "Paused by " + who
	if in.Reason != "" {
		detail += ": " + in.Reason
	}
	current := currentAttempt(run, attempts)
	now := s.clock()
	t := domain.PipelineTransition{
		RunID: run.ID, ExpectedRevision: run.Revision, At: now,
		Run:    &domain.PipelineRunUpdate{State: domain.PipelineRunPaused, PauseReason: reason, PauseDetail: detail, CurrentStageID: run.CurrentStageID},
		Events: []domain.PipelineEvent{{Kind: eventRunPaused, Detail: eventDetail{Code: string(reason), Message: detail}.marshal()}},
	}
	if current != nil {
		t.Events[0].AttemptID = current.ID
		// Persisting the pause first fences new stage starts and handoffs: the
		// driver only advances running runs and Submit refuses a paused one.
		if current.State == domain.PipelineAttemptActive {
			t.Attempt = &domain.PipelineAttemptFinish{ID: current.ID, State: domain.PipelineAttemptInterrupted, Outcome: "interrupted", FinishedAt: now}
		}
	}
	if _, err := s.store.CommitPipelineTransition(ctx, t); err != nil {
		return ControlResult{}, s.controlCommitError(err, "pause")
	}
	stop := s.stopExecution(ctx, run, current, true)
	return ControlResult{Changed: true, Stop: stop}, nil
}

// currentAttempt returns the newest attempt of the run's current stage.
func currentAttempt(run domain.PipelineRun, attempts []domain.PipelineStageAttempt) *domain.PipelineStageAttempt {
	var cur *domain.PipelineStageAttempt
	for i := range attempts {
		if attempts[i].StageID == run.CurrentStageID {
			cur = &attempts[i]
		}
	}
	return cur
}

func (s *Service) controlCommitError(err error, what string) error {
	if errors.Is(err, domain.ErrPipelineConflict) {
		return apierr.Conflict("PIPELINE_STALE_CONTROL", "The run changed while the request was being applied; read it and decide again", nil)
	}
	s.logger.Error("pipeline: control transition failed", "action", what, "err", err)
	return apierr.Internal("PIPELINE_CONTROL_FAILED", "Failed to "+what+" the pipeline run")
}

// ---- stop hooks ----

// Stage controls: how each kind of work honors a pause or cancel once the
// request is persisted. A stage kind that lands later supplies its own case
// here instead of a new control path.
//
//   - executor stages (Build, specialists): interrupt the running turn and try to
//     prove quiescence; never answer a pending permission or input request.
//   - command validation: cancel the in-flight round; its process group is killed
//     and each command is recorded as cancelled.
//   - Review: nothing executes; evaluation simply stops. AO's built-in reviewer
//     keeps its own lifecycle and the ordinary review controls.
func (s *Service) stopExecution(ctx context.Context, run domain.PipelineRun, a *domain.PipelineStageAttempt, reopen bool) *StopView {
	if a == nil {
		return &StopView{Confirmed: true, Detail: "No stage attempt was executing."}
	}
	stop := &StopView{}
	record := func() {
		code := "unconfirmed"
		if stop.Confirmed {
			code = "confirmed"
		}
		_ = s.store.AddPipelineEvent(context.WithoutCancel(ctx), domain.PipelineEvent{RunID: run.ID, AttemptID: a.ID, Kind: eventExecutionStop, CreatedAt: s.clock(), Detail: eventDetail{Code: code, Message: stop.Detail}.marshal()})
	}
	defer record()

	switch {
	case a.StageKind == string(pipeline.StageReview):
		stop.Confirmed = true
		stop.Detail = "Review has no executor; AO's built-in reviewer keeps its own lifecycle."
	case a.State == domain.PipelineAttemptValidating:
		stop.Requested = true
		s.cancelValidation(run.ID)
		if s.waitDriverIdle(run.ID, stopSettleWait) {
			stop.Confirmed = true
			stop.Detail = "The validation round was cancelled and its commands were killed."
		} else {
			stop.Detail = "The validation round was asked to stop but has not reported that its commands were killed yet."
		}
	case a.State == domain.PipelineAttemptHandoff:
		stop.Confirmed = true
		stop.Detail = "A handoff was pending; no stage was executing, and it will not start while the run is not running."
	case a.State == domain.PipelineAttemptActive && a.ExecutorSessionID != "" && s.executor != nil:
		stop.Requested = true
		if err := s.executor.InterruptExecutor(ports.WithPipelineBypass(ctx), a.ExecutorSessionID); err != nil {
			// Not proof: the turn, background work, or a permission request may
			// still be live. AO says so and leaves the prompt for the person.
			stop.Detail = fmt.Sprintf("Stop requested for the stage's executor, but it was not confirmed stopped: %v", err)
		} else {
			stop.Confirmed = true
			stop.Detail = "The stage's executor was interrupted and shown to be idle."
		}
		if reopen {
			// A paused run keeps its executor reachable so a person can talk to it.
			_ = s.executor.ReleaseExecutor(context.WithoutCancel(ctx), a.ExecutorSessionID)
		}
	default:
		stop.Confirmed = true
		stop.Detail = "The attempt was not executing."
	}
	return stop
}

// cancelValidation asks the run's in-flight validation round, if any, to stop.
func (s *Service) cancelValidation(runID string) {
	if cancel, ok := s.validations.Load(runID); ok {
		if fn, isFn := cancel.(context.CancelFunc); isFn {
			fn()
		}
	}
}

// waitDriverIdle waits until no driver is working on the run, up to d.
func (s *Service) waitDriverIdle(runID string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if _, busy := s.driving.Load(runID); !busy {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// releaseOwner lifts the handoff fence on the task's own worker so a finished or
// cancelled run hands the conversation back. It starts nothing and is a no-op
// for a worker that was never fenced.
func (s *Service) releaseOwner(ctx context.Context, id domain.SessionID) {
	if s.executor == nil {
		return
	}
	if err := s.executor.ReleaseExecutor(context.WithoutCancel(ctx), id); err != nil {
		s.logger.Error("pipeline: release worker intake failed", "session_id", id, "err", err)
	}
}

// ---- cancel ----

func (s *Service) cancelByControl(ctx context.Context, run domain.PipelineRun, requester domain.PipelineRequester, in ControlInput) (ControlResult, error) {
	switch run.State {
	case domain.PipelineRunCancelled:
		return ControlResult{Changed: false}, nil
	case domain.PipelineRunCompleted:
		return ControlResult{}, finishedConflict(run)
	}
	if err := staleRevision(run, in); err != nil {
		return ControlResult{}, err
	}
	attempts, err := s.store.ListPipelineStageAttempts(ctx, run.ID)
	if err != nil {
		return ControlResult{}, apierr.Internal("PIPELINE_LOAD_FAILED", "Failed to load pipeline attempts")
	}
	var live *domain.PipelineStageAttempt
	for i := range attempts {
		switch attempts[i].State {
		case domain.PipelineAttemptActive, domain.PipelineAttemptHandoff, domain.PipelineAttemptValidating:
			live = &attempts[i]
		}
	}
	who := "the user"
	if requester == domain.PipelineRequestedByOrchestrator {
		who = "an orchestrator"
	}
	message := fmt.Sprintf("Cancelled by %s. Nothing was reset or deleted: the worktree, branch, pull request, conversations, and evidence are kept, and the original worker still owns the task", who)
	if in.Reason != "" {
		message += ". Reason: " + in.Reason
	}
	now := s.clock()
	t := domain.PipelineTransition{
		RunID: run.ID, ExpectedRevision: run.Revision, At: now,
		Run:    &domain.PipelineRunUpdate{State: domain.PipelineRunCancelled, CurrentStageID: run.CurrentStageID, CompletedAt: &now},
		Events: []domain.PipelineEvent{{Kind: eventRunCancelled, Detail: eventDetail{Message: message}.marshal()}},
	}
	if live != nil {
		t.Events[0].AttemptID = live.ID
		t.Attempt = &domain.PipelineAttemptFinish{
			ID: live.ID, State: domain.PipelineAttemptCancelled, Outcome: "cancelled", OutputCommit: live.OutputCommit, NoChange: live.NoChange,
			Summary: live.Summary, ResultKey: live.ResultKey, ResultJSON: live.ResultJSON, FinishedAt: now,
		}
	}
	if _, err := s.store.CommitPipelineTransition(ctx, t); err != nil {
		return ControlResult{}, s.controlCommitError(err, "cancel")
	}
	if live != nil {
		s.waits.Delete(live.ID)
	}
	stop := s.stopExecution(ctx, run, live, false)
	// The run no longer owns the task: hand the worker back. The specialists'
	// conversations are retained; the execution gate keeps them closed.
	s.releaseOwner(ctx, run.SessionID)
	if live != nil && live.ExecutorSessionID != "" && live.ExecutorSessionID != run.SessionID && s.executor != nil {
		_ = s.executor.ReleaseExecutor(context.WithoutCancel(ctx), live.ExecutorSessionID)
	}
	return ControlResult{Changed: true, Stop: stop}, nil
}

// ---- authorize repairs ----

func (s *Service) authorizeRepairs(ctx context.Context, run domain.PipelineRun, requester domain.PipelineRequester, in ControlInput) (ControlResult, error) {
	if requester != domain.PipelineRequestedByUser {
		return ControlResult{}, apierr.Forbidden("PIPELINE_HUMAN_AUTHORIZATION_REQUIRED", "Only a person can authorize additional repairs; an orchestrator cannot extend the repair budget")
	}
	if in.AdditionalRepairs < 1 || in.AdditionalRepairs > pipeline.MaxRepairBudget {
		return ControlResult{}, apierr.Invalid("INVALID_PIPELINE_CONTROL", fmt.Sprintf("additionalRepairs must be between 1 and %d", pipeline.MaxRepairBudget), nil)
	}
	key := strings.TrimSpace(in.RequestKey)
	if key == "" || len(key) > maxIdempotencyKeyLen {
		return ControlResult{}, apierr.Invalid("INVALID_PIPELINE_CONTROL", fmt.Sprintf("requestKey is required (at most %d characters) so a repeated authorization is not granted twice", maxIdempotencyKeyLen), nil)
	}
	grants, err := s.store.ListPipelineRepairGrants(ctx, run.ID)
	if err != nil {
		return ControlResult{}, apierr.Internal("PIPELINE_LOAD_FAILED", "Failed to load repair authorizations")
	}
	for _, g := range grants {
		if g.RequestKey == key {
			return ControlResult{Changed: false}, nil // the same authorization was already recorded
		}
	}
	if run.State != domain.PipelineRunPaused || run.PauseReason != PauseRepairBudgetExhausted {
		return ControlResult{}, apierr.Conflict("PIPELINE_NO_AUTHORIZATION_NEEDED", "Additional repairs can only be authorized while the run is paused because its repair budget is exhausted", nil)
	}
	if err := staleRevision(run, in); err != nil {
		return ControlResult{}, err
	}
	now := s.clock()
	detail := fmt.Sprintf("%d additional repair(s) authorized by the user (budget is now %d); resume the run to apply them", in.AdditionalRepairs, run.RepairBudget+in.AdditionalRepairs)
	if in.Reason != "" {
		detail += ". Reason: " + in.Reason
	}
	t := domain.PipelineTransition{
		RunID: run.ID, ExpectedRevision: run.Revision, At: now,
		Run:    &domain.PipelineRunUpdate{State: domain.PipelineRunPaused, PauseReason: PauseRepairBudgetExhausted, PauseDetail: detail, CurrentStageID: run.CurrentStageID, BudgetDelta: in.AdditionalRepairs},
		Grant:  &domain.PipelineRepairGrant{ID: s.newID("pgrt"), RunID: run.ID, Amount: in.AdditionalRepairs, AuthorizedBy: domain.PipelineRequestedByUser, RequestKey: key, Note: in.Reason, CreatedAt: now},
		Events: []domain.PipelineEvent{{Kind: eventRepairsAuthorized, Detail: eventDetail{Code: "user", Message: detail}.marshal()}},
	}
	if _, err := s.store.CommitPipelineTransition(ctx, t); err != nil {
		if errors.Is(err, domain.ErrPipelineGrantDuplicate) {
			return ControlResult{Changed: false}, nil
		}
		return ControlResult{}, s.controlCommitError(err, "authorize repairs for")
	}
	return ControlResult{Changed: true}, nil
}

// ---- resume ----

func (s *Service) resumeByControl(ctx context.Context, run domain.PipelineRun, requester domain.PipelineRequester, in ControlInput) (ControlResult, error) {
	switch run.State {
	case domain.PipelineRunRunning:
		return ControlResult{Changed: false}, nil
	case domain.PipelineRunPaused:
	default:
		return ControlResult{}, finishedConflict(run)
	}
	if err := staleRevision(run, in); err != nil {
		return ControlResult{}, err
	}
	if humanOnlyPause(run.PauseReason) && requester != domain.PipelineRequestedByUser {
		return ControlResult{}, apierr.Forbidden("PIPELINE_HUMAN_DECISION_REQUIRED", fmt.Sprintf("This pause (%s) is a decision for a person; an orchestrator cannot resume it", run.PauseReason))
	}
	if in.Recovery != "" && in.Recovery != RecoveryRestoreConversation {
		return ControlResult{}, apierr.Invalid("INVALID_PIPELINE_CONTROL", `recovery must be "restore_conversation"`, nil)
	}
	if in.Recovery != "" && run.PauseReason != PauseRecoveryDecision {
		return ControlResult{}, apierr.Invalid("INVALID_PIPELINE_CONTROL", "a recovery choice only applies to a run paused for a recovery decision", nil)
	}
	if run.PauseReason == PauseRecoveryDecision && in.Recovery == "" {
		return ControlResult{}, apierr.Conflict("PIPELINE_RECOVERY_CHOICE_REQUIRED", `This pause needs a recovery decision. AO will not start a fresh conversation in place of the lost one; choose recovery "restore_conversation" to bring the same conversation back, or cancel the run`, nil)
	}
	if run.PauseReason == domain.PipelinePauseSessionTerminated {
		return ControlResult{}, apierr.Conflict("PIPELINE_SESSION_ENDED", "The task ended, so its pipeline cannot continue; cancel the run", nil)
	}
	owner, ok, err := s.store.GetSession(ctx, run.SessionID)
	if err != nil {
		return ControlResult{}, apierr.Internal("SESSION_LOAD_FAILED", "Failed to load session")
	}
	if !ok || owner.IsTerminated {
		return ControlResult{}, apierr.Conflict("PIPELINE_SESSION_ENDED", "The task ended, so its pipeline cannot continue; cancel the run", nil)
	}
	snap, err := parseSnapshot(run.Snapshot)
	if err != nil {
		return ControlResult{}, apierr.Internal("PIPELINE_SNAPSHOT_CORRUPT", "The pipeline snapshot could not be read")
	}
	attempts, err := s.store.ListPipelineStageAttempts(ctx, run.ID)
	if err != nil {
		return ControlResult{}, apierr.Internal("PIPELINE_LOAD_FAILED", "Failed to load pipeline attempts")
	}

	// Ownership and prerequisites are revalidated before anything is restarted.
	state, gerr := s.git.Inspect(ctx, owner.Metadata.WorkspacePath)
	if gerr != nil {
		return ControlResult{}, apierr.Conflict("PIPELINE_RESUME_BLOCKED", fmt.Sprintf("The task workspace cannot be read, so the run cannot resume: %v", gerr), nil)
	}
	if state.Branch != run.ExpectedBranch {
		return ControlResult{}, apierr.Conflict("PIPELINE_RESUME_BLOCKED", fmt.Sprintf("The workspace is on %q but the run expects branch %q; check out the task branch first", branchLabel(state.Branch), run.ExpectedBranch), nil)
	}

	if in.Recovery == RecoveryRestoreConversation {
		if err := s.restoreStageConversation(ctx, run, attempts); err != nil {
			return ControlResult{}, err
		}
	}

	now := s.clock()
	resumedUpdate := &domain.PipelineRunUpdate{State: domain.PipelineRunRunning, CurrentStageID: run.CurrentStageID}
	t := domain.PipelineTransition{RunID: run.ID, ExpectedRevision: run.Revision, At: now, Run: resumedUpdate}
	message := fmt.Sprintf("Resumed by %s after %s", requester, run.PauseReason)
	var cur *domain.PipelineStageAttempt
	if c := currentAttempt(run, attempts); c != nil {
		cur = c
	}

	switch {
	case run.PauseReason == PauseRepairBudgetExhausted:
		if run.RepairsUsed >= run.RepairBudget {
			return ControlResult{}, apierr.Conflict("PIPELINE_REPAIR_AUTHORIZATION_REQUIRED", "The repair budget is spent. Resuming does not grant more attempts; a person must authorize additional repairs first", nil)
		}
		if cur == nil || cur.State != domain.PipelineAttemptFailed || cur.FeedbackJSON == "" {
			return ControlResult{}, apierr.Conflict("PIPELINE_RESUME_BLOCKED", "The failing attempt did not keep its feedback, so the repair cannot be reconstructed; cancel the run", nil)
		}
		fb := parseFeedback(cur.FeedbackJSON)
		if fb == nil {
			return ControlResult{}, apierr.Conflict("PIPELINE_RESUME_BLOCKED", "The retained repair feedback could not be read; cancel the run", nil)
		}
		plan, _ := s.planRepair(ctx, run, snap, *cur, domain.PipelineRepairKind(fb.Kind), *fb, fb.Revision)
		if plan == nil {
			return ControlResult{}, apierr.Conflict("PIPELINE_RESUME_BLOCKED", "The failing stage no longer has a repair route", nil)
		}
		t.Run, t.NewAttempt, t.Repair = &plan.update, &plan.build, &plan.repair
		t.Events = append(t.Events, plan.event)
		message += "; the authorized repair was routed to Build"
	case hasState(attempts, run.CurrentStageID, domain.PipelineAttemptHandoff):
		message += "; the pending handoff will be retried"
	case hasState(attempts, run.CurrentStageID, domain.PipelineAttemptValidating):
		message += "; validation will run again in a new round, keeping the earlier results"
	case cur != nil && (cur.State == domain.PipelineAttemptInterrupted || cur.State == domain.PipelineAttemptFailed || cur.State == domain.PipelineAttemptCancelled):
		retry, rerr := s.retryAttempt(ctx, run, snap, attempts, *cur, state, owner.Metadata.WorkspacePath)
		if rerr != nil {
			return ControlResult{}, rerr
		}
		t.NewAttempt = retry
		message += fmt.Sprintf("; stage %q continues as attempt %d", cur.StageID, retry.AttemptNo)
	default:
		message += "; nothing needed to be restarted"
	}
	t.Events = append(t.Events, domain.PipelineEvent{Kind: eventRunResumed, Detail: eventDetail{Code: string(run.PauseReason), Message: message}.marshal()})
	if _, err := s.store.CommitPipelineTransition(ctx, t); err != nil {
		return ControlResult{}, s.controlCommitError(err, "resume")
	}
	s.wake()
	return ControlResult{Changed: true}, nil
}

func hasState(attempts []domain.PipelineStageAttempt, stageID string, state domain.PipelineAttemptState) bool {
	for _, a := range attempts {
		if a.StageID == stageID && a.State == state {
			return true
		}
	}
	return false
}

// retryAttempt builds the new attempt that continues an interrupted one. It
// refuses when the workspace no longer allows a safe continuation, and it never
// replays: the new attempt resumes the stage's own conversation (or, for
// Review, re-evaluates the same checkpoint) instead of restarting the work.
func (s *Service) retryAttempt(ctx context.Context, run domain.PipelineRun, snap Snapshot, attempts []domain.PipelineStageAttempt, cur domain.PipelineStageAttempt, state GitState, workspace string) (*domain.PipelineStageAttempt, error) {
	blocked := func(format string, args ...any) error {
		return apierr.Conflict("PIPELINE_RESUME_BLOCKED", fmt.Sprintf(format, args...), nil)
	}
	next := &domain.PipelineStageAttempt{
		ID: s.newID("pstg"), RunID: run.ID, StageID: cur.StageID, StageKind: cur.StageKind,
		AttemptNo: s.nextAttemptNo(ctx, run.ID, cur.StageID), State: domain.PipelineAttemptHandoff,
		StartedAt: s.clock(), RetryOfAttemptID: cur.ID,
		// A retried repair keeps its route and feedback.
		RepairSourceAttemptID: cur.RepairSourceAttemptID, ReturnStageID: cur.ReturnStageID, FeedbackJSON: cur.FeedbackJSON,
	}
	if cur.StageKind == string(pipeline.StageReview) {
		var accepted *domain.PipelineStageAttempt
		for i := range attempts {
			if attempts[i].State == domain.PipelineAttemptAccepted {
				accepted = &attempts[i]
			}
		}
		if accepted == nil {
			return nil, blocked("There is no accepted checkpoint to review")
		}
		checkpoint := accepted.OutputCommit
		switch {
		case state.Head != checkpoint:
			return nil, blocked("HEAD is %s but the accepted checkpoint is %s. Restore the checkpoint (or cancel the run) so approvals and CI results describe the revision under review", shortCommit(state.Head), shortCommit(checkpoint))
		case state.TrackedTotal > 0:
			return nil, blocked("%d tracked path(s) have uncommitted changes; commit or discard them before the review resumes", state.TrackedTotal)
		}
		next.InputCommit, next.PredecessorAttemptID = checkpoint, accepted.ID
		return next, nil
	}
	// An executor stage keeps its original input revision, so its scope and
	// results are still judged against the same base. Its own uncommitted work
	// is its own; history must not have been rewritten.
	if state.Head != cur.InputCommit {
		descends, err := s.git.IsAncestor(ctx, workspace, cur.InputCommit, state.Head)
		if err != nil {
			return nil, blocked("History could not be verified: %v", err)
		}
		if !descends {
			return nil, blocked("HEAD %s does not descend from the stage's input revision %s; history was rewritten, so the stage cannot continue safely", shortCommit(state.Head), shortCommit(cur.InputCommit))
		}
	}
	if _, _, ok := snap.stage(cur.StageID); !ok {
		return nil, blocked("Stage %q is not in the run's snapshot", cur.StageID)
	}
	next.InputCommit, next.PredecessorAttemptID = cur.InputCommit, cur.PredecessorAttemptID
	return next, nil
}

// restoreStageConversation brings back the controller of the conversation the
// paused stage must continue in. It never starts a fresh conversation: a stage
// that never had one simply starts normally when the handoff is retried.
func (s *Service) restoreStageConversation(ctx context.Context, run domain.PipelineRun, attempts []domain.PipelineStageAttempt) error {
	cur := currentAttempt(run, attempts)
	if cur == nil || s.executor == nil {
		return apierr.Conflict("PIPELINE_RECOVERY_FAILED", "AO has no executor that can restore the stage's conversation in this build", nil)
	}
	var target domain.SessionID
	switch {
	case cur.StageKind == string(pipeline.StageBuild):
		target = run.SessionID
	case cur.ExecutorSessionID != "":
		target = cur.ExecutorSessionID
	default:
		target = s.retainedStageSession(attempts, cur.StageID)
		if target == "" {
			if id, found, err := s.store.FindAttachedSessionForAttempt(ctx, cur.ID); err == nil && found {
				target = id
			}
		}
	}
	if target == "" || cur.StageKind == string(pipeline.StageReview) {
		return nil // there is no conversation to restore
	}
	if err := s.executor.RestoreExecutor(ports.WithPipelineBypass(ctx), target); err != nil {
		return apierr.Conflict("PIPELINE_RECOVERY_FAILED", fmt.Sprintf("The stage's conversation could not be restored: %v. The run stays paused; nothing was started in its place", err), nil)
	}
	return nil
}
