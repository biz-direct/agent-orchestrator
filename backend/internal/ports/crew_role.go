package ports

import (
	"context"
	"errors"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Crew role sessions: visible, interactive Chat sessions attached to a worker
// task that share the worker's worktree. The worker keeps ownership of the
// worktree, branch, and PR; a role session is never a board card.

var (
	// ErrCrewRoleUnsupported means the harness or build cannot run a role as a
	// Chat session. Roles never fall back to a Terminal.
	ErrCrewRoleUnsupported = errors.New("crew role is not supported by this harness or mode")
	// ErrCrewRoleExists means the task already has a live session for the role.
	ErrCrewRoleExists = errors.New("the task already has a session for this role")
	// ErrCrewRoleNotFound means the task has no live session for the role.
	ErrCrewRoleNotFound = errors.New("the task has no session for this role")
	// ErrCrewOwnerBusy means another lifecycle operation (a kill, switch, or
	// restore) currently owns the worker, so a role cannot be added safely.
	ErrCrewOwnerBusy = errors.New("the task is busy with another operation; try again shortly")
	// ErrCrewOwnerInvalid means the target is not a live worker task that can
	// host role sessions.
	ErrCrewOwnerInvalid = errors.New("the task cannot host role sessions")
)

// CrewRoleStart describes a role session to start inside Owner's worktree.
type CrewRoleStart struct {
	Owner  domain.SessionID
	RoleID string
	// Description is the role's one-line purpose, used as the tab title.
	Description string
	// Instructions are the resolved role profile instructions, frozen into the
	// session when it is created.
	Instructions string
	Harness      domain.AgentHarness
	Model        string
}

// CrewRoleHost starts and ends role sessions. The session manager implements it.
type CrewRoleHost interface {
	// PreflightCrewRole reports, before anything is created, whether the harness
	// can run a role as a Chat session right now.
	PreflightCrewRole(ctx context.Context, harness domain.AgentHarness) error
	// StartCrewRole creates and starts the role session.
	StartCrewRole(ctx context.Context, start CrewRoleStart) (domain.SessionRecord, error)
	// StopCrewRole ends only the role session's conversation. It never touches
	// the owner's worktree, branch, or PR.
	StopCrewRole(ctx context.Context, owner domain.SessionID, role string) error
	// ReconnectCrewRoles adopts the still-running provider hosts of every live
	// role session after a daemon restart. It never starts a new conversation.
	ReconnectCrewRoles(ctx context.Context)
}
