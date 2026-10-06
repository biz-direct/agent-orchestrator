package sessionmanager

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Attached pipeline stages.
//
// A specialist stage is a separate Chat conversation that runs inside the
// worker's own worktree. It is stored as a hidden session row (see
// Store.CreateAttachedSession) so the existing conversation, approval, and
// resume machinery serves it, while every session listing — board, reaper, SCM
// observer, startup reconcile — never sees it. This file is the only place that
// creates, quiesces, or stops one. It never creates, registers, or destroys a
// worktree: the shared workspace belongs to the owner worker for its whole life.

// pipelineRelinquishTimeout bounds how long a handoff waits for the source
// controller to drain before the handoff pauses as uncertain.
const pipelineRelinquishTimeout = 2 * time.Minute

type attachedSessionStore interface {
	CreateAttachedSession(ctx context.Context, rec domain.SessionRecord, owner domain.SessionID) (domain.SessionRecord, error)
}

type liveChatProbe interface {
	HasLiveChatController(id domain.SessionID) bool
}

var _ ports.PipelineExecutor = (*Manager)(nil)

// ErrPipelineExecutionOwned reports that a pipeline run, not this session, owns
// execution right now.
var ErrPipelineExecutionOwned = ports.ErrPipelineExecutionOwned

// SetPipelineGate late-binds the pipeline execution gate. A nil gate (no
// pipelines) leaves every session's behavior unchanged.
func (m *Manager) SetPipelineGate(gate ports.PipelineExecutionGate) {
	m.pipelineGate = gate
}

// admitPipelineExecution refuses input and controller (re)starts for a session
// whose pipeline run is executing a different stage.
func (m *Manager) admitPipelineExecution(ctx context.Context, id domain.SessionID) error {
	if m.pipelineGate == nil || ports.PipelineBypass(ctx) {
		return nil
	}
	if ok, reason := m.pipelineGate.AdmitSessionExecution(ctx, id); !ok {
		return fmt.Errorf("%w: %s", ports.ErrPipelineExecutionOwned, reason)
	}
	return nil
}

// PreflightStage implements ports.PipelineExecutor.
func (m *Manager) PreflightStage(ctx context.Context, harness domain.AgentHarness) error {
	if m.chat == nil {
		return fmt.Errorf("%w: Chat is not available in this build", ports.ErrPipelineStageUnsupported)
	}
	if !harness.IsKnown() {
		return fmt.Errorf("%w: unknown harness %q", ports.ErrPipelineStageUnsupported, harness)
	}
	if !m.chat.SupportsChat(harness) {
		return fmt.Errorf("%w: harness %s has no Chat driver (stages never fall back to a terminal)", ports.ErrPipelineStageUnsupported, harness)
	}
	if err := m.chat.PreflightChat(ctx, harness, ports.PermissionModeAuto); err != nil {
		return fmt.Errorf("%w: %w", ports.ErrPipelineStageUnsupported, err)
	}
	return nil
}

// RelinquishExecutor implements ports.PipelineExecutor. It closes the
// executor's intake and proves it is quiescent. "Idle" alone is not proof, so
// the controller must drain, report no work or approval waiting, and stay
// fenced; anything unprovable is returned as an uncertainty and the handoff
// pauses instead of risking two writers.
func (m *Manager) RelinquishExecutor(ctx context.Context, id domain.SessionID) error {
	uncertain := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ports.ErrPipelineExecutionUncertain, fmt.Sprintf(format, args...))
	}
	rec, ok, err := m.store.GetSession(ctx, id)
	if err != nil {
		return uncertain("session %s could not be read: %v", id, err)
	}
	if !ok {
		return uncertain("session %s no longer exists", id)
	}
	if !rec.IsTerminated && m.chat != nil {
		handoff, supported := m.chat.(chatHandoffLauncher)
		live := true
		if probe, ok := m.chat.(liveChatProbe); ok {
			live = probe.HasLiveChatController(id)
		}
		switch {
		case !live:
			// A missing controller cannot be running a turn.
		case !supported:
			return uncertain("the Chat controller cannot be fenced in this build")
		default:
			if err := handoff.ArmChatHandoff(ctx, id, domain.SessionInterfaceTransitionDrain); err != nil {
				return uncertain("could not close the controller's intake: %v", err)
			}
			drainCtx, cancel := context.WithTimeout(ctx, pipelineRelinquishTimeout)
			defer cancel()
			if err := handoff.PrepareChatHandoff(drainCtx, id, domain.SessionInterfaceTransitionDrain); err != nil {
				// The fence stays closed: the executor must not resume on its own.
				return uncertain("the controller did not drain: %v", err)
			}
		}
	}
	after, ok, err := m.store.GetSession(ctx, id)
	if err != nil || !ok {
		return uncertain("session %s could not be re-read after draining", id)
	}
	switch after.Activity.State {
	case domain.ActivityActive:
		return uncertain("the agent still reports activity (a turn or background work may be running)")
	case domain.ActivityWaitingInput, domain.ActivityBlocked:
		return uncertain("the agent is waiting on a permission or input request from the user")
	}
	return nil
}

// StartStage implements ports.PipelineExecutor.
func (m *Manager) StartStage(ctx context.Context, start ports.PipelineStageStart) (ports.PipelineStageStarted, error) {
	if m.chat == nil {
		return ports.PipelineStageStarted{}, fmt.Errorf("%w: Chat is not available in this build", ports.ErrPipelineStageUnsupported)
	}
	owner, err := m.getRecord(ctx, start.Owner)
	if err != nil {
		return ports.PipelineStageStarted{}, fmt.Errorf("load owner worker: %w", err)
	}
	if owner.IsTerminated || strings.TrimSpace(owner.Metadata.WorkspacePath) == "" {
		return ports.PipelineStageStarted{}, errors.New("the owner worker has no live workspace to share")
	}
	project, ok, err := m.store.GetProject(ctx, string(owner.ProjectID))
	if err != nil || !ok {
		return ports.PipelineStageStarted{}, fmt.Errorf("load project %s: %w", owner.ProjectID, err)
	}
	releaseHarness, err := m.beginHarnessUse(start.Harness)
	if err != nil {
		return ports.PipelineStageStarted{}, err
	}
	defer releaseHarness()
	releaseCodex, err := m.acquireCodexControllerAdmission(ctx, start.Harness)
	if err != nil {
		return ports.PipelineStageStarted{}, err
	}
	defer releaseCodex()

	agentConfig := effectiveAgentConfig(start.Harness, domain.KindWorker, project.Config)
	if start.Model != "" {
		agentConfig.Model = start.Model
	}
	if owner.Metadata.Permissions != "" {
		agentConfig.Permissions = owner.Metadata.Permissions
	}

	var rec domain.SessionRecord
	created := false
	if start.ExistingSessionID != "" {
		rec, err = m.getRecord(ctx, start.ExistingSessionID)
		if err != nil {
			return ports.PipelineStageStarted{}, fmt.Errorf("load the stage's retained conversation: %w", err)
		}
	} else {
		maker, ok := m.store.(attachedSessionStore)
		if !ok {
			return ports.PipelineStageStarted{}, errors.New("this store cannot create attached stage sessions")
		}
		now := m.clock()
		rec, err = maker.CreateAttachedSession(ctx, domain.SessionRecord{
			ProjectID: owner.ProjectID, Kind: domain.KindWorker, Harness: start.Harness, Mode: domain.SessionModeChat,
			DisplayName: fmt.Sprintf("%s stage", start.StageID),
			Activity:    domain.Activity{State: domain.ActivityActive, LastActivityAt: now},
			Metadata: domain.SessionMetadata{
				Permissions: agentConfig.Permissions, Branch: owner.Metadata.Branch,
				WorkspacePath: owner.Metadata.WorkspacePath, WorkspaceRepoPath: owner.Metadata.WorkspaceRepoPath,
				DiffBaseSHA: owner.Metadata.DiffBaseSHA, DiffBaseRef: owner.Metadata.DiffBaseRef,
				Prompt: start.Prompt, Model: agentConfig.Model, Effort: agentConfig.Effort,
			},
			ProvisionState: domain.SessionProvisionReady, CreatedAt: now, UpdatedAt: now,
		}, owner.ID)
		if err != nil {
			return ports.PipelineStageStarted{}, fmt.Errorf("create attached stage session: %w", err)
		}
		created = true
	}
	fail := func(cause error) (ports.PipelineStageStarted, error) {
		m.stopChatBestEffort(ctx, rec.ID)
		if created {
			_ = m.lcm.MarkTerminated(context.WithoutCancel(ctx), rec.ID)
		}
		return ports.PipelineStageStarted{}, cause
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
		SystemPrompt:            start.SystemPrompt,
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
			metadata.BrowserCapabilityVerifier = rec.Metadata.BrowserCapabilityVerifier
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
			return fail(fmt.Errorf("commit stage controller: %w", completionErr))
		}
		return fail(fmt.Errorf("start stage controller: %w", err))
	}
	// The stage prompt is a normal turn through the controller. The pipeline's
	// own delivery is the one thing the execution gate must let through.
	if _, err := m.chat.StartChatTurn(ports.WithPipelineBypass(ctx), rec.ID, start.Prompt); err != nil {
		return fail(fmt.Errorf("deliver stage prompt: %w", err))
	}
	final, err := m.getRecord(ctx, rec.ID)
	if err != nil {
		return fail(err)
	}
	return ports.PipelineStageStarted{
		SessionID: final.ID, ControllerGeneration: final.Metadata.ControllerGeneration,
		ProviderConversationID: final.Metadata.ProviderConversationID,
	}, nil
}

// StopStage implements ports.PipelineExecutor. It releases the stage's
// controller and nothing else: the shared workspace is not this session's to
// remove, and the conversation is retained for later resumption.
func (m *Manager) StopStage(ctx context.Context, id domain.SessionID) error {
	if m.chat == nil {
		return nil
	}
	return m.chat.StopChat(ctx, id)
}
