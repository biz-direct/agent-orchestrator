package domain

import (
	"errors"
	"time"
)

// PipelineRunState is the lifecycle of one pipeline run attached to a worker.
type PipelineRunState string

// Pipeline run states. Running and paused runs are "unfinished": the worker's
// ordinary completion and review shortcuts stay suppressed for them.
const (
	PipelineRunRunning   PipelineRunState = "running"
	PipelineRunPaused    PipelineRunState = "paused"
	PipelineRunCompleted PipelineRunState = "completed"
	PipelineRunCancelled PipelineRunState = "cancelled"
)

// Unfinished reports whether the run still owns the worker's lifecycle.
func (s PipelineRunState) Unfinished() bool {
	return s == PipelineRunRunning || s == PipelineRunPaused
}

// PipelineAttemptState is the outcome of one stage attempt.
type PipelineAttemptState string

// Pipeline stage attempt states.
const (
	// PipelineAttemptHandoff marks a successor attempt that has been created
	// (inputs recorded, predecessor accepted) but whose executor is not yet
	// confirmed started. Nothing executes in this state.
	PipelineAttemptHandoff     PipelineAttemptState = "handoff"
	PipelineAttemptActive      PipelineAttemptState = "active"
	PipelineAttemptAccepted    PipelineAttemptState = "accepted"
	PipelineAttemptFailed      PipelineAttemptState = "failed"
	PipelineAttemptInterrupted PipelineAttemptState = "interrupted"
	PipelineAttemptCancelled   PipelineAttemptState = "cancelled"
)

// PipelinePauseReason is the machine-readable cause of a pause. Operational
// pauses never consume the repair budget.
type PipelinePauseReason string

// Pipeline pause reasons.
const (
	PipelinePauseControllerChanged PipelinePauseReason = "controller_changed"
	PipelinePauseSessionTerminated PipelinePauseReason = "session_terminated"
	PipelinePauseStageFailed       PipelinePauseReason = "stage_failed"
	PipelinePauseBranchChanged     PipelinePauseReason = "branch_changed"
)

// PipelineRequester records who started a run. Only a user may supply harness
// or model overrides for a stage.
type PipelineRequester string

// Pipeline requesters.
const (
	PipelineRequestedByUser         PipelineRequester = "user"
	PipelineRequestedByOrchestrator PipelineRequester = "orchestrator"
)

// PipelineRun is durable execution state attached to an existing worker
// session. The worker remains the owner of the worktree, branch, and PR.
type PipelineRun struct {
	ID             string
	SessionID      SessionID
	ProjectID      ProjectID
	WorkflowID     string
	State          PipelineRunState
	PauseReason    PipelinePauseReason
	PauseDetail    string
	CurrentStageID string
	RequestedBy    PipelineRequester
	ExpectedBranch string
	RepairBudget   int
	RepairsUsed    int
	// Snapshot is the frozen JSON definition captured when the run started.
	Snapshot       string
	SnapshotSHA256 string
	Revision       int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
	CompletedAt    *time.Time
}

// PipelineStageAttempt is one execution attempt of one stage.
type PipelineStageAttempt struct {
	ID                   string
	RunID                string
	StageID              string
	StageKind            string
	AttemptNo            int
	State                PipelineAttemptState
	ExecutorSessionID    SessionID
	ControllerGeneration string
	InputCommit          string
	OutputCommit         string
	NoChange             bool
	Outcome              string
	Summary              string
	ResultKey            string
	InstructionDelivery  string
	StartedAt            time.Time
	FinishedAt           *time.Time
	// PredecessorAttemptID names the attempt whose output is this attempt's input.
	PredecessorAttemptID string
	// ResultJSON is the structured report submitted for the attempt.
	ResultJSON string
}

// PipelineEvent is one append-only execution fact.
type PipelineEvent struct {
	ID        int64
	RunID     string
	AttemptID string
	Kind      string
	Detail    string
	CreatedAt time.Time
}

// Pipeline persistence errors.
var (
	// ErrPipelineRunActive means the session already owns an unfinished run.
	ErrPipelineRunActive = errors.New("session already has an unfinished pipeline run")
	// ErrPipelineConflict means a compare-and-set transition lost a race.
	ErrPipelineConflict = errors.New("pipeline run changed concurrently")
)

// PipelineRunUpdate replaces the mutable run fields in one compare-and-set.
type PipelineRunUpdate struct {
	State          PipelineRunState
	PauseReason    PipelinePauseReason
	PauseDetail    string
	CurrentStageID string
	CompletedAt    *time.Time
}

// PipelineAttemptFinish closes an active attempt.
type PipelineAttemptFinish struct {
	ID           string
	State        PipelineAttemptState
	OutputCommit string
	NoChange     bool
	Outcome      string
	Summary      string
	ResultKey    string
	ResultJSON   string
	FinishedAt   time.Time
}

// PipelineAttemptActivation confirms a handoff attempt's executor started.
type PipelineAttemptActivation struct {
	ID                   string
	ExecutorSessionID    SessionID
	ControllerGeneration string
	At                   time.Time
}

// PipelineTransition is one atomic change: optionally finish an attempt,
// update the run (guarded by its revision), and append events.
type PipelineTransition struct {
	RunID            string
	ExpectedRevision int64
	Run              *PipelineRunUpdate
	Attempt          *PipelineAttemptFinish
	// NewAttempt inserts a successor attempt in the same atomic change.
	NewAttempt *PipelineStageAttempt
	// Activate flips a handoff attempt to active.
	Activate *PipelineAttemptActivation
	Events   []PipelineEvent
	At       time.Time
}
