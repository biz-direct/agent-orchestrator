// Package crews exposes repository-defined crew roles and crew workflows to
// the daemon API: the validated catalog and the project-default crew
// selection.
//
// Definitions are read from the project's repository files on every call, so
// repository edits show up immediately; only the selection reference is stored.
package crews

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/crew"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Default-selection states reported to clients.
const (
	StateUnset                = "unset"
	StateNoCrew               = "no_crew"
	StateWorkflowAvailable    = "workflow_available"
	StateWorkflowInvalid      = "workflow_invalid"
	StateWorkflowMissing      = "workflow_missing"
	maxDiagnosticsInErrorBody = 20
)

// Store is the durable project surface the service needs.
type Store interface {
	GetProject(ctx context.Context, id string) (domain.ProjectRecord, bool, error)
	SetProjectDefaultCrew(ctx context.Context, id string, selection *domain.CrewSelection) (domain.ProjectRecord, bool, error)
}

// Manager is the controller-facing contract.
type Manager interface {
	// Catalog discovers and validates the project's repository definitions.
	Catalog(ctx context.Context, id domain.ProjectID) (CatalogResponse, error)
	// GetDefault returns the project's default crew selection and how it
	// currently resolves.
	GetDefault(ctx context.Context, id domain.ProjectID) (DefaultStatus, error)
	// SetDefault saves (or, with a nil selection, clears) the project default
	// without touching any other project setting.
	SetDefault(ctx context.Context, id domain.ProjectID, in SetDefaultInput) (DefaultStatus, error)
	// AddRole starts a visible, interactive role session inside a worker
	// task's worktree.
	AddRole(ctx context.Context, owner domain.SessionID, in AddRoleInput) (RoleSession, error)
	// ListRoles lists the task's live role sessions.
	ListRoles(ctx context.Context, owner domain.SessionID) ([]RoleSession, error)
	// RemoveRole ends only the role's conversation.
	RemoveRole(ctx context.Context, owner domain.SessionID, role string) error
	// SendMessage records a durable message from a role session to a teammate
	// role on the same task and delivers it.
	SendMessage(ctx context.Context, sender domain.SessionID, in SendMessageInput) (CrewMessage, error)
	// ListMessages returns the task's crew thread.
	ListMessages(ctx context.Context, session domain.SessionID) ([]CrewMessage, error)
	// EditMessage, DropMessage, and ReleaseMessage are a person's controls over
	// a message that has not been delivered.
	EditMessage(ctx context.Context, session domain.SessionID, messageID string, in EditMessageInput) (CrewMessage, error)
	DropMessage(ctx context.Context, session domain.SessionID, messageID string) (CrewMessage, error)
	ReleaseMessage(ctx context.Context, session domain.SessionID, messageID string) (CrewMessage, error)
	// GetBaton returns who holds the task's write baton and the move history.
	GetBaton(ctx context.Context, owner domain.SessionID) (BatonStatus, error)
	// MoveBaton is the human override that hands the baton to any holder.
	MoveBaton(ctx context.Context, owner domain.SessionID, in MoveBatonInput) (BatonStatus, error)
}

// CatalogResponse is the body of GET /projects/{id}/crew.
type CatalogResponse struct {
	ProjectID   domain.ProjectID     `json:"projectId"`
	Roles       []crew.RoleEntry     `json:"roles"`
	Workflows   []crew.WorkflowEntry `json:"workflows"`
	Diagnostics []crew.Diagnostic    `json:"diagnostics"`
	Default     DefaultStatus        `json:"default"`
}

// DefaultStatus describes the stored selection and its current resolution.
type DefaultStatus struct {
	Selection *domain.CrewSelection `json:"selection"`
	State     string                `json:"state" enum:"unset,no_crew,workflow_available,workflow_invalid,workflow_missing"`
	// Message explains the state in plain language.
	Message string `json:"message"`
}

// SetDefaultInput is the body of PUT /projects/{id}/crew/default. A nil
// selection clears the default.
type SetDefaultInput struct {
	Selection *domain.CrewSelection `json:"selection"`
}

// Service implements Manager.
type Service struct {
	store     Store
	sessions  SessionStore
	host      ports.CrewRoleHost
	baton     BatonStore
	messenger Messenger
	now       func() time.Time
	// batonMoves serializes each task's baton moves together with their notices,
	// so an older move cannot deliver a grant after a newer one revoked it.
	batonMoves sync.Map // domain.SessionID -> *sync.Mutex
	msgs       *messaging
}

var _ Manager = (*Service)(nil)

// New returns a crews service. Role sessions are unavailable until
// WithRoleSessions is applied.
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
	cat := crew.Discover(row.Path)
	return CatalogResponse{
		ProjectID:   id,
		Roles:       cat.Roles,
		Workflows:   cat.Workflows,
		Diagnostics: cat.Diagnostics,
		Default:     resolveDefault(row.Config.DefaultCrew, cat),
	}, nil
}

// GetDefault implements Manager.
func (s *Service) GetDefault(ctx context.Context, id domain.ProjectID) (DefaultStatus, error) {
	row, err := s.project(ctx, id)
	if err != nil {
		return DefaultStatus{}, err
	}
	return resolveDefault(row.Config.DefaultCrew, crew.Discover(row.Path)), nil
}

// SetDefault implements Manager.
func (s *Service) SetDefault(ctx context.Context, id domain.ProjectID, in SetDefaultInput) (DefaultStatus, error) {
	row, err := s.project(ctx, id)
	if err != nil {
		return DefaultStatus{}, err
	}
	cat := crew.Discover(row.Path)
	if sel := in.Selection; sel != nil {
		if err := sel.Validate(); err != nil {
			return DefaultStatus{}, apierr.Invalid("INVALID_CREW_SELECTION", err.Error(), nil)
		}
		if sel.Mode == domain.CrewModeWorkflow {
			entry, ok := cat.FindWorkflow(sel.WorkflowID)
			switch {
			case !ok:
				return DefaultStatus{}, apierr.Invalid("CREW_WORKFLOW_NOT_FOUND",
					fmt.Sprintf("Workflow %q is not defined in %s", sel.WorkflowID, crew.WorkflowsDir), nil)
			case !entry.Valid:
				diags := entry.Diagnostics
				if len(diags) > maxDiagnosticsInErrorBody {
					diags = diags[:maxDiagnosticsInErrorBody]
				}
				return DefaultStatus{}, apierr.Invalid("CREW_WORKFLOW_INVALID",
					fmt.Sprintf("Workflow %q has validation errors; fix %s before selecting it", sel.WorkflowID, entry.File),
					map[string]any{"diagnostics": diags})
			}
		}
	}
	updated, ok, err := s.store.SetProjectDefaultCrew(ctx, string(id), in.Selection)
	if err != nil {
		return DefaultStatus{}, apierr.Internal("CREW_DEFAULT_UPDATE_FAILED", "Failed to save the default crew workflow")
	}
	if !ok {
		return DefaultStatus{}, apierr.NotFound("PROJECT_NOT_FOUND", "Unknown project")
	}
	return resolveDefault(updated.Config.DefaultCrew, cat), nil
}

// resolveDefault classifies a stored selection against the live catalog.
func resolveDefault(sel *domain.CrewSelection, cat crew.Catalog) DefaultStatus {
	switch {
	case sel == nil:
		return DefaultStatus{State: StateUnset, Message: "No default crew workflow is set. Tasks start without a crew."}
	case sel.Mode == domain.CrewModeNoCrew:
		return DefaultStatus{Selection: sel, State: StateNoCrew, Message: "Tasks start without a crew by explicit choice."}
	}
	entry, ok := cat.FindWorkflow(sel.WorkflowID)
	switch {
	case !ok:
		return DefaultStatus{Selection: sel, State: StateWorkflowMissing,
			Message: fmt.Sprintf("Workflow %q is selected but no longer defined in %s. It is unavailable; tasks are not silently started with a different crew.", sel.WorkflowID, crew.WorkflowsDir)}
	case !entry.Valid || entry.Workflow == nil:
		return DefaultStatus{Selection: sel, State: StateWorkflowInvalid,
			Message: fmt.Sprintf("Workflow %q has validation errors in %s and is unavailable.", sel.WorkflowID, entry.File)}
	}
	return DefaultStatus{Selection: sel, State: StateWorkflowAvailable,
		Message: fmt.Sprintf("Workflow %q is the project default crew workflow.", sel.WorkflowID)}
}
