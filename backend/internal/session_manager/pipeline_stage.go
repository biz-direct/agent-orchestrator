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

type attachedAttemptStore interface {
	CreateAttachedSessionForAttempt(ctx context.Context, rec domain.SessionRecord, owner domain.SessionID, attemptID string) (domain.SessionRecord, error)
	FindAttachedSessionForAttempt(ctx context.Context, attemptID string) (domain.SessionID, bool, error)
}

// deliverStagePrompt delivers a stage prompt under the pipeline's deterministic
// key when one is set, so redelivery after a crash cannot create a second turn.
func (m *Manager) deliverStagePrompt(ctx context.Context, id domain.SessionID, prompt string) error {
	bypass := ports.WithPipelineBypass(ctx)
	if key := ports.PipelineDeliveryKey(ctx); key != "" {
		_, err := m.chat.RelayChatTurnWithID(bypass, id, prompt, key)
		return err
	}
	_, err := m.chat.RelayChatTurn(bypass, id, prompt)
	return err
}

// attachedSessionStore is the optional store capability that tells an attached
// specialist stage session apart from an ordinary one. Stores without it have
// no attached sessions.
type attachedSessionStore interface {
	GetSessionAttachedTo(ctx context.Context, id domain.SessionID) (domain.SessionID, error)
	ListAttachedSessionIDs(ctx context.Context, owner domain.SessionID) ([]domain.SessionID, error)
}

// attachedOwner returns the owner worker of an attached stage session, or ""
// for an ordinary session. A lookup failure is returned, never read as "not
// attached": the answer decides whether a shared workspace may be destroyed.
func (m *Manager) attachedOwner(ctx context.Context, id domain.SessionID) (domain.SessionID, error) {
	store, ok := m.store.(attachedSessionStore)
	if !ok {
		return "", nil
	}
	return store.GetSessionAttachedTo(ctx, id)
}

// terminateAttachedSession ends an attached stage session: it stops the
// controller and marks the row terminated, and nothing else. The worktree,
// branch, reviewer, and restore markers belong to the owner worker, so none of
// them are touched here.
func (m *Manager) terminateAttachedSession(ctx context.Context, id domain.SessionID) error {
	m.stopChatBestEffort(ctx, id)
	termCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), terminalIntentBudget)
	defer cancel()
	if err := m.lcm.MarkTerminated(termCtx, id); err != nil {
		return fmt.Errorf("terminate attached stage session %s: %w", id, err)
	}
	m.cleanupSystemPromptDir(id)
	return nil
}

// terminateAttachedSessionsOf stops and terminates every stage session attached
// to owner. It runs before the owner's workspace is torn down so no controller
// is left running in a removed directory, and so no hidden row outlives its
// owner (every listing excludes attached rows, so nothing else would reap it).
func (m *Manager) terminateAttachedSessionsOf(ctx context.Context, owner domain.SessionID) error {
	store, ok := m.store.(attachedSessionStore)
	if !ok {
		return nil
	}
	ids, err := store.ListAttachedSessionIDs(ctx, owner)
	if err != nil {
		return fmt.Errorf("list attached stage sessions of %s: %w", owner, err)
	}
	for _, id := range ids {
		rec, found, err := m.store.GetSession(ctx, id)
		if err != nil {
			return fmt.Errorf("load attached stage session %s: %w", id, err)
		}
		if !found {
			continue
		}
		if rec.IsTerminated {
			m.stopChatBestEffort(ctx, id)
			continue
		}
		if err := m.terminateAttachedSession(ctx, id); err != nil {
			return err
		}
	}
	return nil
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
	return m.quiesceExecutor(ctx, id, domain.SessionInterfaceTransitionDrain)
}

// InterruptExecutor implements ports.PipelineExecutor. It interrupts the
// running turn (and settles queued turns, which are recorded as cancelled)
// before applying the same proof as RelinquishExecutor. The controller stays
// fenced; the caller decides whether to ReleaseExecutor.
func (m *Manager) InterruptExecutor(ctx context.Context, id domain.SessionID) error {
	return m.quiesceExecutor(ctx, id, domain.SessionInterfaceTransitionInterrupt)
}

// ReleaseExecutor implements ports.PipelineExecutor.
func (m *Manager) ReleaseExecutor(ctx context.Context, id domain.SessionID) error {
	if m.chat == nil {
		return nil
	}
	if handoff, supported := m.chat.(chatHandoffLauncher); supported {
		handoff.AbortChatHandoff(id)
	}
	return nil
}

func (m *Manager) quiesceExecutor(ctx context.Context, id domain.SessionID, policy domain.SessionInterfaceTransitionPolicy) error {
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
		if !live {
			// No controller in this process is not proof that nothing is running:
			// provider hosts outlive the daemon, so a host this daemon has not
			// adopted may still be finishing a turn. Adopt it when it is there
			// (and fence through it), accept only when it is provably not
			// running, and otherwise stay uncertain.
			adopted, rerr := m.ReconnectExecutor(ctx, id)
			switch {
			case rerr != nil:
				return uncertain("the executor has no controller in this process and its host could not be probed: %v", rerr)
			case adopted:
				live = true
			}
		}
		switch {
		case !live:
			// The host is provably not running (or never was), so it cannot be
			// running a turn.
		case !supported:
			return uncertain("the Chat controller cannot be fenced in this build")
		default:
			if err := handoff.ArmChatHandoff(ctx, id, policy); err != nil {
				return uncertain("could not close the controller's intake: %v", err)
			}
			drainCtx, cancel := context.WithTimeout(ctx, pipelineRelinquishTimeout)
			defer cancel()
			if err := handoff.PrepareChatHandoff(drainCtx, id, policy); err != nil {
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

	// A session already created for this very attempt (the daemon stopped before
	// the attempt was confirmed) is adopted, never duplicated: when its
	// controller survived the stage prompt is redelivered under its idempotent
	// key; when it did not, AO will not start a fresh conversation in its place.
	if adopter, ok := m.store.(attachedAttemptStore); ok && start.ExistingSessionID == "" && start.AttemptID != "" {
		if id, found, ferr := adopter.FindAttachedSessionForAttempt(ctx, start.AttemptID); ferr != nil {
			return ports.PipelineStageStarted{}, fmt.Errorf("look up the stage's existing session: %w", ferr)
		} else if found {
			return m.adoptStage(ctx, id, start)
		}
	}
	var rec domain.SessionRecord
	created := false
	if start.ExistingSessionID != "" {
		rec, err = m.getRecord(ctx, start.ExistingSessionID)
		if err != nil {
			return ports.PipelineStageStarted{}, fmt.Errorf("load the stage's retained conversation: %w", err)
		}
	} else {
		maker, ok := m.store.(attachedAttemptStore)
		if !ok {
			return ports.PipelineStageStarted{}, errors.New("this store cannot create attached stage sessions")
		}
		now := m.clock()
		rec, err = maker.CreateAttachedSessionForAttempt(ctx, domain.SessionRecord{
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
		}, owner.ID, start.AttemptID)
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
	if err := m.deliverStagePrompt(m.stagePromptContext(ctx, start), rec.ID, start.Prompt); err != nil {
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

// stagePromptContext keys a stage's first prompt by its attempt so that a
// redelivery after a crash is the same delivery.
func (m *Manager) stagePromptContext(ctx context.Context, start ports.PipelineStageStart) context.Context {
	if ports.PipelineDeliveryKey(ctx) != "" || start.AttemptID == "" {
		return ctx
	}
	return ports.WithPipelineDeliveryKey(ctx, "pipeline-stage:"+start.AttemptID)
}

// adoptStage reconnects to the surviving session created for an attempt.
func (m *Manager) adoptStage(ctx context.Context, id domain.SessionID, start ports.PipelineStageStart) (ports.PipelineStageStarted, error) {
	unsafe := func(format string, args ...any) (ports.PipelineStageStarted, error) {
		return ports.PipelineStageStarted{}, fmt.Errorf("%w: %s", ports.ErrPipelineResumeUnsafe, fmt.Sprintf(format, args...))
	}
	rec, ok, err := m.store.GetSession(ctx, id)
	if err != nil {
		return ports.PipelineStageStarted{}, fmt.Errorf("load the stage's existing session: %w", err)
	}
	if !ok || rec.IsTerminated {
		return unsafe("the stage session %s created for this attempt is gone or terminated", id)
	}
	probe, hasProbe := m.chat.(liveChatProbe)
	if m.chat == nil || !hasProbe || !probe.HasLiveChatController(id) {
		return unsafe("the stage session %s was created for this attempt but its controller is not running", id)
	}
	if err := m.deliverStagePrompt(m.stagePromptContext(ctx, start), id, start.Prompt); err != nil {
		return ports.PipelineStageStarted{}, fmt.Errorf("deliver the stage prompt to the adopted session: %w", err)
	}
	after, err := m.getRecord(ctx, id)
	if err != nil {
		return ports.PipelineStageStarted{}, err
	}
	return ports.PipelineStageStarted{SessionID: id, ControllerGeneration: after.Metadata.ControllerGeneration, ProviderConversationID: after.Metadata.ProviderConversationID}, nil
}

// ReconnectExecutor implements ports.PipelineExecutor. It is the same
// reconnect-only path the daemon's startup health check uses: it adopts a
// provider host that is still running and never creates one.
func (m *Manager) ReconnectExecutor(ctx context.Context, id domain.SessionID) (bool, error) {
	if m.chat == nil {
		return false, nil
	}
	if m.chat.HasLiveChatController(id) {
		return true, nil
	}
	rec, ok, err := m.store.GetSession(ctx, id)
	if err != nil {
		return false, fmt.Errorf("load session %s: %w", id, err)
	}
	if !ok || rec.IsTerminated || rec.Metadata.ProviderConversationID == "" {
		return false, nil
	}
	project, err := m.loadProject(ctx, rec.ProjectID)
	if err != nil {
		return false, err
	}
	_, err = m.resumeChatController(ports.WithPipelineBypass(ctx), "reconnect pipeline stage", rec, project,
		workspaceInfo(rec), false, true, "", domain.SessionInterfaceTransitionHistoryStrict)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ports.ErrChatHostNotRunning):
		return false, nil
	}
	return false, err
}

// RestoreExecutor implements ports.PipelineExecutor. It is only called after a
// person chose to recover: it resumes the SAME native conversation (the session's
// recorded provider conversation) and refuses anything that would start a new one.
func (m *Manager) RestoreExecutor(ctx context.Context, id domain.SessionID) error {
	if m.chat == nil {
		return fmt.Errorf("%w: Chat is not available in this build", ports.ErrPipelineStageUnsupported)
	}
	if m.chat.HasLiveChatController(id) {
		return nil
	}
	rec, ok, err := m.store.GetSession(ctx, id)
	if err != nil {
		return fmt.Errorf("load session %s: %w", id, err)
	}
	if !ok || rec.IsTerminated {
		return fmt.Errorf("session %s is gone or terminated", id)
	}
	if rec.Metadata.ProviderConversationID == "" {
		return fmt.Errorf("session %s has no recorded conversation to resume; AO will not start a fresh one", id)
	}
	project, err := m.loadProject(ctx, rec.ProjectID)
	if err != nil {
		return err
	}
	if _, err := m.resumeChatController(ports.WithPipelineBypass(ctx), "restore pipeline stage", rec, project,
		workspaceInfo(rec), false, false, "", domain.SessionInterfaceTransitionHistoryStrict); err != nil {
		return fmt.Errorf("resume the stage's conversation: %w", err)
	}
	return nil
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

// ResumeExecutor implements ports.PipelineExecutor. It only ever resumes a
// conversation whose controller is still running here and was fenced by an
// earlier relinquish: reopening it is just lifting the fence and delivering the
// next turn. Anything that would mean launching a fresh controller (and so
// risking an unrelated conversation presented as continuous) is refused as
// unsafe so the run can ask a human.
func (m *Manager) ResumeExecutor(ctx context.Context, id domain.SessionID, prompt string) (ports.PipelineStageStarted, error) {
	unsafe := func(format string, args ...any) (ports.PipelineStageStarted, error) {
		return ports.PipelineStageStarted{}, fmt.Errorf("%w: %s", ports.ErrPipelineResumeUnsafe, fmt.Sprintf(format, args...))
	}
	if m.chat == nil {
		return unsafe("Chat is not available in this build")
	}
	rec, ok, err := m.store.GetSession(ctx, id)
	if err != nil || !ok || rec.IsTerminated {
		return unsafe("session %s is gone or terminated", id)
	}
	probe, hasProbe := m.chat.(liveChatProbe)
	if !hasProbe || !probe.HasLiveChatController(id) {
		return unsafe("its controller is not running, so its native conversation would have to be restored")
	}
	if handoff, supported := m.chat.(chatHandoffLauncher); supported {
		handoff.AbortChatHandoff(id)
	}
	if err := m.deliverStagePrompt(ctx, id, prompt); err != nil {
		return ports.PipelineStageStarted{}, fmt.Errorf("deliver the stage prompt: %w", err)
	}
	after, err := m.getRecord(ctx, id)
	if err != nil {
		return ports.PipelineStageStarted{}, err
	}
	return ports.PipelineStageStarted{SessionID: id, ControllerGeneration: after.Metadata.ControllerGeneration, ProviderConversationID: after.Metadata.ProviderConversationID}, nil
}
