// Package pipelineruns executes repository-defined pipelines on existing
// worker sessions. The worker remains the owner of the worktree, branch, and PR;
// a run only records which stage is active, freezes the definition snapshot, and
// decides whether a stage result may advance the run.
//
// Authority rule: an agent reports a result, but the daemon proves it. Stage
// results are accepted only for the active attempt, under the controller
// generation it started with, against revisions the daemon itself observes in
// the worktree (clean, on the expected branch, HEAD equal to the reported
// commit). Ordinary `ao report` calls are informational and never advance a run.
package pipelineruns

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/pipeline"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Execution facts recorded in pipeline_events.
const (
	eventRunStarted           = "run_started"
	eventInstructionDelivered = "instruction_delivered"
	eventInstructionFailed    = "instruction_delivery_failed"
	eventSubmissionRejected   = "submission_rejected"
	eventStageAccepted        = "stage_accepted"
	eventStageReportedFailure = "stage_reported_failure"
	eventHandoffStarted       = "handoff_started"
	eventHandoffActivated     = "handoff_activated"
	eventProductionDefect     = "production_defect"
	eventRunCompleted         = "run_completed"
	eventRunPaused            = "run_paused"
	eventRecovery             = "recovery"
	maxSummaryLen             = 2000
	maxIdempotencyKeyLen      = 128
	recentEventLimit          = 25
)

// Stage outcomes an executor may report.
const (
	OutcomeSucceeded = "succeeded"
	OutcomeFailed    = "failed"
)

var commitPattern = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// Store is the durable surface the service needs.
type Store interface {
	GetSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error)
	GetProject(ctx context.Context, id string) (domain.ProjectRecord, bool, error)
	GetSessionAttachedTo(ctx context.Context, id domain.SessionID) (domain.SessionID, error)
	FindAttachedSessionForAttempt(ctx context.Context, attemptID string) (domain.SessionID, bool, error)
	CreatePipelineIntent(ctx context.Context, in domain.PipelineIntent) error
	GetPipelineIntent(ctx context.Context, id domain.SessionID) (domain.PipelineIntent, bool, error)
	ListPendingPipelineIntents(ctx context.Context) ([]domain.PipelineIntent, error)
	SettlePipelineIntent(ctx context.Context, id domain.SessionID, state domain.PipelineIntentState, detail, runID string, at time.Time) (bool, error)
	CreatePipelineRun(ctx context.Context, run domain.PipelineRun, first domain.PipelineStageAttempt) (domain.PipelineRun, domain.PipelineStageAttempt, error)
	GetPipelineRun(ctx context.Context, id string) (domain.PipelineRun, bool, error)
	GetActivePipelineRunBySession(ctx context.Context, id domain.SessionID) (domain.PipelineRun, bool, error)
	GetLatestPipelineRunBySession(ctx context.Context, id domain.SessionID) (domain.PipelineRun, bool, error)
	ListUnfinishedPipelineRuns(ctx context.Context) ([]domain.PipelineRun, error)
	HasUnfinishedPipelineRun(ctx context.Context, id domain.SessionID) (bool, error)
	ListPipelineStageAttempts(ctx context.Context, runID string) ([]domain.PipelineStageAttempt, error)
	GetPipelineStageAttempt(ctx context.Context, id string) (domain.PipelineStageAttempt, bool, error)
	ListPipelineEvents(ctx context.Context, runID string, limit int) ([]domain.PipelineEvent, error)
	AddPipelineEvent(ctx context.Context, ev domain.PipelineEvent) error
	SetPipelineAttemptInstructionDelivery(ctx context.Context, attemptID, state string) error
	CommitPipelineTransition(ctx context.Context, t domain.PipelineTransition) (domain.PipelineRun, error)
	CreatePipelineCommandResult(ctx context.Context, r domain.PipelineCommandResult) (int64, error)
	FinishPipelineCommandResult(ctx context.Context, id int64, r domain.PipelineCommandResult) error
	ListPipelineCommandResults(ctx context.Context, attemptID string) ([]domain.PipelineCommandResult, error)
	ListPipelineRepairs(ctx context.Context, runID string) ([]domain.PipelineRepair, error)
	ListPipelineRepairGrants(ctx context.Context, runID string) ([]domain.PipelineRepairGrant, error)
	LinkPipelineReview(ctx context.Context, l domain.PipelineReviewLink) (domain.PipelineReviewLink, error)
	GetPipelineReviewLink(ctx context.Context, attemptID string) (domain.PipelineReviewLink, bool, error)
	ListPipelineReviewLinks(ctx context.Context, runID string) ([]domain.PipelineReviewLink, error)
	MarkRunningPipelineCommandsUnknown(ctx context.Context, runID string, at time.Time, detail string) (int, error)
}

// Messenger delivers coordination messages to a session's conversation.
type Messenger interface {
	Send(ctx context.Context, id domain.SessionID, message string, attachment *ports.SpawnAttachment) error
}

// Reporter lets the run tell the task's orchestrator what happened as AO's own
// report: completion only when the pipeline validated, and an actionable pause.
type Reporter interface {
	Report(ctx context.Context, id domain.SessionID, state domain.ReportState, note string) error
}

// Manager is the controller-facing contract.
type Manager interface {
	Start(ctx context.Context, in StartInput) (RunView, error)
	Get(ctx context.Context, id domain.SessionID) (RunEnvelope, error)
	Submit(ctx context.Context, in SubmitInput) (SubmitResult, error)
	Control(ctx context.Context, in ControlInput) (ControlResult, error)
	// ValidateSelection refuses an unusable explicit selection before a task is
	// created; RecordIntent then records what the new task should run.
	ValidateSelection(ctx context.Context, projectID domain.ProjectID, sel *domain.PipelineSelection) error
	RecordIntent(ctx context.Context, in IntentInput) (*IntentView, error)
}

// StartInput starts a run on an existing worker.
type StartInput struct {
	SessionID  domain.SessionID `json:"-"`
	WorkflowID string           `json:"workflowId"`
	// RequestedBy is "user" or "orchestrator". Only a user may supply
	// Overrides; an empty value is treated as orchestrator.
	RequestedBy string                   `json:"requestedBy" enum:"user,orchestrator"`
	Overrides   map[string]StageOverride `json:"overrides,omitempty"`
}

// SubmitInput is one structured, idempotent stage result.
type SubmitInput struct {
	SessionID            domain.SessionID `json:"-"`
	RunID                string           `json:"runId"`
	AttemptID            string           `json:"attemptId"`
	ControllerGeneration string           `json:"controllerGeneration"`
	IdempotencyKey       string           `json:"idempotencyKey"`
	Outcome              string           `json:"outcome" enum:"succeeded,failed,production_defect"`
	ExpectedInputCommit  string           `json:"expectedInputCommit"`
	OutputCommit         string           `json:"outputCommit,omitempty"`
	Summary              string           `json:"summary,omitempty"`
	// Report is the specialist's structured result. It is required for a
	// specialist's succeeded or production_defect outcome.
	Report *StageReport `json:"report,omitempty"`
}

// SubmitResult reports what the daemon decided.
type SubmitResult struct {
	Run      RunView     `json:"run"`
	Attempt  AttemptView `json:"attempt"`
	Accepted bool        `json:"accepted"`
	// Replayed is true when this exact result had already been recorded.
	Replayed bool `json:"replayed"`
}

// Deps carries the service collaborators.
type Deps struct {
	Store     Store
	Git       Git
	Messenger Messenger
	// Executor starts, quiesces, and stops stage executors. Without it only
	// single-stage Build workflows can run.
	Executor ports.PipelineExecutor
	// Reporter delivers AO-authored completion and attention reports to the
	// task's orchestrator. Without it runs simply stay quiet.
	Reporter Reporter
	// Reviews adapts AO's built-in review subsystem for Review stages. Without
	// it a workflow with a Review stage cannot start.
	Reviews ReviewGateway
	// Runner executes validation commands; defaults to ExecRunner.
	Runner CommandRunner
	Clock  func() time.Time
	NewID  func(prefix string) string
	Logger *slog.Logger
}

// Service implements Manager and the lifecycle guard.
type Service struct {
	store     Store
	git       Git
	messenger Messenger
	executor  ports.PipelineExecutor
	runner    CommandRunner
	reviews   ReviewGateway
	reporter  Reporter
	waits     sync.Map // review attempt id -> last recorded waiting code
	// validations holds the cancel function of each run's in-flight validation
	// round so a pause or cancel can kill its commands.
	validations sync.Map
	wakeCh      chan struct{}
	driving     sync.Map // run id -> struct{}: at most one handoff driver per run
	clock       func() time.Time
	newID       func(prefix string) string
	logger      *slog.Logger
	locks       sync.Map // session id -> *sync.Mutex
}

var _ Manager = (*Service)(nil)

// New returns a pipeline run service.
func New(d Deps) *Service {
	s := &Service{store: d.Store, git: d.Git, messenger: d.Messenger, executor: d.Executor, runner: d.Runner, reviews: d.Reviews, wakeCh: make(chan struct{}, 1), clock: d.Clock, newID: d.NewID, logger: d.Logger}
	s.reporter = d.Reporter
	if s.reporter != nil {
		s.store = &notifyingStore{Store: d.Store, svc: s}
	}
	if s.git == nil {
		s.git = ExecGit{}
	}
	if s.runner == nil {
		s.runner = ExecRunner{}
	}
	if s.clock == nil {
		s.clock = func() time.Time { return time.Now().UTC() }
	}
	if s.newID == nil {
		s.newID = randomID
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	return s
}

func randomID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("pipelineruns: random id: %v", err))
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}

func (s *Service) lock(id domain.SessionID) func() {
	m, _ := s.locks.LoadOrStore(id, &sync.Mutex{})
	mu, ok := m.(*sync.Mutex)
	if !ok {
		panic("pipelineruns: lock map holds a non-mutex")
	}
	mu.Lock()
	return mu.Unlock
}

// SuppressesLifecycleShortcuts reports whether ordinary automatic review and
// completion/cleanup shortcuts must stay out of the way for this session
// because an unfinished pipeline run still owns its lifecycle. It fails closed:
// when the answer cannot be determined the shortcut is suppressed this round.
func (s *Service) SuppressesLifecycleShortcuts(ctx context.Context, id domain.SessionID) bool {
	has, err := s.store.HasUnfinishedPipelineRun(ctx, id)
	if err != nil {
		s.logger.Error("pipeline: cannot determine run state; suppressing lifecycle shortcut", "session_id", id, "err", err)
		return true
	}
	return has
}

// Start implements Manager.
func (s *Service) Start(ctx context.Context, in StartInput) (RunView, error) {
	requester := domain.PipelineRequester(strings.TrimSpace(in.RequestedBy))
	switch requester {
	case "":
		requester = domain.PipelineRequestedByOrchestrator
	case domain.PipelineRequestedByUser, domain.PipelineRequestedByOrchestrator:
	default:
		return RunView{}, apierr.Invalid("INVALID_PIPELINE_REQUESTER", `requestedBy must be "user" or "orchestrator"`, nil)
	}
	if len(in.Overrides) > 0 && requester != domain.PipelineRequestedByUser {
		return RunView{}, apierr.Forbidden("PIPELINE_OVERRIDE_USER_ONLY", "Only an explicit user choice may override a stage's harness or model; orchestrators use the workflow defaults")
	}
	for stage, ov := range in.Overrides {
		if ov.Harness != "" && !domain.AgentHarness(ov.Harness).IsKnown() {
			return RunView{}, apierr.Invalid("INVALID_PIPELINE_OVERRIDE", fmt.Sprintf("Override for stage %q names unknown harness %q", stage, ov.Harness), nil)
		}
		if ov.Harness == "" && ov.Model == "" {
			return RunView{}, apierr.Invalid("INVALID_PIPELINE_OVERRIDE", fmt.Sprintf("Override for stage %q sets neither harness nor model", stage), nil)
		}
	}
	workflowID := strings.TrimSpace(in.WorkflowID)
	if workflowID == "" {
		return RunView{}, apierr.Invalid("PIPELINE_WORKFLOW_REQUIRED", "workflowId is required: pipeline selection is explicit and never falls back to a normal worker", nil)
	}

	unlock := s.lock(in.SessionID)
	defer unlock()

	session, ok, err := s.store.GetSession(ctx, in.SessionID)
	if err != nil {
		return RunView{}, apierr.Internal("SESSION_LOAD_FAILED", "Failed to load session")
	}
	if !ok {
		return RunView{}, apierr.NotFound("SESSION_NOT_FOUND", "Unknown session")
	}
	if err := checkRunnableWorker(session); err != nil {
		return RunView{}, err
	}
	if has, err := s.store.HasUnfinishedPipelineRun(ctx, in.SessionID); err != nil {
		return RunView{}, apierr.Internal("PIPELINE_LOAD_FAILED", "Failed to check existing pipeline run")
	} else if has {
		return RunView{}, apierr.Conflict("PIPELINE_RUN_ACTIVE", "This task already has an unfinished pipeline run", nil)
	}

	project, ok, err := s.store.GetProject(ctx, string(session.ProjectID))
	if err != nil || !ok {
		return RunView{}, apierr.Internal("PROJECT_LOAD_FAILED", "Failed to load project")
	}
	catalog := pipeline.Discover(project.Path)
	entry, ok := catalog.FindWorkflow(workflowID)
	if !ok {
		return RunView{}, apierr.Invalid("PIPELINE_WORKFLOW_NOT_FOUND", fmt.Sprintf("Workflow %q is not defined in %s", workflowID, pipeline.WorkflowsDir), nil)
	}
	if !entry.Valid || entry.Workflow == nil {
		return RunView{}, apierr.Invalid("PIPELINE_WORKFLOW_INVALID", fmt.Sprintf("Workflow %q has validation errors; fix %s first", workflowID, entry.File), map[string]any{"diagnostics": entry.Diagnostics})
	}
	if reason := pipeline.UnsupportedExecutionReason(*entry.Workflow); reason != "" {
		return RunView{}, apierr.Conflict("PIPELINE_WORKFLOW_UNAVAILABLE", reason, nil)
	}
	if attachedTo, aerr := s.store.GetSessionAttachedTo(ctx, in.SessionID); aerr != nil {
		return RunView{}, apierr.Internal("SESSION_LOAD_FAILED", "Failed to load session")
	} else if attachedTo != "" {
		return RunView{}, apierr.Conflict("PIPELINE_SESSION_UNSUPPORTED", "A pipeline stage session cannot start its own pipeline", nil)
	}
	// Specialist stages run as attached Chat conversations. Refuse up front,
	// visibly, when this build or a stage's harness cannot do that; there is no
	// Terminal fallback.
	for _, st := range entry.Workflow.Stages {
		if st.Kind == pipeline.StageReview && s.reviews == nil {
			return RunView{}, apierr.Conflict("PIPELINE_WORKFLOW_UNAVAILABLE", fmt.Sprintf("Stage %q needs AO's built-in review, which this build cannot drive.", st.ID), nil)
		}
		if st.Kind != pipeline.StageSpecialist {
			continue
		}
		if s.executor == nil {
			return RunView{}, apierr.Conflict("PIPELINE_WORKFLOW_UNAVAILABLE", fmt.Sprintf("Stage %q needs an attached Chat specialist, which this build cannot run.", st.ID), nil)
		}
		harness := stageHarness(catalog, st, in.Overrides)
		if perr := s.executor.PreflightStage(ctx, harness); perr != nil {
			return RunView{}, apierr.Conflict("PIPELINE_STAGE_UNSUPPORTED", fmt.Sprintf("Stage %q cannot run as a Chat specialist (%s): %v", st.ID, harness, perr), nil)
		}
	}

	gitState, err := s.git.Inspect(ctx, session.Metadata.WorkspacePath)
	if err != nil {
		return RunView{}, apierr.Conflict("PIPELINE_WORKSPACE_UNREADABLE", fmt.Sprintf("Cannot read the task workspace: %v", err), nil)
	}
	if gitState.Branch == "" {
		return RunView{}, apierr.Conflict("PIPELINE_DETACHED_HEAD", "The task workspace is on a detached HEAD; check out the task branch before starting a pipeline", nil)
	}

	now := s.clock()
	snap, snapJSON, snapSum, err := buildSnapshot(snapshotInput{
		Workflow: entry, Catalog: catalog, Worker: session, RequestedBy: requester, Overrides: in.Overrides, Now: now,
	})
	if err != nil {
		return RunView{}, apierr.Invalid("INVALID_PIPELINE_OVERRIDE", err.Error(), nil)
	}
	first := snap.Stages[0]
	run := domain.PipelineRun{
		ID: s.newID("prun"), SessionID: in.SessionID, ProjectID: session.ProjectID, WorkflowID: workflowID,
		State: domain.PipelineRunRunning, CurrentStageID: first.ID, RequestedBy: requester,
		ExpectedBranch: gitState.Branch, RepairBudget: snap.Workflow.RepairBudget,
		Snapshot: snapJSON, SnapshotSHA256: snapSum, CreatedAt: now, UpdatedAt: now,
	}
	attempt := domain.PipelineStageAttempt{
		ID: s.newID("pstg"), RunID: run.ID, StageID: first.ID, StageKind: string(first.Kind), AttemptNo: 1,
		ExecutorSessionID: in.SessionID, ControllerGeneration: session.Metadata.ControllerGeneration,
		InputCommit: gitState.Head, StartedAt: now,
	}
	run, attempt, err = s.store.CreatePipelineRun(ctx, run, attempt)
	if errors.Is(err, domain.ErrPipelineRunActive) {
		return RunView{}, apierr.Conflict("PIPELINE_RUN_ACTIVE", "This task already has an unfinished pipeline run", nil)
	}
	if err != nil {
		s.logger.Error("pipeline: create run failed", "session_id", in.SessionID, "err", err)
		return RunView{}, apierr.Internal("PIPELINE_CREATE_FAILED", "Failed to start the pipeline run")
	}
	startDetail := eventDetail{Message: fmt.Sprintf("Started workflow %q at %s on branch %s", workflowID, shortCommit(gitState.Head), gitState.Branch)}
	if gitState.DirtyTotal > 0 {
		startDetail.Message += fmt.Sprintf("; %d uncommitted path(s) were present at start", gitState.DirtyTotal)
		startDetail.Paths = gitState.Dirty
	}
	_ = s.store.AddPipelineEvent(ctx, domain.PipelineEvent{RunID: run.ID, AttemptID: attempt.ID, Kind: eventRunStarted, Detail: startDetail.marshal(), CreatedAt: now})

	s.deliverInstructions(ctx, run, attempt, snap, first)
	return s.view(ctx, run.ID)
}

// checkRunnableWorker enforces what a pipeline may run on in v1: a live,
// fully provisioned Chat worker with a controller and a workspace.
func checkRunnableWorker(session domain.SessionRecord) error {
	unsupported := func(msg string) error { return apierr.Conflict("PIPELINE_SESSION_UNSUPPORTED", msg, nil) }
	switch {
	case session.Kind != domain.KindWorker:
		return unsupported("Pipelines run on worker tasks only")
	case session.IsTerminated:
		return unsupported("The task is terminated")
	case domain.NormalizeSessionMode(session.Mode) != domain.SessionModeChat:
		return unsupported("Pipeline stages are Chat-only in this version; this task is a Terminal session")
	case session.ProvisionState.WithDefault() != domain.SessionProvisionReady:
		return unsupported("The task has not finished starting; try again when it is ready")
	case strings.TrimSpace(session.Metadata.WorkspacePath) == "":
		return unsupported("The task has no workspace yet")
	case session.Metadata.ControllerGeneration == "":
		return unsupported("The task's agent controller has not started yet")
	}
	return nil
}

func (s *Service) deliverInstructions(ctx context.Context, run domain.PipelineRun, attempt domain.PipelineStageAttempt, snap Snapshot, stage SnapshotStage) {
	state, kind, detail := "delivered", eventInstructionDelivered, eventDetail{Message: "Stage instructions delivered to the worker"}
	if s.messenger == nil {
		state, kind, detail = "failed", eventInstructionFailed, eventDetail{Message: "No message channel is available to deliver stage instructions"}
	} else if err := s.messenger.Send(ports.WithPipelineBypass(ctx), run.SessionID, stageInstruction(run, attempt, snap, stage), nil); err != nil {
		state, kind, detail = "failed", eventInstructionFailed, eventDetail{Message: fmt.Sprintf("Stage instructions could not be delivered: %v", err)}
	}
	if err := s.store.SetPipelineAttemptInstructionDelivery(ctx, attempt.ID, state); err != nil {
		s.logger.Error("pipeline: record instruction delivery failed", "run_id", run.ID, "err", err)
	}
	_ = s.store.AddPipelineEvent(ctx, domain.PipelineEvent{RunID: run.ID, AttemptID: attempt.ID, Kind: kind, Detail: detail.marshal(), CreatedAt: s.clock()})
}

// stageInstruction is the coordination message that tells the worker it is now
// the active Build stage and how to hand off. The mandatory workflow
// instructions are part of the snapshot and always included.
func stageInstruction(run domain.PipelineRun, attempt domain.PipelineStageAttempt, snap Snapshot, stage SnapshotStage) string {
	var b strings.Builder
	fmt.Fprintf(&b, "AO pipeline %q started for this task (run %s, stage %q, attempt %s).\n", snap.Workflow.ID, run.ID, stage.ID, attempt.ID)
	fmt.Fprintf(&b, "You remain the owner of this worktree, branch (%s), and PR.\n", run.ExpectedBranch)
	if snap.Workflow.Instructions != "" {
		b.WriteString("\nWorkflow instructions (mandatory):\n")
		b.WriteString(strings.TrimSpace(snap.Workflow.Instructions))
		b.WriteString("\n")
	}
	b.WriteString("\nWhen your work is complete, commit it so the working tree is clean on the task branch, then submit the stage result:\n")
	b.WriteString("  ao pipeline submit --outcome succeeded --summary \"<what you did>\"\n")
	b.WriteString("Use --outcome failed if you cannot complete the stage. ")
	b.WriteString("`ao report` is informational and does not complete a stage; only a verified submission does.\n")
	if _, idx, ok := snap.stage(stage.ID); ok && idx+1 < len(snap.Stages) {
		fmt.Fprintf(&b, "After you submit, stage %q takes over in its own conversation on this same worktree. Stop working once you have submitted: do not edit files or run further commands, and do not wait for it.\n", snap.Stages[idx+1].ID)
	}
	return b.String()
}

// Get implements Manager.
func (s *Service) Get(ctx context.Context, id domain.SessionID) (RunEnvelope, error) {
	id, err := s.ownerOf(ctx, id)
	if err != nil {
		return RunEnvelope{}, err
	}
	if err := s.reconcileSession(ctx, id); err != nil {
		s.logger.Error("pipeline: reconcile failed", "session_id", id, "err", err)
	}
	var intent *IntentView
	if in, found, ierr := s.store.GetPipelineIntent(ctx, id); ierr == nil && found {
		intent = intentView(in)
	}
	run, ok, err := s.store.GetLatestPipelineRunBySession(ctx, id)
	if err != nil {
		return RunEnvelope{}, apierr.Internal("PIPELINE_LOAD_FAILED", "Failed to load pipeline run")
	}
	if !ok {
		return RunEnvelope{Intent: intent}, nil
	}
	view, err := s.view(ctx, run.ID)
	if err != nil {
		return RunEnvelope{}, err
	}
	return RunEnvelope{Run: &view, Intent: intent}, nil
}

func (s *Service) view(ctx context.Context, runID string) (RunView, error) {
	run, ok, err := s.store.GetPipelineRun(ctx, runID)
	if err != nil || !ok {
		return RunView{}, apierr.Internal("PIPELINE_LOAD_FAILED", "Failed to load pipeline run")
	}
	snap, err := parseSnapshot(run.Snapshot)
	if err != nil {
		return RunView{}, apierr.Internal("PIPELINE_SNAPSHOT_CORRUPT", "The pipeline snapshot could not be read")
	}
	attempts, err := s.store.ListPipelineStageAttempts(ctx, runID)
	if err != nil {
		return RunView{}, apierr.Internal("PIPELINE_LOAD_FAILED", "Failed to load pipeline attempts")
	}
	events, err := s.store.ListPipelineEvents(ctx, runID, recentEventLimit)
	if err != nil {
		return RunView{}, apierr.Internal("PIPELINE_LOAD_FAILED", "Failed to load pipeline events")
	}
	commands := map[string][]domain.PipelineCommandResult{}
	for _, a := range attempts {
		results, cerr := s.store.ListPipelineCommandResults(ctx, a.ID)
		if cerr != nil {
			return RunView{}, apierr.Internal("PIPELINE_LOAD_FAILED", "Failed to load validation evidence")
		}
		if len(results) > 0 {
			commands[a.ID] = results
		}
	}
	repairs, err := s.store.ListPipelineRepairs(ctx, runID)
	if err != nil {
		return RunView{}, apierr.Internal("PIPELINE_LOAD_FAILED", "Failed to load pipeline repairs")
	}
	links, err := s.store.ListPipelineReviewLinks(ctx, runID)
	if err != nil {
		return RunView{}, apierr.Internal("PIPELINE_LOAD_FAILED", "Failed to load review evidence")
	}
	grants, err := s.store.ListPipelineRepairGrants(ctx, runID)
	if err != nil {
		return RunView{}, apierr.Internal("PIPELINE_LOAD_FAILED", "Failed to load repair authorizations")
	}
	v := buildRunView(run, snap, attempts, events, commands, repairs, links)
	v.Control = buildControlView(run, events)
	for _, g := range grants {
		v.RepairGrants = append(v.RepairGrants, RepairGrantView{Amount: g.Amount, AuthorizedBy: string(g.AuthorizedBy), Note: g.Note, CreatedAt: g.CreatedAt})
	}
	v.ReviewGate = s.reviewGateFor(ctx, run, attempts)
	return v, nil
}

// Submit implements Manager.
func (s *Service) Submit(ctx context.Context, in SubmitInput) (SubmitResult, error) {
	if err := validateSubmitShape(in); err != nil {
		return SubmitResult{}, err
	}
	// An attached specialist submits under its own session id; the run belongs
	// to the worker that owns the task.
	executorID := in.SessionID
	owner, err := s.ownerOf(ctx, executorID)
	if err != nil {
		return SubmitResult{}, err
	}
	unlock := s.lock(owner)
	defer unlock()

	run, ok, err := s.store.GetPipelineRun(ctx, in.RunID)
	if err != nil {
		return SubmitResult{}, apierr.Internal("PIPELINE_LOAD_FAILED", "Failed to load pipeline run")
	}
	if !ok || run.SessionID != owner {
		return SubmitResult{}, apierr.NotFound("PIPELINE_RUN_NOT_FOUND", "Unknown pipeline run for this task")
	}
	attempt, ok, err := s.store.GetPipelineStageAttempt(ctx, in.AttemptID)
	if err != nil {
		return SubmitResult{}, apierr.Internal("PIPELINE_LOAD_FAILED", "Failed to load pipeline attempt")
	}
	if !ok || attempt.RunID != run.ID {
		return SubmitResult{}, apierr.NotFound("PIPELINE_ATTEMPT_NOT_FOUND", "Unknown stage attempt for this run")
	}
	// A Review attempt has no executor: AO's built-in reviewer and the facts the
	// daemon reads decide it, never an agent's report.
	if attempt.StageKind == string(pipeline.StageReview) {
		return SubmitResult{}, apierr.Forbidden("PIPELINE_STAGE_NOT_SUBMITTABLE", "The Review stage is decided by AO's built-in review and the pull request's checks; it does not take a submitted result")
	}
	// Only the attempt's own executor may report its result. A worker cannot
	// answer for the specialist (or the reverse): that would be a stale or
	// foreign event, however well-formed.
	if attempt.ExecutorSessionID != "" && attempt.ExecutorSessionID != executorID {
		return SubmitResult{}, apierr.Forbidden("PIPELINE_NOT_ATTEMPT_EXECUTOR", "This session is not the executor of that stage attempt")
	}

	// A finished attempt only answers an exact replay; anything else is stale.
	if attempt.State != domain.PipelineAttemptActive {
		if attempt.ResultKey != "" && attempt.ResultKey == in.IdempotencyKey {
			return s.replay(ctx, run, attempt)
		}
		return SubmitResult{}, apierr.Conflict("PIPELINE_ATTEMPT_STALE", fmt.Sprintf("Stage attempt %s is already %s and cannot take another result", attempt.ID, attempt.State), nil)
	}
	if run.State == domain.PipelineRunPaused {
		return SubmitResult{}, apierr.Conflict("PIPELINE_RUN_PAUSED", fmt.Sprintf("The run is paused (%s); results are not accepted until it is resumed", run.PauseReason), nil)
	}
	if run.State != domain.PipelineRunRunning || run.CurrentStageID != attempt.StageID {
		return SubmitResult{}, apierr.Conflict("PIPELINE_ATTEMPT_STALE", "This attempt no longer belongs to the run's current stage", nil)
	}

	// Controller fencing: both the caller and the live session must still be on
	// the generation this attempt started under.
	session, ok, err := s.store.GetSession(ctx, in.SessionID)
	if err != nil {
		return SubmitResult{}, apierr.Internal("SESSION_LOAD_FAILED", "Failed to load session")
	}
	if !ok || session.IsTerminated {
		if perr := s.pause(ctx, run, &attempt, domain.PipelinePauseSessionTerminated, "The task ended while the stage was active"); perr != nil {
			return SubmitResult{}, submitPauseError(perr)
		}
		return SubmitResult{}, apierr.Conflict("PIPELINE_RUN_PAUSED", "The task ended while the stage was active; the run is paused", nil)
	}
	if session.Metadata.ControllerGeneration != attempt.ControllerGeneration {
		if perr := s.pause(ctx, run, &attempt, domain.PipelinePauseControllerChanged, "The worker's controller restarted while the stage was active"); perr != nil {
			return SubmitResult{}, submitPauseError(perr)
		}
		return SubmitResult{}, apierr.Conflict("PIPELINE_RUN_PAUSED", "The worker's controller restarted while the stage was active; the run is paused", nil)
	}
	if in.ControllerGeneration != attempt.ControllerGeneration {
		return SubmitResult{}, s.reject(ctx, run, attempt, "PIPELINE_STALE_GENERATION", "The result was produced under a different controller generation than this attempt", nil)
	}
	if in.ExpectedInputCommit != attempt.InputCommit {
		return SubmitResult{}, s.reject(ctx, run, attempt, "PIPELINE_STALE_REVISION", fmt.Sprintf("The result expects input commit %s but this attempt started from %s", shortCommit(in.ExpectedInputCommit), shortCommit(attempt.InputCommit)), nil)
	}

	// A specialist's pass or defect is a structured claim, validated before
	// anything is decided. Only a specialist may report a production defect.
	if attempt.StageKind == string(pipeline.StageSpecialist) && in.Outcome != OutcomeFailed {
		if in.Report == nil {
			return SubmitResult{}, apierr.Invalid("INVALID_PIPELINE_RESULT", "a specialist stage result needs a structured report (findings, commands, remainingIssues, defects)", nil)
		}
		normalized, rerr := in.Report.validate(in.Outcome)
		if rerr != nil {
			return SubmitResult{}, apierr.Invalid("INVALID_PIPELINE_RESULT", "invalid stage report: "+rerr.Error(), nil)
		}
		in.Report = &normalized
	} else if in.Outcome == OutcomeProductionDefect {
		return SubmitResult{}, apierr.Invalid("INVALID_PIPELINE_RESULT", "only a specialist stage can report a production defect", nil)
	}

	if in.Outcome == OutcomeFailed {
		return s.acceptFailure(ctx, run, attempt, in)
	}
	return s.acceptCommitted(ctx, run, attempt, session, in)
}

func validateSubmitShape(in SubmitInput) error {
	switch {
	case strings.TrimSpace(in.RunID) == "" || strings.TrimSpace(in.AttemptID) == "":
		return apierr.Invalid("INVALID_PIPELINE_RESULT", "runId and attemptId are required", nil)
	case strings.TrimSpace(in.IdempotencyKey) == "" || len(in.IdempotencyKey) > maxIdempotencyKeyLen:
		return apierr.Invalid("INVALID_PIPELINE_RESULT", fmt.Sprintf("idempotencyKey is required (at most %d characters)", maxIdempotencyKeyLen), nil)
	case in.Outcome != OutcomeSucceeded && in.Outcome != OutcomeFailed && in.Outcome != OutcomeProductionDefect:
		return apierr.Invalid("INVALID_PIPELINE_RESULT", `outcome must be "succeeded", "failed", or "production_defect"`, nil)
	case utf8.RuneCountInString(in.Summary) > maxSummaryLen:
		return apierr.Invalid("INVALID_PIPELINE_RESULT", fmt.Sprintf("summary must be at most %d characters", maxSummaryLen), nil)
	case !commitPattern.MatchString(in.ExpectedInputCommit):
		return apierr.Invalid("INVALID_PIPELINE_RESULT", "expectedInputCommit must be a full or abbreviated lowercase commit hash", nil)
	case in.Outcome != OutcomeFailed && !commitPattern.MatchString(in.OutputCommit):
		return apierr.Invalid("INVALID_PIPELINE_RESULT", "a succeeded or production_defect result needs outputCommit, a lowercase commit hash", nil)
	}
	return nil
}

func (s *Service) replay(ctx context.Context, run domain.PipelineRun, attempt domain.PipelineStageAttempt) (SubmitResult, error) {
	view, err := s.view(ctx, run.ID)
	if err != nil {
		return SubmitResult{}, err
	}
	return SubmitResult{Run: view, Attempt: attemptView(attempt, view), Accepted: attempt.State == domain.PipelineAttemptAccepted, Replayed: true}, nil
}

func attemptView(a domain.PipelineStageAttempt, view RunView) AttemptView {
	for _, av := range view.Attempts {
		if av.ID == a.ID {
			return av
		}
	}
	return AttemptView{ID: a.ID, StageID: a.StageID, AttemptNo: a.AttemptNo, State: string(a.State)}
}

// reject records why a submission was refused and returns the API error. The
// attempt stays active and the worktree is left exactly as it is.
func (s *Service) reject(ctx context.Context, run domain.PipelineRun, attempt domain.PipelineStageAttempt, code, message string, paths []string) error {
	detail := eventDetail{Code: code, Message: message, Paths: paths}
	if err := s.store.AddPipelineEvent(ctx, domain.PipelineEvent{RunID: run.ID, AttemptID: attempt.ID, Kind: eventSubmissionRejected, Detail: detail.marshal(), CreatedAt: s.clock()}); err != nil {
		s.logger.Error("pipeline: record rejection failed", "run_id", run.ID, "err", err)
	}
	details := map[string]any{"attemptId": attempt.ID}
	if len(paths) > 0 {
		details["paths"] = paths
	}
	return apierr.Conflict(code, message, details)
}

func reportJSON(r *StageReport) string {
	if r == nil {
		return ""
	}
	return r.marshal()
}

func (s *Service) acceptFailure(ctx context.Context, run domain.PipelineRun, attempt domain.PipelineStageAttempt, in SubmitInput) (SubmitResult, error) {
	now := s.clock()
	reason := "The worker reported that the stage could not be completed"
	if in.Summary != "" {
		reason += ": " + in.Summary
	}
	_, err := s.store.CommitPipelineTransition(ctx, domain.PipelineTransition{
		RunID: run.ID, ExpectedRevision: run.Revision, At: now,
		Attempt: &domain.PipelineAttemptFinish{ID: attempt.ID, State: domain.PipelineAttemptFailed, Outcome: OutcomeFailed, Summary: in.Summary, ResultKey: in.IdempotencyKey, ResultJSON: reportJSON(in.Report), FinishedAt: now},
		Run:     &domain.PipelineRunUpdate{State: domain.PipelineRunPaused, PauseReason: domain.PipelinePauseStageFailed, PauseDetail: reason, CurrentStageID: run.CurrentStageID},
		Events:  []domain.PipelineEvent{{AttemptID: attempt.ID, Kind: eventStageReportedFailure, Detail: eventDetail{Message: reason}.marshal()}},
	})
	return s.finishSubmit(ctx, run, attempt, in, err, false)
}

func (s *Service) acceptCommitted(ctx context.Context, run domain.PipelineRun, attempt domain.PipelineStageAttempt, session domain.SessionRecord, in SubmitInput) (SubmitResult, error) {
	state, err := s.git.Inspect(ctx, session.Metadata.WorkspacePath)
	if err != nil {
		return SubmitResult{}, s.reject(ctx, run, attempt, "PIPELINE_WORKSPACE_UNREADABLE", fmt.Sprintf("The workspace could not be inspected: %v", err), nil)
	}
	switch {
	case state.Branch != run.ExpectedBranch:
		return SubmitResult{}, s.reject(ctx, run, attempt, "PIPELINE_BRANCH_CHANGED", fmt.Sprintf("The workspace is on %q but the run expects branch %q; check out the task branch and submit again", branchLabel(state.Branch), run.ExpectedBranch), nil)
	case state.DirtyTotal > 0:
		return SubmitResult{}, s.reject(ctx, run, attempt, "PIPELINE_DIRTY_WORKSPACE", fmt.Sprintf("The workspace has %d uncommitted path(s). Commit or discard them yourself, then submit again; AO never resets your worktree", state.DirtyTotal), state.Dirty)
	case !strings.HasPrefix(state.Head, in.OutputCommit):
		return SubmitResult{}, s.reject(ctx, run, attempt, "PIPELINE_STALE_REVISION", fmt.Sprintf("The result reports output commit %s but the workspace HEAD is %s", shortCommit(in.OutputCommit), shortCommit(state.Head)), nil)
	}
	noChange := state.Head == attempt.InputCommit
	if !noChange {
		descends, err := s.git.IsAncestor(ctx, session.Metadata.WorkspacePath, attempt.InputCommit, state.Head)
		if err != nil {
			return SubmitResult{}, s.reject(ctx, run, attempt, "PIPELINE_WORKSPACE_UNREADABLE", fmt.Sprintf("History could not be verified: %v", err), nil)
		}
		if !descends {
			return SubmitResult{}, s.reject(ctx, run, attempt, "PIPELINE_NON_LINEAR_HANDOFF", fmt.Sprintf("Commit %s does not descend from the stage's input commit %s; history was rewritten", shortCommit(state.Head), shortCommit(attempt.InputCommit)), nil)
		}
	}
	// A specialist's whole stage-owned diff (every commit since its input
	// checkpoint, including deletions, renames, and symlinks) must stay inside
	// the scope snapshotted at start. Violations are preserved and explained;
	// the stage cannot advance until the net diff is back inside scope.
	snap, perr := parseSnapshot(run.Snapshot)
	if perr != nil {
		return SubmitResult{}, apierr.Internal("PIPELINE_SNAPSHOT_CORRUPT", "The pipeline snapshot could not be read")
	}
	if attempt.StageKind == string(pipeline.StageSpecialist) && !noChange {
		if rejection := s.checkScope(ctx, session.Metadata.WorkspacePath, snap, attempt, state.Head); rejection != nil {
			return SubmitResult{}, s.reject(ctx, run, attempt, rejection.code, rejection.message, rejection.paths)
		}
	}
	if in.Outcome == OutcomeProductionDefect {
		return s.recordProductionDefect(ctx, run, snap, attempt, in, state.Head, noChange)
	}
	if needsValidation(snap, attempt, in) {
		return s.beginValidation(ctx, run, attempt, in, state.Head, noChange)
	}
	now := s.clock()
	err = s.commitAcceptance(ctx, run, attempt, snap, &domain.PipelineAttemptFinish{
		ID: attempt.ID, State: domain.PipelineAttemptAccepted, OutputCommit: state.Head, NoChange: noChange,
		Outcome: OutcomeSucceeded, Summary: in.Summary, ResultKey: in.IdempotencyKey, ResultJSON: reportJSON(in.Report), FinishedAt: now,
	})
	return s.finishSubmit(ctx, run, attempt, in, err, true)
}

// commitAcceptance records an accepted stage and moves the run on: to a
// successor waiting in `handoff`, or to completion when this was the last stage.
// Every acceptance path (a direct result, or one that passed independent
// validation) goes through here so they cannot diverge.
func (s *Service) commitAcceptance(ctx context.Context, run domain.PipelineRun, attempt domain.PipelineStageAttempt, snap Snapshot, fin *domain.PipelineAttemptFinish) error {
	now := fin.FinishedAt
	head := fin.OutputCommit
	message := fmt.Sprintf("Stage %q accepted at %s", attempt.StageID, shortCommit(head))
	if fin.NoChange {
		message = fmt.Sprintf("Stage %q accepted with no new commits (input commit retained: %s)", attempt.StageID, shortCommit(head))
	}
	events := []domain.PipelineEvent{{AttemptID: attempt.ID, Kind: eventStageAccepted, Detail: eventDetail{Message: message}.marshal()}}
	update := &domain.PipelineRunUpdate{State: domain.PipelineRunRunning, CurrentStageID: run.CurrentStageID}
	var successor *domain.PipelineStageAttempt
	nextStage, hasNext := snap.nextStage(attempt)
	if hasNext {
		next := nextStage
		update.CurrentStageID = next.ID
		// The successor starts in `handoff`: its input revision and predecessor
		// are durable, but nothing executes until the predecessor is proven
		// quiescent and the successor's executor is confirmed started.
		successor = &domain.PipelineStageAttempt{
			ID: s.newID("pstg"), RunID: run.ID, StageID: next.ID, StageKind: string(next.Kind),
			AttemptNo: s.nextAttemptNo(ctx, run.ID, next.ID), State: domain.PipelineAttemptHandoff,
			InputCommit: head, PredecessorAttemptID: attempt.ID, StartedAt: now,
		}
		events = append(events, domain.PipelineEvent{AttemptID: successor.ID, Kind: eventHandoffStarted, Detail: eventDetail{Message: fmt.Sprintf("Handing off to stage %q at %s", next.ID, shortCommit(head))}.marshal()})
	} else {
		// No further stage: the run is complete. Ordinary lifecycle shortcuts
		// resume because the pipeline has reached its end.
		update = &domain.PipelineRunUpdate{State: domain.PipelineRunCompleted, CurrentStageID: "", CompletedAt: &now}
		events = append(events, domain.PipelineEvent{AttemptID: attempt.ID, Kind: eventRunCompleted, Detail: eventDetail{Message: "All stages accepted; the pipeline run is complete"}.marshal()})
	}
	_, err := s.store.CommitPipelineTransition(ctx, domain.PipelineTransition{
		RunID: run.ID, ExpectedRevision: run.Revision, At: now, Run: update, Events: events, NewAttempt: successor, Attempt: fin,
	})
	if err == nil && successor != nil {
		// The submitting executor may still be mid-turn (blocked inside its own
		// request), so the handoff must run after we answer, never inside it.
		s.wake()
	}
	if err == nil && successor == nil {
		// The run is complete: hand the worker's conversation back. Its intake
		// was closed at the first handoff and stays closed until someone reopens it.
		s.releaseOwner(ctx, run.SessionID)
	}
	return err
}

// nextAttemptNo numbers the next attempt of a stage within a run.
func (s *Service) nextAttemptNo(ctx context.Context, runID, stageID string) int {
	attempts, err := s.store.ListPipelineStageAttempts(ctx, runID)
	if err != nil {
		return 1
	}
	n := 0
	for _, a := range attempts {
		if a.StageID == stageID && a.AttemptNo > n {
			n = a.AttemptNo
		}
	}
	return n + 1
}

// finishSubmit turns the outcome of the compare-and-set into the response. A
// lost race means another writer already decided this attempt; if that writer
// recorded the very same result key this is a replay, otherwise it is stale.
func (s *Service) finishSubmit(ctx context.Context, run domain.PipelineRun, attempt domain.PipelineStageAttempt, in SubmitInput, err error, accepted bool) (SubmitResult, error) {
	if err != nil {
		if errors.Is(err, domain.ErrPipelineConflict) {
			if current, ok, gerr := s.store.GetPipelineStageAttempt(ctx, attempt.ID); gerr == nil && ok && current.State != domain.PipelineAttemptActive {
				if current.ResultKey == in.IdempotencyKey {
					return s.replay(ctx, run, current)
				}
				return SubmitResult{}, apierr.Conflict("PIPELINE_RESULT_CONFLICT", "A different result was already recorded for this attempt", nil)
			}
			return SubmitResult{}, apierr.Conflict("PIPELINE_CONFLICT", "The pipeline run changed concurrently; read it and submit again", nil)
		}
		s.logger.Error("pipeline: commit result failed", "run_id", run.ID, "err", err)
		return SubmitResult{}, apierr.Internal("PIPELINE_COMMIT_FAILED", "Failed to record the stage result")
	}
	view, verr := s.view(ctx, run.ID)
	if verr != nil {
		return SubmitResult{}, verr
	}
	final, _, _ := s.store.GetPipelineStageAttempt(ctx, attempt.ID)
	return SubmitResult{Run: view, Attempt: attemptView(final, view), Accepted: accepted}, nil
}

// pause persists an operational pause. It never consumes the repair budget and
// never touches the worktree. A lost compare-and-set (the run changed under us)
// is returned as domain.ErrPipelineConflict: the run may not be paused, so a
// caller that reports "paused" must not do so on a conflict.
func (s *Service) pause(ctx context.Context, run domain.PipelineRun, attempt *domain.PipelineStageAttempt, reason domain.PipelinePauseReason, detail string) error {
	now := s.clock()
	t := domain.PipelineTransition{
		RunID: run.ID, ExpectedRevision: run.Revision, At: now,
		Run:    &domain.PipelineRunUpdate{State: domain.PipelineRunPaused, PauseReason: reason, PauseDetail: detail, CurrentStageID: run.CurrentStageID},
		Events: []domain.PipelineEvent{{Kind: eventRunPaused, Detail: eventDetail{Code: string(reason), Message: detail}.marshal()}},
	}
	if attempt != nil {
		t.Events[0].AttemptID = attempt.ID
		// A handoff attempt stays as it is so a later resume can retry the
		// handoff; only an executing attempt is closed as interrupted.
		if attempt.State == domain.PipelineAttemptActive {
			t.Attempt = &domain.PipelineAttemptFinish{ID: attempt.ID, State: domain.PipelineAttemptInterrupted, Outcome: "interrupted", FinishedAt: now}
		}
	}
	if _, err := s.store.CommitPipelineTransition(ctx, t); err != nil {
		if errors.Is(err, domain.ErrPipelineConflict) {
			return err
		}
		s.logger.Error("pipeline: pause failed", "run_id", run.ID, "err", err)
		return apierr.Internal("PIPELINE_PAUSE_FAILED", "Failed to pause the pipeline run")
	}
	return nil
}

// pauseOrYield is pause for the background drivers: losing the compare-and-set
// means another writer (a control request, a cancel, a newer pass) already
// decided the run's fate, and the next pass reads that result instead of
// overwriting it.
func (s *Service) pauseOrYield(ctx context.Context, run domain.PipelineRun, attempt *domain.PipelineStageAttempt, reason domain.PipelinePauseReason, detail string) error {
	if err := s.pause(ctx, run, attempt, reason, detail); err != nil && !errors.Is(err, domain.ErrPipelineConflict) {
		return err
	}
	return nil
}

// submitPauseError turns a lost pause race inside Submit into the same stale
// answer other lost races give, instead of claiming the run is paused.
func submitPauseError(err error) error {
	if errors.Is(err, domain.ErrPipelineConflict) {
		return apierr.Conflict("PIPELINE_ATTEMPT_STALE", "The run changed while the result was being processed; read the task's current run and try again", nil)
	}
	return err
}

// reconcileSession pauses a running run whose executor can no longer prove it
// is the one that started the active attempt. Until recovery support lands,
// interrupted execution pauses safely instead of being replayed.
func (s *Service) reconcileSession(ctx context.Context, id domain.SessionID) error {
	unlock := s.lock(id)
	defer unlock()
	run, ok, err := s.store.GetActivePipelineRunBySession(ctx, id)
	if err != nil || !ok || run.State != domain.PipelineRunRunning {
		return err
	}
	return s.reconcileRun(ctx, run, false, "")
}

func (s *Service) reconcileRun(ctx context.Context, run domain.PipelineRun, announce bool, note string) error {
	attempts, err := s.store.ListPipelineStageAttempts(ctx, run.ID)
	if err != nil {
		return err
	}
	var active, handoff, validating *domain.PipelineStageAttempt
	for i := range attempts {
		switch attempts[i].State {
		case domain.PipelineAttemptActive:
			active = &attempts[i]
		case domain.PipelineAttemptHandoff:
			handoff = &attempts[i]
		case domain.PipelineAttemptValidating:
			validating = &attempts[i]
		}
	}
	switch {
	case active == nil && handoff != nil:
		s.recordRecovery(ctx, run, announce, handoff.ID, "retrying", fmt.Sprintf("After a restart, the handoff to stage %q was still pending; AO will retry it and adopt the stage session already created for it instead of starting a second one%s", handoff.StageID, note))
		return nil // the driver owns a handoff in flight
	case active == nil && validating != nil:
		return nil // reconcileValidation reports it
	case active == nil:
		return nil
	}
	if active.StageKind == string(pipeline.StageReview) {
		s.recordRecovery(ctx, run, announce, active.ID, "continued", fmt.Sprintf("After a restart, the review of revision %s continues from durable pull request and review facts; nothing was replayed%s", shortCommit(active.InputCommit), note))
		return nil // Review has no executor; the review driver evaluates it from durable facts
	}
	session, ok, err := s.store.GetSession(ctx, active.ExecutorSessionID)
	if err != nil {
		return err
	}
	switch {
	case !ok || session.IsTerminated:
		return s.pauseOrYield(ctx, run, active, domain.PipelinePauseSessionTerminated, "The stage's executor ended while the stage was active")
	case session.Metadata.ControllerGeneration != active.ControllerGeneration:
		return s.pauseOrYield(ctx, run, active, domain.PipelinePauseControllerChanged, "The stage executor's controller restarted while the stage was active")
	}
	s.recordRecovery(ctx, run, announce, active.ID, "continued", fmt.Sprintf("After a restart, stage %q (attempt %d) is still attached to the same controller generation; AO did not start or replay anything%s", active.StageID, active.AttemptNo, note))
	return nil
}

// reconnectSurvivors lets the executors a run needs reattach to provider hosts
// that outlived the previous daemon. It only adopts what is still running: it
// never launches anything, and a host that is not running is reported, not read
// as proof the executor is dead (resume asks a person if it matters).
func (s *Service) reconnectSurvivors(ctx context.Context, run domain.PipelineRun) string {
	if s.executor == nil {
		return ""
	}
	attempts, err := s.store.ListPipelineStageAttempts(ctx, run.ID)
	if err != nil {
		return ""
	}
	var ids []domain.SessionID
	add := func(id domain.SessionID) {
		if id == "" || slices.Contains(ids, id) {
			return
		}
		ids = append(ids, id)
	}
	for _, a := range attempts {
		switch {
		case a.StageKind == string(pipeline.StageReview):
		case a.State == domain.PipelineAttemptActive:
			add(a.ExecutorSessionID)
		case a.State == domain.PipelineAttemptHandoff && a.StageKind == string(pipeline.StageBuild):
			add(run.SessionID)
		case a.State == domain.PipelineAttemptHandoff:
			if id := s.retainedStageSession(attempts, a.StageID); id != "" {
				add(id)
			} else if id, found, ferr := s.store.FindAttachedSessionForAttempt(ctx, a.ID); ferr == nil && found {
				add(id)
			}
		}
	}
	var reconnected, missing []string
	for _, id := range ids {
		ok, rerr := s.executor.ReconnectExecutor(ports.WithPipelineBypass(ctx), id)
		switch {
		case rerr != nil:
			missing = append(missing, fmt.Sprintf("%s (%v)", id, rerr))
		case ok:
			reconnected = append(reconnected, string(id))
		default:
			missing = append(missing, string(id))
		}
	}
	var note string
	if len(reconnected) > 0 {
		note += "; reconnected to the surviving controller of " + strings.Join(reconnected, ", ")
	}
	if len(missing) > 0 {
		note += "; no surviving controller could be adopted for " + strings.Join(missing, ", ") + " (that is not proof it is gone, and nothing was started in its place)"
	}
	return note
}

// recordRecovery appends a reconciliation outcome to the run's own history.
func (s *Service) recordRecovery(ctx context.Context, run domain.PipelineRun, announce bool, attemptID, outcome, message string) {
	if !announce {
		return // ordinary reads re-check ownership but only a restart reports an outcome
	}
	if err := s.store.AddPipelineEvent(ctx, domain.PipelineEvent{RunID: run.ID, AttemptID: attemptID, Kind: eventRecovery, CreatedAt: s.clock(), Detail: eventDetail{Code: outcome, Message: message}.marshal()}); err != nil {
		s.logger.Error("pipeline: record recovery outcome failed", "run_id", run.ID, "err", err)
	}
}

// ReconcileAll applies reconcileRun to every running run. The daemon calls it at
// startup so interrupted execution is paused before anything else touches it.
func (s *Service) ReconcileAll(ctx context.Context) error {
	runs, err := s.store.ListUnfinishedPipelineRuns(ctx)
	if err != nil {
		return err
	}
	var firstErr error
	for _, run := range runs {
		if run.State != domain.PipelineRunRunning {
			continue
		}
		unlock := s.lock(run.SessionID)
		note := s.reconnectSurvivors(ctx, run)
		err := s.reconcileValidation(ctx, run)
		if err == nil {
			if current, ok, gerr := s.store.GetPipelineRun(ctx, run.ID); gerr == nil && ok && current.State == domain.PipelineRunRunning {
				err = s.reconcileRun(ctx, current, true, note)
			}
		}
		unlock()
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func shortCommit(c string) string {
	if len(c) > 10 {
		return c[:10]
	}
	if c == "" {
		return "(none)"
	}
	return c
}

func branchLabel(b string) string {
	if b == "" {
		return "a detached HEAD"
	}
	return b
}

// UnfinishedRunChecker is the single store query the guard needs.
type UnfinishedRunChecker interface {
	HasUnfinishedPipelineRun(ctx context.Context, id domain.SessionID) (bool, error)
	GetActivePipelineRunBySession(ctx context.Context, id domain.SessionID) (domain.PipelineRun, bool, error)
	ListPipelineStageAttempts(ctx context.Context, runID string) ([]domain.PipelineStageAttempt, error)
}

// StoreGuard implements ports.PipelineGuard directly over the store, so
// lifecycle components can be wired before the run service exists.
type StoreGuard struct {
	store  UnfinishedRunChecker
	logger *slog.Logger
}

var _ ports.PipelineGuard = (*StoreGuard)(nil)
var _ ports.PipelineGuard = (*Service)(nil)

// NewStoreGuard returns a guard backed by store.
func NewStoreGuard(store UnfinishedRunChecker, logger *slog.Logger) *StoreGuard {
	if logger == nil {
		logger = slog.Default()
	}
	return &StoreGuard{store: store, logger: logger}
}

// SuppressesLifecycleShortcuts implements ports.PipelineGuard.
func (g *StoreGuard) SuppressesLifecycleShortcuts(ctx context.Context, id domain.SessionID) bool {
	has, err := g.store.HasUnfinishedPipelineRun(ctx, id)
	if err != nil {
		g.logger.Error("pipeline: cannot determine run state; suppressing lifecycle shortcut", "session_id", id, "err", err)
		return true
	}
	return has
}

// ReviewTriggerAllowed implements ports.PipelineReviewGuard.
func (g *StoreGuard) ReviewTriggerAllowed(ctx context.Context, id domain.SessionID) (bool, string) {
	return reviewTriggerAllowed(ctx, g.store, id, g.logger.Error)
}

// stageHarness resolves the harness a specialist stage will use at start: an
// explicit user override, else the profile default.
func stageHarness(cat pipeline.Catalog, st pipeline.Stage, overrides map[string]StageOverride) domain.AgentHarness {
	if ov, ok := overrides[st.ID]; ok && ov.Harness != "" {
		return domain.AgentHarness(ov.Harness)
	}
	if pe, ok := cat.FindProfile(st.Profile); ok && pe.Profile != nil {
		return domain.AgentHarness(pe.Profile.Harness)
	}
	return ""
}

type scopeRejection struct {
	code    string
	message string
	paths   []string
}

// checkScope validates a specialist's stage diff and returns the rejection to
// report, or nil when every changed path is in scope.
func (s *Service) checkScope(ctx context.Context, workspace string, snap Snapshot, attempt domain.PipelineStageAttempt, head string) *scopeRejection {
	stage, _, found := snap.stage(attempt.StageID)
	if !found {
		return &scopeRejection{code: "PIPELINE_SCOPE_UNVERIFIABLE", message: fmt.Sprintf("Stage %q is not in the run's snapshot, so its change scope cannot be verified", attempt.StageID)}
	}
	var allowed []string
	for _, p := range snap.Profiles {
		if p.ID == stage.Profile {
			allowed = p.AllowedPaths
		}
	}
	entries, err := s.git.DiffEntries(ctx, workspace, attempt.InputCommit, head)
	if err != nil {
		return &scopeRejection{code: "PIPELINE_SCOPE_UNVERIFIABLE", message: fmt.Sprintf("The stage's changes could not be listed, so its scope cannot be verified: %v", err)}
	}
	violations, total := validateScope(entries, allowed, func(sha string) (string, error) {
		return s.git.ReadBlob(ctx, workspace, sha)
	})
	if total == 0 {
		return nil
	}
	paths := make([]string, 0, len(violations))
	for _, v := range violations {
		paths = append(paths, v.Path+": "+v.Reason)
	}
	return &scopeRejection{
		code:    "PIPELINE_SCOPE_VIOLATION",
		message: fmt.Sprintf("%d changed path(s) are outside stage %q's allowed scope. The commits are preserved; make the net change since %s stay within the allowed paths (for example, commit a revert) and submit again. Path validation is a hand-off constraint, not a sandbox", total, stage.ID, shortCommit(attempt.InputCommit)),
		paths:   paths,
	}
}

// recordProductionDefect closes a specialist attempt that found a defect in
// production code. The defect is reported for return to Build and nothing here
// fixes it. When the stage declares a repair route and the shared budget has
// room, the same atomic change that records the defect counts the return and
// creates the Build attempt; otherwise the run pauses with the retained result.
func (s *Service) recordProductionDefect(ctx context.Context, run domain.PipelineRun, snap Snapshot, attempt domain.PipelineStageAttempt, in SubmitInput, head string, noChange bool) (SubmitResult, error) {
	now := s.clock()
	detail := fmt.Sprintf("Stage %q found %d production defect(s) for Build to fix", attempt.StageID, len(in.Report.Defects))
	if len(in.Report.Defects) > 0 {
		detail += ": " + in.Report.Defects[0].Description
	}
	t := domain.PipelineTransition{
		RunID: run.ID, ExpectedRevision: run.Revision, At: now,
		Attempt: &domain.PipelineAttemptFinish{ID: attempt.ID, State: domain.PipelineAttemptFailed, OutputCommit: head, NoChange: noChange, Outcome: OutcomeProductionDefect, Summary: in.Summary, ResultKey: in.IdempotencyKey, ResultJSON: reportJSON(in.Report), FinishedAt: now},
		Events:  []domain.PipelineEvent{{AttemptID: attempt.ID, Kind: eventProductionDefect, Detail: eventDetail{Code: OutcomeProductionDefect, Message: detail}.marshal()}},
	}
	fb := defectFeedback(attempt, head, in.Report)
	plan, exhausted := s.planRepair(ctx, run, snap, attempt, domain.PipelineRepairProductionDefect, fb, head)
	s.applyRepairOrPause(&t, run, plan, exhausted, PauseProductionDefect, detail, fb)
	_, err := s.store.CommitPipelineTransition(ctx, t)
	if err == nil && plan != nil {
		s.wake()
	}
	return s.finishSubmit(ctx, run, attempt, in, err, false)
}

// applyRepairOrPause finishes a failing-stage transition one of three ways: a
// counted return to Build, a pause because the shared budget is spent, or the
// stage's ordinary pause when it declares no repair route.
func (s *Service) applyRepairOrPause(t *domain.PipelineTransition, run domain.PipelineRun, plan *repairPlan, exhausted string, defaultReason domain.PipelinePauseReason, defaultDetail string, fb Feedback) {
	switch {
	case plan != nil:
		t.Run, t.NewAttempt, t.Repair = &plan.update, &plan.build, &plan.repair
		t.Events = append(t.Events, plan.event)
	case exhausted != "":
		// Keep the revision-bound feedback on the failing attempt so a repair a
		// person later authorizes still carries it.
		if t.Attempt != nil {
			t.Attempt.FeedbackJSON = fb.marshal()
		}
		t.Run = &domain.PipelineRunUpdate{State: domain.PipelineRunPaused, PauseReason: PauseRepairBudgetExhausted, PauseDetail: exhausted + ". " + defaultDetail, CurrentStageID: run.CurrentStageID}
		t.Events = append(t.Events, domain.PipelineEvent{Kind: eventRunPaused, Detail: eventDetail{Code: string(PauseRepairBudgetExhausted), Message: exhausted}.marshal()})
	default:
		t.Run = &domain.PipelineRunUpdate{State: domain.PipelineRunPaused, PauseReason: defaultReason, PauseDetail: defaultDetail, CurrentStageID: run.CurrentStageID}
	}
}

// reconcileValidation runs once at startup, before the driver does. A command
// still marked running belonged to a previous process: its outcome is unknown,
// so it is recorded as such and the run pauses. AO never infers that it passed
// and never runs it again without a person resuming the run.
func (s *Service) reconcileValidation(ctx context.Context, run domain.PipelineRun) error {
	n, err := s.store.MarkRunningPipelineCommandsUnknown(ctx, run.ID, s.clock(), "AO stopped while this command was running; its outcome is unknown")
	if err != nil || n == 0 {
		return err
	}
	attempts, err := s.store.ListPipelineStageAttempts(ctx, run.ID)
	if err != nil {
		return err
	}
	var validating *domain.PipelineStageAttempt
	for i := range attempts {
		if attempts[i].State == domain.PipelineAttemptValidating {
			validating = &attempts[i]
		}
	}
	return s.pauseOrYield(ctx, run, validating, PauseValidationInterrupted, fmt.Sprintf("AO stopped while %d validation command(s) were running; their outcomes are unknown and they were not retried", n))
}
