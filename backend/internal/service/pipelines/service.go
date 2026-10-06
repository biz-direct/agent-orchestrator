// Package pipelines exposes repository-defined specialist profiles and
// sequential workflows to the daemon API: the validated catalog and the
// project-default pipeline selection.
//
// Definitions are read from the project's repository files on every call, so
// repository edits show up immediately; only the selection reference is stored.
package pipelines

import (
	"context"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/pipeline"
)

// Default-selection states reported to clients.
const (
	StateUnset                = "unset"
	StateNormalWorker         = "normal_worker"
	StateWorkflowAvailable    = "workflow_available"
	StateWorkflowUnavailable  = "workflow_unavailable"
	StateWorkflowInvalid      = "workflow_invalid"
	StateWorkflowMissing      = "workflow_missing"
	maxDiagnosticsInErrorBody = 20
)

// Store is the durable project surface the service needs.
type Store interface {
	GetProject(ctx context.Context, id string) (domain.ProjectRecord, bool, error)
	SetProjectDefaultPipeline(ctx context.Context, id string, selection *domain.PipelineSelection) (domain.ProjectRecord, bool, error)
}

// Manager is the controller-facing contract.
type Manager interface {
	// Catalog discovers and validates the project's repository definitions.
	Catalog(ctx context.Context, id domain.ProjectID) (CatalogResponse, error)
	// GetDefault returns the project's default pipeline selection and whether
	// it can currently run.
	GetDefault(ctx context.Context, id domain.ProjectID) (DefaultStatus, error)
	// SetDefault saves (or, with a nil selection, clears) the project default
	// without touching any other project setting.
	SetDefault(ctx context.Context, id domain.ProjectID, in SetDefaultInput) (DefaultStatus, error)
}

// WorkflowView is one discovered workflow plus whether this build can run it.
type WorkflowView struct {
	ID                string                `json:"id"`
	File              string                `json:"file"`
	Valid             bool                  `json:"valid"`
	Workflow          *pipeline.Workflow    `json:"workflow,omitempty"`
	Diagnostics       []pipeline.Diagnostic `json:"diagnostics"`
	Executable        bool                  `json:"executable"`
	UnavailableReason string                `json:"unavailableReason,omitempty"`
}

// CatalogResponse is the body of GET /projects/{id}/pipelines.
type CatalogResponse struct {
	ProjectID   domain.ProjectID        `json:"projectId"`
	Profiles    []pipeline.ProfileEntry `json:"profiles"`
	Workflows   []WorkflowView          `json:"workflows"`
	Diagnostics []pipeline.Diagnostic   `json:"diagnostics"`
	Default     DefaultStatus           `json:"default"`
}

// DefaultStatus describes the stored selection and its current resolution.
type DefaultStatus struct {
	Selection *domain.PipelineSelection `json:"selection"`
	State     string                    `json:"state" enum:"unset,normal_worker,workflow_available,workflow_unavailable,workflow_invalid,workflow_missing"`
	// Executable is true only when a task started now would really run the
	// selected workflow. It is false for every state but workflow_available.
	Executable bool `json:"executable"`
	// Message explains the state in plain language.
	Message string `json:"message"`
}

// SetDefaultInput is the body of PUT /projects/{id}/pipelines/default. A nil
// selection clears the default.
type SetDefaultInput struct {
	Selection *domain.PipelineSelection `json:"selection"`
}

// Service implements Manager.
type Service struct {
	store Store
}

var _ Manager = (*Service)(nil)

// New returns a pipelines service.
func New(store Store) *Service { return &Service{store: store} }

func (s *Service) project(ctx context.Context, id domain.ProjectID) (domain.ProjectRecord, error) {
	if strings.TrimSpace(string(id)) == "" {
		return domain.ProjectRecord{}, apierr.Invalid("INVALID_PROJECT_ID", "Project id is required", nil)
	}
	row, ok, err := s.store.GetProject(ctx, string(id))
	if err != nil {
		return domain.ProjectRecord{}, apierr.Internal("PROJECT_LOAD_FAILED", "Failed to load project")
	}
	if !ok || !row.ArchivedAt.IsZero() {
		return domain.ProjectRecord{}, apierr.NotFound("PROJECT_NOT_FOUND", "Unknown project")
	}
	return row, nil
}

// Catalog implements Manager.
func (s *Service) Catalog(ctx context.Context, id domain.ProjectID) (CatalogResponse, error) {
	row, err := s.project(ctx, id)
	if err != nil {
		return CatalogResponse{}, err
	}
	cat := pipeline.Discover(row.Path)
	return CatalogResponse{
		ProjectID:   id,
		Profiles:    cat.Profiles,
		Workflows:   workflowViews(cat),
		Diagnostics: cat.Diagnostics,
		Default:     resolveDefault(row.Config.DefaultPipeline, cat),
	}, nil
}

// GetDefault implements Manager.
func (s *Service) GetDefault(ctx context.Context, id domain.ProjectID) (DefaultStatus, error) {
	row, err := s.project(ctx, id)
	if err != nil {
		return DefaultStatus{}, err
	}
	return resolveDefault(row.Config.DefaultPipeline, pipeline.Discover(row.Path)), nil
}

// SetDefault implements Manager.
func (s *Service) SetDefault(ctx context.Context, id domain.ProjectID, in SetDefaultInput) (DefaultStatus, error) {
	row, err := s.project(ctx, id)
	if err != nil {
		return DefaultStatus{}, err
	}
	cat := pipeline.Discover(row.Path)
	if sel := in.Selection; sel != nil {
		if err := sel.Validate(); err != nil {
			return DefaultStatus{}, apierr.Invalid("INVALID_PIPELINE_SELECTION", err.Error(), nil)
		}
		if sel.Mode == domain.PipelineModeWorkflow {
			entry, ok := cat.FindWorkflow(sel.WorkflowID)
			switch {
			case !ok:
				return DefaultStatus{}, apierr.Invalid("PIPELINE_WORKFLOW_NOT_FOUND",
					fmt.Sprintf("Workflow %q is not defined in %s", sel.WorkflowID, pipeline.WorkflowsDir), nil)
			case !entry.Valid:
				diags := entry.Diagnostics
				if len(diags) > maxDiagnosticsInErrorBody {
					diags = diags[:maxDiagnosticsInErrorBody]
				}
				return DefaultStatus{}, apierr.Invalid("PIPELINE_WORKFLOW_INVALID",
					fmt.Sprintf("Workflow %q has validation errors; fix %s before selecting it", sel.WorkflowID, entry.File),
					map[string]any{"diagnostics": diags})
			}
		}
	}
	updated, ok, err := s.store.SetProjectDefaultPipeline(ctx, string(id), in.Selection)
	if err != nil {
		return DefaultStatus{}, apierr.Internal("PIPELINE_DEFAULT_UPDATE_FAILED", "Failed to save the default pipeline")
	}
	if !ok {
		return DefaultStatus{}, apierr.NotFound("PROJECT_NOT_FOUND", "Unknown project")
	}
	return resolveDefault(updated.Config.DefaultPipeline, cat), nil
}

func workflowViews(cat pipeline.Catalog) []WorkflowView {
	out := make([]WorkflowView, 0, len(cat.Workflows))
	for _, e := range cat.Workflows {
		v := WorkflowView{ID: e.ID, File: e.File, Valid: e.Valid, Workflow: e.Workflow, Diagnostics: e.Diagnostics}
		if e.Valid && e.Workflow != nil {
			v.UnavailableReason = pipeline.UnsupportedExecutionReason(*e.Workflow)
			v.Executable = v.UnavailableReason == ""
		} else {
			v.UnavailableReason = "The workflow definition has validation errors."
		}
		out = append(out, v)
	}
	return out
}

// resolveDefault classifies a stored selection against the live catalog.
func resolveDefault(sel *domain.PipelineSelection, cat pipeline.Catalog) DefaultStatus {
	switch {
	case sel == nil:
		return DefaultStatus{State: StateUnset, Message: "No default pipeline is set. Tasks start as normal workers."}
	case sel.Mode == domain.PipelineModeNormalWorker:
		return DefaultStatus{Selection: sel, State: StateNormalWorker, Message: "Tasks start as normal workers by explicit choice."}
	}
	entry, ok := cat.FindWorkflow(sel.WorkflowID)
	switch {
	case !ok:
		return DefaultStatus{Selection: sel, State: StateWorkflowMissing,
			Message: fmt.Sprintf("Workflow %q is selected but no longer defined in %s. It is unavailable; tasks are not silently started as a different kind of worker.", sel.WorkflowID, pipeline.WorkflowsDir)}
	case !entry.Valid || entry.Workflow == nil:
		return DefaultStatus{Selection: sel, State: StateWorkflowInvalid,
			Message: fmt.Sprintf("Workflow %q has validation errors in %s and is unavailable.", sel.WorkflowID, entry.File)}
	}
	if reason := pipeline.UnsupportedExecutionReason(*entry.Workflow); reason != "" {
		return DefaultStatus{Selection: sel, State: StateWorkflowUnavailable, Message: reason}
	}
	return DefaultStatus{Selection: sel, State: StateWorkflowAvailable, Executable: true,
		Message: fmt.Sprintf("Tasks start with workflow %q.", sel.WorkflowID)}
}
