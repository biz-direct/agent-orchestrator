package ports

import (
	"context"
	"errors"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// PipelineGuard lets ordinary lifecycle shortcuts (automatic review, merge-driven
// completion/cleanup) stay out of the way while a pipeline run still owns a
// worker's lifecycle. A nil guard means "no pipelines": behavior is unchanged
// for every ordinary worker.
type PipelineGuard interface {
	// SuppressesLifecycleShortcuts reports whether an unfinished pipeline run
	// owns the session. It fails closed: an undeterminable answer suppresses.
	SuppressesLifecycleShortcuts(ctx context.Context, id domain.SessionID) bool
}

// PipelineReviewGuard is the optional second half of a PipelineGuard: it decides
// whether a review pass may be started for a session right now. A task whose
// pipeline run has not reached its Review stage must not be reviewed early, so
// a manual or idle-worker trigger cannot bypass Build and Test. A session with
// no unfinished run is always allowed, so ordinary review is unchanged.
type PipelineReviewGuard interface {
	ReviewTriggerAllowed(ctx context.Context, id domain.SessionID) (allowed bool, reason string)
}

// PipelineExecutionGate decides whether a session may currently act for its
// pipeline run: receive new input, restart its controller, or be restored.
// Only the active stage's executor is admitted; the owner worker during an
// attached specialist's stage, an attached session outside its stage, and
// everyone during a handoff are refused. A session with no pipeline is always
// admitted, so ordinary workers never see this gate.
type PipelineExecutionGate interface {
	AdmitSessionExecution(ctx context.Context, id domain.SessionID) (admitted bool, reason string)
}

type pipelineBypassKey struct{}

// WithPipelineBypass marks ctx as carrying the pipeline coordinator's own
// delivery (stage instructions, handoff prompts), which the execution gate must
// not refuse. Every other path is gated.
func WithPipelineBypass(ctx context.Context) context.Context {
	return context.WithValue(ctx, pipelineBypassKey{}, true)
}

// PipelineBypass reports whether ctx was marked by WithPipelineBypass.
func PipelineBypass(ctx context.Context) bool {
	v, _ := ctx.Value(pipelineBypassKey{}).(bool)
	return v
}

// PipelineExecutor starts, quiesces, and stops the Chat executors of pipeline
// stages. The worker executes Build; every other stage is an attached
// specialist: a separate provider conversation that runs in the worker's own
// worktree and can never delete or claim it.
type PipelineExecutor interface {
	// PreflightStage reports whether a harness can run as an attached Chat
	// specialist now. Unsupported combinations fail visibly; there is no
	// Terminal fallback.
	PreflightStage(ctx context.Context, harness domain.AgentHarness) error
	// RelinquishExecutor fences the executor's intake and proves it is
	// quiescent: no running turn, no queued work, no pending approval, and no
	// background work. Anything it cannot prove is an error wrapping
	// ErrPipelineExecutionUncertain.
	RelinquishExecutor(ctx context.Context, id domain.SessionID) error
	// InterruptExecutor asks the executor to stop its running turn now and then
	// tries to prove it quiescent, exactly as RelinquishExecutor does. A
	// requested interrupt is never proof: when the turn, a queued item, a
	// background task, or a pending permission or input request cannot be shown
	// to be gone it returns an error wrapping ErrPipelineExecutionUncertain. It
	// never answers a permission or input request on the user's behalf and
	// never touches the workspace.
	InterruptExecutor(ctx context.Context, id domain.SessionID) error
	// ReleaseExecutor lifts a handoff fence so a person can talk to the
	// executor again. It delivers no turn and starts nothing; for an executor
	// that is not fenced it does nothing.
	ReleaseExecutor(ctx context.Context, id domain.SessionID) error
	// StartStage starts (or, for a retried stage, resumes) the stage's
	// attached conversation and delivers the stage prompt.
	StartStage(ctx context.Context, start PipelineStageStart) (PipelineStageStarted, error)
	// ResumeExecutor reopens an executor that was relinquished earlier and
	// delivers prompt as a new turn in its own existing conversation. A
	// controller that is no longer running cannot be resumed safely here: it
	// returns ErrPipelineResumeUnsafe and the run asks for a recovery decision
	// instead of silently starting a fresh conversation.
	ResumeExecutor(ctx context.Context, id domain.SessionID, prompt string) (PipelineStageStarted, error)
	// StopStage stops an attached stage's controller. It never touches the
	// shared workspace.
	StopStage(ctx context.Context, id domain.SessionID) error
}

// PipelineStageStart is everything an attached specialist needs.
type PipelineStageStart struct {
	RunID     string
	StageID   string
	AttemptID string
	Owner     domain.SessionID
	// ExistingSessionID resumes the stage's retained conversation; empty on the
	// first attempt.
	ExistingSessionID domain.SessionID
	Harness           domain.AgentHarness
	Model             string
	SystemPrompt      string
	Prompt            string
}

// PipelineStageStarted identifies the running attached conversation.
type PipelineStageStarted struct {
	SessionID              domain.SessionID
	ControllerGeneration   string
	ProviderConversationID string
}

// Pipeline execution errors.
var (
	// ErrPipelineExecutionUncertain means quiescence could not be proven.
	ErrPipelineExecutionUncertain = errors.New("pipeline executor quiescence could not be proven")
	// ErrPipelineResumeUnsafe means a stage's native conversation cannot be
	// resumed without a human recovery decision.
	ErrPipelineResumeUnsafe = errors.New("the stage conversation cannot be resumed safely")
	// ErrPipelineStageUnsupported means the stage cannot run as a Chat specialist.
	ErrPipelineStageUnsupported = errors.New("pipeline stage is not supported by this harness or mode")
	// ErrPipelineExecutionOwned is returned to callers refused by the gate.
	ErrPipelineExecutionOwned = errors.New("the task's pipeline run is executing another stage")
	// ErrPipelineReviewNotReady means a review pass was requested before the
	// task's pipeline reached its Review stage.
	ErrPipelineReviewNotReady = errors.New("the task's pipeline has not reached its review stage")
)
