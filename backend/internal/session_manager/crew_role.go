package sessionmanager

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Crew role sessions.
//
// A role session is a hidden attached Chat session (the same row shape a
// pipeline stage uses, see pipeline_stage.go) that runs one repository-defined
// role inside the owner worker's worktree. The hidden row keeps it off the
// board, out of session listings, the reaper, startup reconcile, and SCM
// observation; the human reaches it through the task view, which addresses it by
// id. This file never creates, registers, or removes a worktree: the shared
// workspace belongs to the owner worker for its whole life.

// crewRoleStore is the optional store capability that creates role sessions and
// tells them apart from pipeline stages. Stores without it host no roles.
type crewRoleStore interface {
	attachedSessionStore
	attachedAttemptStore
	GetSessionCrewRole(ctx context.Context, id domain.SessionID) (string, error)
}

var _ ports.CrewRoleHost = (*Manager)(nil)

// crewRoleOf returns the role an attached session runs, or "" for any other
// session.
func (m *Manager) crewRoleOf(ctx context.Context, id domain.SessionID) (string, error) {
	store, ok := m.store.(crewRoleStore)
	if !ok {
		return "", nil
	}
	return store.GetSessionCrewRole(ctx, id)
}

// PreflightCrewRole implements ports.CrewRoleHost. Roles are Chat-only: a
// harness without a Chat driver is refused, never run in a Terminal.
func (m *Manager) PreflightCrewRole(ctx context.Context, harness domain.AgentHarness) error {
	if m.chat == nil {
		return fmt.Errorf("%w: Chat is not available in this build", ports.ErrCrewRoleUnsupported)
	}
	if !harness.IsKnown() {
		return fmt.Errorf("%w: unknown harness %q", ports.ErrCrewRoleUnsupported, harness)
	}
	if !m.chat.SupportsChat(harness) {
		return fmt.Errorf("%w: harness %s has no Chat driver (roles never fall back to a terminal)", ports.ErrCrewRoleUnsupported, harness)
	}
	if err := m.chat.PreflightChat(ctx, harness, ports.PermissionModeAuto); err != nil {
		return fmt.Errorf("%w: %w", ports.ErrCrewRoleUnsupported, err)
	}
	return nil
}

// StartCrewRole implements ports.CrewRoleHost.
func (m *Manager) StartCrewRole(ctx context.Context, start ports.CrewRoleStart) (domain.SessionRecord, error) {
	store, ok := m.store.(crewRoleStore)
	if !ok || m.chat == nil {
		return domain.SessionRecord{}, fmt.Errorf("%w: this build cannot host role sessions", ports.ErrCrewRoleUnsupported)
	}
	if err := m.PreflightCrewRole(ctx, start.Harness); err != nil {
		return domain.SessionRecord{}, err
	}
	// Hold the owner's lifecycle operation for the whole add. A kill, switch, or
	// restore that already owns the worker refuses the add, and one that arrives
	// meanwhile is refused until the role is registered, so the teardown cascade
	// can never miss a role whose controller is starting in the owner's worktree.
	if err := m.beginAgentOperation(ctx, start.Owner, agentOperationCrewRole); err != nil {
		if errors.Is(err, errAgentOperationInProgress) {
			return domain.SessionRecord{}, ports.ErrCrewOwnerBusy
		}
		return domain.SessionRecord{}, err
	}
	defer m.endAgentOperation(start.Owner, agentOperationCrewRole)
	owner, err := m.getRecord(ctx, start.Owner)
	if err != nil {
		return domain.SessionRecord{}, fmt.Errorf("%w: %w", ports.ErrCrewOwnerInvalid, err)
	}
	if owner.Kind != domain.KindWorker || owner.IsTerminated || strings.TrimSpace(owner.Metadata.WorkspacePath) == "" {
		return domain.SessionRecord{}, fmt.Errorf("%w: %s is not a live worker with a workspace", ports.ErrCrewOwnerInvalid, owner.ID)
	}
	if attachedTo, aerr := m.attachedOwner(ctx, owner.ID); aerr != nil {
		return domain.SessionRecord{}, aerr
	} else if attachedTo != "" {
		return domain.SessionRecord{}, fmt.Errorf("%w: %s is itself an attached session", ports.ErrCrewOwnerInvalid, owner.ID)
	}
	project, found, err := m.store.GetProject(ctx, string(owner.ProjectID))
	if err != nil || !found {
		return domain.SessionRecord{}, fmt.Errorf("load project %s: %w", owner.ProjectID, err)
	}
	if _, exists, ferr := m.findCrewRole(ctx, store, owner.ID, start.RoleID); ferr != nil {
		return domain.SessionRecord{}, ferr
	} else if exists {
		return domain.SessionRecord{}, fmt.Errorf("%w: %s", ports.ErrCrewRoleExists, start.RoleID)
	}
	releaseHarness, err := m.beginHarnessUse(start.Harness)
	if err != nil {
		return domain.SessionRecord{}, err
	}
	defer releaseHarness()
	releaseCodex, err := m.acquireCodexControllerAdmission(ctx, start.Harness)
	if err != nil {
		return domain.SessionRecord{}, err
	}
	defer releaseCodex()

	agentConfig := effectiveAgentConfig(start.Harness, domain.KindWorker, project.Config)
	if start.Model != "" {
		agentConfig.Model = start.Model
	}
	if owner.Metadata.Permissions != "" {
		agentConfig.Permissions = owner.Metadata.Permissions
	}
	now := m.clock()
	rec, err := store.CreateAttachedSessionForAttempt(ctx, domain.SessionRecord{
		ProjectID: owner.ProjectID, Kind: domain.KindWorker, Harness: start.Harness, Mode: domain.SessionModeChat,
		DisplayName: start.RoleID,
		Activity:    domain.Activity{State: domain.ActivityActive, LastActivityAt: now},
		Metadata: domain.SessionMetadata{
			Permissions: agentConfig.Permissions, Branch: owner.Metadata.Branch,
			WorkspacePath: owner.Metadata.WorkspacePath, WorkspaceRepoPath: owner.Metadata.WorkspaceRepoPath,
			DiffBaseSHA: owner.Metadata.DiffBaseSHA, DiffBaseRef: owner.Metadata.DiffBaseRef,
			// The role's instructions are frozen here so a resume after a
			// repository edit keeps the profile the human started with.
			Prompt: start.Instructions, Model: agentConfig.Model, Effort: agentConfig.Effort,
		},
		ProvisionState: domain.SessionProvisionReady, CreatedAt: now, UpdatedAt: now,
	}, owner.ID, domain.CrewRoleMarker(owner.ID, start.RoleID))
	if err != nil {
		if exists, _ := m.crewRoleClaimed(ctx, store, owner.ID, start.RoleID); exists {
			return domain.SessionRecord{}, fmt.Errorf("%w: %s", ports.ErrCrewRoleExists, start.RoleID)
		}
		return domain.SessionRecord{}, fmt.Errorf("create crew role session: %w", err)
	}
	fail := func(cause error) (domain.SessionRecord, error) {
		m.stopChatBestEffort(ctx, rec.ID)
		_ = m.lcm.MarkTerminated(context.WithoutCancel(ctx), rec.ID)
		m.releaseAttachedAttempt(ctx, rec.ID)
		return domain.SessionRecord{}, cause
	}
	// Reserve the role's own lifecycle operation for the rest of creation, so a
	// removal or direct kill cannot interleave between the row existing and its
	// provider being confirmed started. A removal that won the narrow gap before
	// this reservation has already ended the row; never start a provider for it.
	if err := m.beginAgentOperation(ctx, rec.ID, agentOperationCrewRole); err != nil {
		return fail(fmt.Errorf("%w: reserve role %s: %w", ports.ErrCrewOwnerBusy, rec.ID, err))
	}
	defer m.endAgentOperation(rec.ID, agentOperationCrewRole)
	if current, err := m.getRecord(ctx, rec.ID); err != nil || current.IsTerminated {
		return fail(fmt.Errorf("%w: role %s was removed while starting", ports.ErrCrewRoleNotFound, rec.ID))
	}
	systemPrompt, err := m.buildSystemPrompt(ctx, rec.Kind, rec.ProjectID, rec.ID, rec.RequestedOutput, true)
	if err != nil {
		return fail(fmt.Errorf("role system prompt: %w", err))
	}
	env := m.runtimeEnv(rec.ID, rec.ProjectID, owner.IssueID, project.Config.Env)
	if agent, ok := m.agents.Agent(start.Harness); ok {
		m.augmentAgentRuntimeEnv(agent, env)
	}
	var completionErr error
	_, err = m.chat.StartChat(ctx, ChatStart{
		SessionID: rec.ID, ProjectID: rec.ProjectID, Kind: domain.KindWorker, Harness: start.Harness,
		DataDir: m.dataDir, WorkspacePath: owner.Metadata.WorkspacePath, Env: env,
		Model: agentConfig.Model, Effort: agentConfig.Effort, Permissions: agentConfig.Permissions,
		SystemPrompt:            systemPrompt,
		ExpectedControllerOwner: rec.ControllerOwner(),
		ProviderConversationID:  rec.Metadata.ProviderConversationID,
		PrepareControllerEnv: func(launchCtx context.Context, expected domain.SessionControllerOwner) (map[string]string, error) {
			prepared, launchEnv, prepareErr := m.prepareChatControllerEnv(launchCtx, rec, project.Config.Env, expected)
			if prepareErr != nil {
				return nil, fmt.Errorf("%w: %w", ErrSpawnBrowser, prepareErr)
			}
			if agent, ok := m.agents.Agent(start.Harness); ok {
				m.augmentAgentRuntimeEnv(agent, launchEnv)
			}
			rec = prepared
			return launchEnv, nil
		},
		ControllerReady: func(started ChatStarted) (ChatControllerCommit, error) {
			metadata := rec.Metadata
			metadata.ProviderConversationID = started.ProviderConversationID
			metadata.ControllerGeneration = started.ControllerGeneration
			committed, commitErr := m.markChatControllerSpawned(ctx, rec.ID, metadata, started.Conversation, started.ProviderBoundary, started.CommitProviderHistory, nil, started.LiveReconnect)
			completionErr = commitErr
			return ChatControllerCommit{
				Conversation:    committed,
				ControllerOwner: chatControllerOwner(rec, start.Harness, started.ProviderConversationID, started.ControllerGeneration),
			}, commitErr
		},
	})
	if err != nil {
		if completionErr != nil {
			return fail(fmt.Errorf("commit role controller: %w", completionErr))
		}
		return fail(fmt.Errorf("start role controller: %w", err))
	}
	return m.getRecord(ctx, rec.ID)
}

// StopCrewRole implements ports.CrewRoleHost. It ends the role's conversation
// only; the owner's worktree, branch, and PR are untouched.
func (m *Manager) StopCrewRole(ctx context.Context, owner domain.SessionID, role string) error {
	store, ok := m.store.(crewRoleStore)
	if !ok {
		return ports.ErrCrewRoleNotFound
	}
	id, found, err := m.findCrewRole(ctx, store, owner, role)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %s", ports.ErrCrewRoleNotFound, role)
	}
	return m.terminateAttachedSession(ctx, id)
}

// findCrewRole returns the live role session of owner for role.
func (m *Manager) findCrewRole(ctx context.Context, store crewRoleStore, owner domain.SessionID, role string) (domain.SessionID, bool, error) {
	id, found, err := store.FindAttachedSessionForAttempt(ctx, domain.CrewRoleMarker(owner, role))
	if err != nil || !found {
		return "", false, err
	}
	rec, ok, err := m.store.GetSession(ctx, id)
	if err != nil {
		return "", false, err
	}
	if !ok || rec.IsTerminated {
		return "", false, nil
	}
	return id, true, nil
}

// crewRoleClaimed reports whether a create failure was the role's claim being
// taken by a concurrent add.
func (m *Manager) crewRoleClaimed(ctx context.Context, store crewRoleStore, owner domain.SessionID, role string) (bool, error) {
	_, found, err := store.FindAttachedSessionForAttempt(ctx, domain.CrewRoleMarker(owner, role))
	return found, err
}

// crewRolePrompt returns the standing instructions of a role session: its
// frozen role profile, the task it serves, and the crew it works with. It is
// empty for every session that is not a crew role, so ordinary workers and
// pipeline stages are unaffected.
func (m *Manager) crewRolePrompt(ctx context.Context, id domain.SessionID) (string, error) {
	store, ok := m.store.(crewRoleStore)
	if !ok {
		return "", nil
	}
	role, err := store.GetSessionCrewRole(ctx, id)
	if err != nil {
		return "", fmt.Errorf("crew role lookup: %w", err)
	}
	if role == "" {
		return "", nil
	}
	rec, err := m.getRecord(ctx, id)
	if err != nil {
		return "", err
	}
	ownerID, err := store.GetSessionAttachedTo(ctx, id)
	if err != nil || ownerID == "" {
		return "", errors.Join(errors.New("crew role session has no owner"), err)
	}
	owner, err := m.getRecord(ctx, ownerID)
	if err != nil {
		return "", err
	}
	siblings, err := store.ListAttachedSessionIDs(ctx, ownerID)
	if err != nil {
		return "", fmt.Errorf("list crew: %w", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## Crew role: %s\n\n", role)
	b.WriteString("You are a role session on a task crew, not the task's owner. You work in the same worktree and branch as the task's worker (")
	fmt.Fprintf(&b, "session %s, branch %s). The worker owns the worktree, the branch, and the pull request: do not create or switch branches, and do not open, close, or merge a pull request. A person talks to you directly in this conversation; follow their steering.\n\n", owner.ID, owner.Metadata.Branch)
	if task := strings.TrimSpace(owner.Metadata.Prompt); task != "" {
		b.WriteString("### Task\n\n")
		b.WriteString(task)
		b.WriteString("\n\n")
	}
	b.WriteString("### Teammates\n\n")
	fmt.Fprintf(&b, "- worker (session %s): owns the worktree, branch, and PR\n", owner.ID)
	for _, sib := range siblings {
		if sib == id {
			continue
		}
		sibRole, rerr := store.GetSessionCrewRole(ctx, sib)
		if rerr != nil || sibRole == "" {
			continue
		}
		sibRec, ok, gerr := m.store.GetSession(ctx, sib)
		if gerr != nil || !ok || sibRec.IsTerminated {
			continue
		}
		fmt.Fprintf(&b, "- %s (session %s)\n", sibRole, sib)
	}
	if instructions := strings.TrimSpace(rec.Metadata.Prompt); instructions != "" {
		b.WriteString("\n### Your role instructions\n\n")
		b.WriteString(instructions)
		b.WriteString("\n")
	}
	return b.String(), nil
}

// crewRoleLister is the optional store capability that enumerates live role
// sessions, which every ordinary session listing deliberately omits.
type crewRoleLister interface {
	ListLiveCrewRoleSessionIDs(ctx context.Context) ([]domain.SessionID, error)
}

// ReconnectCrewRoles implements ports.CrewRoleHost. It is the startup
// counterpart of the ordinary Chat reconnect for hidden role rows: a role's
// provider host that survived the restart is adopted (so an in-flight turn keeps
// running and the human can keep talking to it), and a host that is gone is left
// for the human to resume. Failures are logged, never fatal to startup.
func (m *Manager) ReconnectCrewRoles(ctx context.Context) {
	lister, ok := m.store.(crewRoleLister)
	if !ok {
		return
	}
	ids, err := lister.ListLiveCrewRoleSessionIDs(ctx)
	if err != nil {
		m.logger.Warn("reconcile: list crew role sessions failed", "error", err)
		return
	}
	for _, id := range ids {
		// Honor lifecycle fencing: a role being removed, resumed, or replaced
		// owns its own reconnection.
		if err := m.beginAgentOperation(ctx, id, agentOperationReconcile); err != nil {
			continue
		}
		if _, err := m.ReconnectExecutor(ctx, id); err != nil {
			m.logger.Warn("reconcile: crew role reconnect failed", "sessionID", id, "error", err)
		}
		m.endAgentOperation(id, agentOperationReconcile)
	}
}
