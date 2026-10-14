package crews

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/crew"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// SessionStore is the durable session surface role sessions need.
type SessionStore interface {
	GetSession(ctx context.Context, id domain.SessionID) (domain.SessionRecord, bool, error)
	GetSessionAttachedTo(ctx context.Context, id domain.SessionID) (domain.SessionID, error)
	ListAttachedSessionIDs(ctx context.Context, owner domain.SessionID) ([]domain.SessionID, error)
	GetSessionCrewRole(ctx context.Context, id domain.SessionID) (string, error)
}

// WithRoleSessions enables role sessions. A nil host leaves them unavailable.
func (s *Service) WithRoleSessions(sessions SessionStore, host ports.CrewRoleHost) *Service {
	s.sessions, s.host = sessions, host
	return s
}

// AddRoleInput is the body of POST /sessions/{id}/crew/roles.
type AddRoleInput struct {
	// Role is the id of a valid role defined under .ao/crew/roles.
	Role string `json:"role"`
	// Harness and Model override the role's defaults, which in turn override
	// the task's own harness.
	Harness string `json:"harness,omitempty"`
	Model   string `json:"model,omitempty"`
}

// RoleSession is one role's live conversation on a task.
type RoleSession struct {
	// SessionID addresses the role's Chat conversation, which uses the same
	// message, steer, approval, and interrupt routes as any Chat session.
	SessionID        domain.SessionID `json:"sessionId"`
	OwnerSessionID   domain.SessionID `json:"ownerSessionId"`
	Role             string           `json:"role"`
	Harness          string           `json:"harness"`
	Model            string           `json:"model,omitempty"`
	ControllerActive bool             `json:"controllerActive"`
	CreatedAt        time.Time        `json:"createdAt"`
}

func (s *Service) rolesAvailable() error {
	if s.sessions == nil || s.host == nil {
		return apierr.Unavailable("CREW_ROLES_UNAVAILABLE", "Role sessions are not available in this build")
	}
	return nil
}

// owner loads a live worker task that can host role sessions.
func (s *Service) owner(ctx context.Context, id domain.SessionID) (domain.SessionRecord, error) {
	rec, ok, err := s.sessions.GetSession(ctx, id)
	if err != nil {
		return domain.SessionRecord{}, apierr.Internal("SESSION_LOAD_FAILED", "Failed to load session")
	}
	if !ok {
		return domain.SessionRecord{}, apierr.NotFound("SESSION_NOT_FOUND", "Unknown session")
	}
	attachedTo, err := s.sessions.GetSessionAttachedTo(ctx, id)
	if err != nil {
		return domain.SessionRecord{}, apierr.Internal("SESSION_LOAD_FAILED", "Failed to load session")
	}
	if rec.Kind != domain.KindWorker || attachedTo != "" {
		return domain.SessionRecord{}, apierr.Invalid("CREW_OWNER_INVALID", "Roles can only be added to a worker task", nil)
	}
	if rec.IsTerminated {
		return domain.SessionRecord{}, apierr.Conflict("CREW_OWNER_TERMINATED", "The task has ended", nil)
	}
	return rec, nil
}

// AddRole implements Manager.
func (s *Service) AddRole(ctx context.Context, ownerID domain.SessionID, in AddRoleInput) (RoleSession, error) {
	if err := s.rolesAvailable(); err != nil {
		return RoleSession{}, err
	}
	roleID := strings.TrimSpace(in.Role)
	if roleID == "" {
		return RoleSession{}, apierr.Invalid("INVALID_CREW_ROLE", "A role id is required", nil)
	}
	owner, err := s.owner(ctx, ownerID)
	if err != nil {
		return RoleSession{}, err
	}
	project, err := s.project(ctx, owner.ProjectID)
	if err != nil {
		return RoleSession{}, err
	}
	entry, ok := crew.Discover(project.Path).FindRole(roleID)
	switch {
	case !ok:
		return RoleSession{}, apierr.Invalid("CREW_ROLE_NOT_FOUND",
			fmt.Sprintf("Role %q is not defined in %s", roleID, crew.RolesDir), nil)
	case !entry.Valid || entry.Role == nil:
		diags := entry.Diagnostics
		if len(diags) > maxDiagnosticsInErrorBody {
			diags = diags[:maxDiagnosticsInErrorBody]
		}
		return RoleSession{}, apierr.Invalid("CREW_ROLE_INVALID",
			fmt.Sprintf("Role %q has validation errors; fix %s before adding it", roleID, entry.File),
			map[string]any{"diagnostics": diags})
	}
	role := entry.Role
	harness := owner.Harness
	for _, candidate := range []string{role.Harness, in.Harness} {
		if strings.TrimSpace(candidate) != "" {
			harness = domain.AgentHarness(strings.TrimSpace(candidate))
		}
	}
	model := role.Model
	if strings.TrimSpace(in.Model) != "" {
		model = strings.TrimSpace(in.Model)
	}
	if err := s.host.PreflightCrewRole(ctx, harness); err != nil {
		return RoleSession{}, roleError(err)
	}
	rec, err := s.host.StartCrewRole(ctx, ports.CrewRoleStart{
		Owner: owner.ID, RoleID: role.ID, Description: role.Description,
		Instructions: role.InstructionsText, Harness: harness, Model: model,
	})
	if err != nil {
		return RoleSession{}, roleError(err)
	}
	return roleView(rec, owner.ID, role.ID), nil
}

// ListRoles implements Manager.
func (s *Service) ListRoles(ctx context.Context, ownerID domain.SessionID) ([]RoleSession, error) {
	if err := s.rolesAvailable(); err != nil {
		return nil, err
	}
	if _, ok, err := s.sessions.GetSession(ctx, ownerID); err != nil {
		return nil, apierr.Internal("SESSION_LOAD_FAILED", "Failed to load session")
	} else if !ok {
		return nil, apierr.NotFound("SESSION_NOT_FOUND", "Unknown session")
	}
	ids, err := s.sessions.ListAttachedSessionIDs(ctx, ownerID)
	if err != nil {
		return nil, apierr.Internal("CREW_ROLES_LOAD_FAILED", "Failed to list role sessions")
	}
	out := []RoleSession{}
	for _, id := range ids {
		role, err := s.sessions.GetSessionCrewRole(ctx, id)
		if err != nil {
			return nil, apierr.Internal("CREW_ROLES_LOAD_FAILED", "Failed to list role sessions")
		}
		if role == "" {
			continue
		}
		rec, ok, err := s.sessions.GetSession(ctx, id)
		if err != nil {
			return nil, apierr.Internal("CREW_ROLES_LOAD_FAILED", "Failed to list role sessions")
		}
		if !ok || rec.IsTerminated {
			continue
		}
		out = append(out, roleView(rec, ownerID, role))
	}
	return out, nil
}

// RemoveRole implements Manager.
func (s *Service) RemoveRole(ctx context.Context, ownerID domain.SessionID, role string) error {
	if err := s.rolesAvailable(); err != nil {
		return err
	}
	if _, ok, err := s.sessions.GetSession(ctx, ownerID); err != nil {
		return apierr.Internal("SESSION_LOAD_FAILED", "Failed to load session")
	} else if !ok {
		return apierr.NotFound("SESSION_NOT_FOUND", "Unknown session")
	}
	if err := s.host.StopCrewRole(ctx, ownerID, strings.TrimSpace(role)); err != nil {
		return roleError(err)
	}
	return nil
}

func roleView(rec domain.SessionRecord, owner domain.SessionID, role string) RoleSession {
	return RoleSession{
		SessionID: rec.ID, OwnerSessionID: owner, Role: role,
		Harness: string(rec.Harness), Model: rec.Metadata.Model,
		ControllerActive: rec.Metadata.ControllerGeneration != "",
		CreatedAt:        rec.CreatedAt,
	}
}

// roleError maps host failures to visible API errors. An unsupported harness is
// always refused with its reason, never downgraded.
func roleError(err error) error {
	switch {
	case errors.Is(err, ports.ErrCrewRoleUnsupported):
		return apierr.Invalid("CREW_ROLE_HARNESS_UNSUPPORTED", err.Error(), nil)
	case errors.Is(err, ports.ErrCrewRoleExists):
		return apierr.Conflict("CREW_ROLE_EXISTS", err.Error(), nil)
	case errors.Is(err, ports.ErrCrewRoleNotFound):
		return apierr.NotFound("CREW_ROLE_NOT_FOUND", err.Error())
	case errors.Is(err, ports.ErrCrewOwnerBusy):
		return apierr.Conflict("CREW_OWNER_BUSY", err.Error(), nil)
	case errors.Is(err, ports.ErrCrewOwnerInvalid):
		return apierr.Conflict("CREW_OWNER_INVALID", err.Error(), nil)
	}
	return apierr.Internal("CREW_ROLE_FAILED", "Failed to update the task's role sessions")
}
