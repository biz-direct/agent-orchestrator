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
)

// intentMaxWait bounds how long a selected pipeline waits for its worker to be
// provisioned with a controller before the intent fails visibly.
const intentMaxWait = 15 * time.Minute

// IntentInput is a task creation's pipeline selection.
type IntentInput struct {
	SessionID domain.SessionID
	ProjectID domain.ProjectID
	// Explicit is the caller's choice: a workflow, or the normal worker. Nil
	// means "use the project default".
	Explicit *domain.PipelineSelection
	// RequestedBy is "user" for a person's task creation and "orchestrator" for
	// an agent's. Neither may carry stage overrides here; those stay user-only
	// on `ao pipeline start`.
	RequestedBy domain.PipelineRequester
}

// IntentView is a task's creation-time pipeline selection and what became of it.
type IntentView struct {
	WorkflowID string `json:"workflowId,omitempty"`
	// NormalWorker is true for the explicit normal-worker override.
	NormalWorker bool   `json:"normalWorker"`
	Source       string `json:"source" enum:"explicit,default"`
	State        string `json:"state" enum:"pending,started,skipped,failed"`
	Detail       string `json:"detail,omitempty"`
	RunID        string `json:"runId,omitempty"`
}

func intentView(in domain.PipelineIntent) *IntentView {
	return &IntentView{
		WorkflowID: in.WorkflowID, NormalWorker: in.WorkflowID == "", Source: string(in.Source),
		State: string(in.State), Detail: in.Detail, RunID: in.RunID,
	}
}

// ValidateSelection checks an explicit selection before a task is created, so
// an unusable choice fails the request instead of silently degrading into a
// normal worker. A normal-worker selection is always valid.
func (s *Service) ValidateSelection(ctx context.Context, projectID domain.ProjectID, sel *domain.PipelineSelection) error {
	if sel == nil {
		return nil
	}
	switch sel.Mode {
	case domain.PipelineModeNormalWorker:
		if strings.TrimSpace(sel.WorkflowID) != "" {
			return apierr.Invalid("INVALID_PIPELINE_SELECTION", "a normal-worker selection takes no workflowId", nil)
		}
		return nil
	case domain.PipelineModeWorkflow:
	default:
		return apierr.Invalid("INVALID_PIPELINE_SELECTION", `pipeline mode must be "workflow" or "normal_worker"`, nil)
	}
	id := strings.TrimSpace(sel.WorkflowID)
	if id == "" {
		return apierr.Invalid("INVALID_PIPELINE_SELECTION", "a workflow selection needs a workflowId", nil)
	}
	project, ok, err := s.store.GetProject(ctx, string(projectID))
	if err != nil {
		return apierr.Internal("PROJECT_LOAD_FAILED", "Failed to load project")
	}
	if !ok {
		return apierr.NotFound("PROJECT_NOT_FOUND", "Unknown project")
	}
	catalog := pipeline.Discover(project.Path)
	entry, found := catalog.FindWorkflow(id)
	switch {
	case !found:
		return apierr.Invalid("PIPELINE_WORKFLOW_NOT_FOUND", fmt.Sprintf("Workflow %q is not defined in %s", id, pipeline.WorkflowsDir), nil)
	case !entry.Valid || entry.Workflow == nil:
		return apierr.Invalid("PIPELINE_WORKFLOW_INVALID", fmt.Sprintf("Workflow %q has validation errors; fix %s first", id, entry.File), map[string]any{"diagnostics": entry.Diagnostics})
	}
	if reason := pipeline.UnsupportedExecutionReason(*entry.Workflow); reason != "" {
		return apierr.Conflict("PIPELINE_WORKFLOW_UNAVAILABLE", reason, nil)
	}
	return nil
}

// RecordIntent resolves and records the pipeline a new task should run: an
// explicit selection wins, otherwise the project's default workflow applies,
// otherwise the task is an ordinary worker and nothing is recorded. It never
// blocks the task from existing: when the default cannot run the intent records
// why. Repeating it for the same task keeps the original.
func (s *Service) RecordIntent(ctx context.Context, in IntentInput) (*IntentView, error) {
	requester := in.RequestedBy
	if requester == "" {
		requester = domain.PipelineRequestedByOrchestrator
	}
	now := s.clock()
	rec := domain.PipelineIntent{SessionID: in.SessionID, ProjectID: in.ProjectID, RequestedBy: requester, State: domain.PipelineIntentPending, CreatedAt: now, UpdatedAt: now}

	switch {
	case in.Explicit != nil:
		rec.Source = domain.PipelineIntentExplicit
		if in.Explicit.Mode == domain.PipelineModeNormalWorker {
			rec.State, rec.Detail = domain.PipelineIntentSkipped, "A normal worker was selected for this task"
			if def, derr := s.projectDefault(ctx, in.ProjectID); derr == nil && def != "" {
				rec.Detail += fmt.Sprintf(" instead of the project default workflow %q", def)
			}
		} else {
			rec.WorkflowID = strings.TrimSpace(in.Explicit.WorkflowID)
		}
	default:
		def, err := s.projectDefault(ctx, in.ProjectID)
		if err != nil {
			return nil, err
		}
		if def == "" {
			return nil, nil
		}
		rec.Source, rec.WorkflowID = domain.PipelineIntentDefault, def
	}

	session, ok, err := s.store.GetSession(ctx, in.SessionID)
	if err != nil {
		return nil, apierr.Internal("SESSION_LOAD_FAILED", "Failed to load session")
	}
	if ok && rec.State == domain.PipelineIntentPending {
		switch {
		case session.Kind != domain.KindWorker:
			return nil, nil // orchestrators never run pipelines
		case domain.NormalizeSessionMode(session.Mode) != domain.SessionModeChat:
			rec.State = domain.PipelineIntentSkipped
			if rec.Source == domain.PipelineIntentExplicit {
				rec.State = domain.PipelineIntentFailed
			}
			rec.Detail = "Pipeline stages are Chat-only in this version; this task is a Terminal session, so it runs as a normal worker"
		}
	}
	if err := s.store.CreatePipelineIntent(ctx, rec); err != nil {
		s.logger.Error("pipeline: record intent failed", "session_id", in.SessionID, "err", err)
		return nil, apierr.Internal("PIPELINE_INTENT_FAILED", "Failed to record the task's pipeline selection")
	}
	stored, found, err := s.store.GetPipelineIntent(ctx, in.SessionID)
	if err != nil || !found {
		return nil, err
	}
	s.wake()
	return intentView(stored), nil
}

// projectDefault returns the project's default workflow id, or "" when the
// project has none (or defaults to the normal worker).
func (s *Service) projectDefault(ctx context.Context, projectID domain.ProjectID) (string, error) {
	project, ok, err := s.store.GetProject(ctx, string(projectID))
	if err != nil {
		return "", apierr.Internal("PROJECT_LOAD_FAILED", "Failed to load project")
	}
	if !ok || project.Config.DefaultPipeline == nil || project.Config.DefaultPipeline.Mode != domain.PipelineModeWorkflow {
		return "", nil
	}
	return strings.TrimSpace(project.Config.DefaultPipeline.WorkflowID), nil
}

// StartPendingIntents starts every selected pipeline whose worker is ready. A
// worker that is still being provisioned waits; one that ended, already has a
// run, or cannot run the workflow settles with the reason, so a selection never
// lingers and never degrades silently.
func (s *Service) StartPendingIntents(ctx context.Context) error {
	intents, err := s.store.ListPendingPipelineIntents(ctx)
	if err != nil {
		return err
	}
	var firstErr error
	for _, in := range intents {
		if err := s.startIntent(ctx, in); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (s *Service) startIntent(ctx context.Context, in domain.PipelineIntent) error {
	settle := func(state domain.PipelineIntentState, detail, runID string) error {
		_, err := s.store.SettlePipelineIntent(ctx, in.SessionID, state, detail, runID, s.clock())
		return err
	}
	session, ok, err := s.store.GetSession(ctx, in.SessionID)
	if err != nil {
		return err
	}
	switch {
	case !ok || session.IsTerminated:
		return settle(domain.PipelineIntentSkipped, "The task ended before its pipeline could start", "")
	case in.WorkflowID == "":
		return settle(domain.PipelineIntentSkipped, "No workflow was selected", "")
	}
	if has, herr := s.store.HasUnfinishedPipelineRun(ctx, in.SessionID); herr != nil {
		return herr
	} else if has {
		return settle(domain.PipelineIntentSkipped, "The task already has a pipeline run", "")
	}
	if cerr := checkRunnableWorker(session); cerr != nil {
		if s.clock().Sub(in.CreatedAt) > intentMaxWait {
			return settle(domain.PipelineIntentFailed, fmt.Sprintf("The task did not become ready to run workflow %q: %s", in.WorkflowID, cerr), "")
		}
		return nil // still being provisioned; try again on the next pass
	}
	view, serr := s.Start(ctx, StartInput{SessionID: in.SessionID, WorkflowID: in.WorkflowID, RequestedBy: string(in.RequestedBy)})
	if serr != nil {
		var ae *apierr.Error
		if errors.As(serr, &ae) && ae.Code == "PIPELINE_RUN_ACTIVE" {
			return settle(domain.PipelineIntentSkipped, "The task already has a pipeline run", "")
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return settle(domain.PipelineIntentFailed, fmt.Sprintf("Workflow %q could not start: %s", in.WorkflowID, serr), "")
	}
	return settle(domain.PipelineIntentStarted, fmt.Sprintf("Started workflow %q", in.WorkflowID), view.ID)
}
