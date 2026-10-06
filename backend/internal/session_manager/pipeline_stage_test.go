package sessionmanager

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// attachedStore adds the one store capability attached stages need to the fake.
type attachedStore struct {
	*fakeStore
	attached map[domain.SessionID]domain.SessionID
}

func (s *attachedStore) CreateAttachedSession(_ context.Context, rec domain.SessionRecord, owner domain.SessionID) (domain.SessionRecord, error) {
	s.num++
	rec.ID = domain.SessionID(fmt.Sprintf("%s-att-%d", rec.ProjectID, s.num))
	s.sessions[rec.ID] = rec
	if s.attached == nil {
		s.attached = map[domain.SessionID]domain.SessionID{}
	}
	s.attached[rec.ID] = owner
	return rec, nil
}

type fixedGate struct {
	admit  bool
	reason string
	asked  []domain.SessionID
}

func (g *fixedGate) AdmitSessionExecution(_ context.Context, id domain.SessionID) (bool, string) {
	g.asked = append(g.asked, id)
	return g.admit, g.reason
}

func newPipelineStageManager(t *testing.T) (*Manager, *attachedStore, *recordingLauncher, *fakeWorkspace, domain.SessionRecord) {
	t.Helper()
	launcher := &recordingLauncher{live: true}
	st := &attachedStore{fakeStore: newFakeStore()}
	st.projects["mer"] = domain.ProjectRecord{ID: "mer", Path: "/repo", Config: testRoleAgents()}
	ws := &fakeWorkspace{}
	lookPath := func(string) (string, error) { return "/bin/true", nil }
	m := New(Deps{
		Runtime: &fakeRuntime{}, Agents: fakeAgents{}, Workspace: ws, Store: st, Messenger: &fakeMessenger{},
		Chat: launcher, Lifecycle: &fakeLCM{store: st.fakeStore}, DataDir: "/ao-test-data", LookPath: lookPath,
	})
	now := time.Now().UTC()
	owner := domain.SessionRecord{
		ID: "mer-1", ProjectID: "mer", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat,
		Activity:  domain.Activity{State: domain.ActivityIdle, LastActivityAt: now},
		Metadata:  domain.SessionMetadata{WorkspacePath: "/ws/mer-1", WorkspaceRepoPath: "/repo", Branch: "feat/x", Permissions: domain.PermissionModeAcceptEdits, ControllerGeneration: "gen-owner"},
		CreatedAt: now, UpdatedAt: now,
	}
	st.sessions[owner.ID] = owner
	return m, st, launcher, ws, owner
}

func TestStartStageRunsTheSpecialistInTheOwnersWorktreeWithItsOwnConversation(t *testing.T) {
	m, st, launcher, ws, owner := newPipelineStageManager(t)
	started, err := m.StartStage(context.Background(), ports.PipelineStageStart{
		RunID: "r1", StageID: "test", AttemptID: "a1", Owner: owner.ID, Harness: domain.HarnessClaudeCode, Model: "opus",
		SystemPrompt: "SYSTEM", Prompt: "PROMPT",
	})
	if err != nil {
		t.Fatal(err)
	}
	if started.SessionID == "" || started.SessionID == owner.ID || started.ControllerGeneration == "" {
		t.Fatalf("started: %+v", started)
	}
	if len(launcher.started) != 1 {
		t.Fatalf("exactly one controller starts: %d", len(launcher.started))
	}
	cfg := launcher.started[0]
	if cfg.SessionID != started.SessionID || cfg.WorkspacePath != owner.Metadata.WorkspacePath || cfg.SystemPrompt != "SYSTEM" || cfg.Kind != domain.KindWorker || cfg.Model != "opus" {
		t.Fatalf("the specialist must run in the worker's own worktree with its own conversation: %+v", cfg)
	}
	if cfg.Permissions != domain.PermissionModeAcceptEdits {
		t.Fatalf("the specialist inherits the worker's pinned permissions, got %q", cfg.Permissions)
	}
	if len(launcher.turns) != 1 || launcher.turns[0] != "PROMPT" {
		t.Fatalf("stage prompt: %v", launcher.turns)
	}
	if st.attached[started.SessionID] != owner.ID {
		t.Fatalf("the row must be attached to its owner, not an unrelated task: %v", st.attached)
	}
	if ws.createCount != 0 || ws.destroyed != 0 {
		t.Fatalf("a stage session never creates or destroys a workspace: create=%d destroy=%d", ws.createCount, ws.destroyed)
	}
	got := st.sessions[started.SessionID]
	if got.Metadata.WorkspacePath != owner.Metadata.WorkspacePath || got.Metadata.Branch != owner.Metadata.Branch {
		t.Fatalf("attached metadata: %+v", got.Metadata)
	}
}

func TestStartStageFailureStopsTheControllerAndLeavesTheWorkspaceAlone(t *testing.T) {
	m, st, launcher, ws, owner := newPipelineStageManager(t)
	launcher.turnErr = errors.New("provider rejected the turn")
	if _, err := m.StartStage(context.Background(), ports.PipelineStageStart{StageID: "test", Owner: owner.ID, Harness: domain.HarnessClaudeCode, Prompt: "P"}); err == nil {
		t.Fatal("a stage whose prompt was not accepted has not started")
	}
	if len(launcher.stopped) != 1 {
		t.Fatalf("the half-started controller must be stopped: %v", launcher.stopped)
	}
	if ws.destroyed != 0 || ws.createCount != 0 {
		t.Fatal("cleanup must never touch the shared workspace")
	}
	if _, ok := st.sessions[owner.ID]; !ok {
		t.Fatal("the owner worker is untouched")
	}
}

func TestStartStageRefusesAnOwnerWithoutALiveWorkspace(t *testing.T) {
	m, st, _, _, owner := newPipelineStageManager(t)
	owner.IsTerminated = true
	st.sessions[owner.ID] = owner
	if _, err := m.StartStage(context.Background(), ports.PipelineStageStart{StageID: "test", Owner: owner.ID, Harness: domain.HarnessClaudeCode}); err == nil {
		t.Fatal("no workspace to share")
	}
}

func TestPreflightStageRefusesWhatCannotRunAsChat(t *testing.T) {
	m, _, launcher, _, _ := newPipelineStageManager(t)
	if err := m.PreflightStage(context.Background(), domain.HarnessClaudeCode); err != nil {
		t.Fatalf("supported: %v", err)
	}
	launcher.preflightErr = errors.New("not signed in")
	err := m.PreflightStage(context.Background(), domain.HarnessClaudeCode)
	if !errors.Is(err, ports.ErrPipelineStageUnsupported) {
		t.Fatalf("a failing preflight is unsupported, never a terminal fallback: %v", err)
	}
	if err := m.PreflightStage(context.Background(), domain.AgentHarness("nope")); !errors.Is(err, ports.ErrPipelineStageUnsupported) {
		t.Fatalf("unknown harness: %v", err)
	}
	m.chat = nil
	if err := m.PreflightStage(context.Background(), domain.HarnessClaudeCode); !errors.Is(err, ports.ErrPipelineStageUnsupported) {
		t.Fatalf("no Chat in this build: %v", err)
	}
}

func TestRelinquishExecutorFencesDrainsAndVerifiesQuiescence(t *testing.T) {
	m, _, launcher, _, owner := newPipelineStageManager(t)
	if err := m.RelinquishExecutor(context.Background(), owner.ID); err != nil {
		t.Fatalf("quiescent executor: %v", err)
	}
	if len(launcher.armed) != 1 || len(launcher.prepared) != 1 || launcher.armPolicy[0] != domain.SessionInterfaceTransitionDrain || launcher.preparePolicy[0] != domain.SessionInterfaceTransitionDrain {
		t.Fatalf("intake must be closed and drained: armed=%v prepared=%v", launcher.armed, launcher.prepared)
	}
	if len(launcher.aborted) != 0 {
		t.Fatal("a relinquished executor stays fenced; the fence is never reopened by the handoff")
	}
}

func TestRelinquishExecutorReportsUncertaintyInsteadOfGuessing(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(*recordingLauncher, *attachedStore, domain.SessionRecord)
		want  string
	}{
		"drain fails": {
			setup: func(l *recordingLauncher, _ *attachedStore, _ domain.SessionRecord) {
				l.prepareErr = errors.New("context deadline exceeded")
			},
			want: "did not drain",
		},
		"agent still active": {
			setup: func(_ *recordingLauncher, st *attachedStore, o domain.SessionRecord) {
				o.Activity.State = domain.ActivityActive
				st.sessions[o.ID] = o
			},
			want: "still reports activity",
		},
		"pending approval": {
			setup: func(_ *recordingLauncher, st *attachedStore, o domain.SessionRecord) {
				o.Activity.State = domain.ActivityWaitingInput
				st.sessions[o.ID] = o
			},
			want: "permission or input",
		},
		"blocked": {
			setup: func(_ *recordingLauncher, st *attachedStore, o domain.SessionRecord) {
				o.Activity.State = domain.ActivityBlocked
				st.sessions[o.ID] = o
			},
			want: "permission or input",
		},
	} {
		t.Run(name, func(t *testing.T) {
			m, st, launcher, _, owner := newPipelineStageManager(t)
			tc.setup(launcher, st, owner)
			err := m.RelinquishExecutor(context.Background(), owner.ID)
			if !errors.Is(err, ports.ErrPipelineExecutionUncertain) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want uncertainty containing %q", err, tc.want)
			}
		})
	}
}

func TestRelinquishExecutorAcceptsAMissingControllerButNotAnActiveAgent(t *testing.T) {
	m, st, launcher, _, owner := newPipelineStageManager(t)
	launcher.live = false
	if err := m.RelinquishExecutor(context.Background(), owner.ID); err != nil {
		t.Fatalf("a controller that is not running cannot be running a turn: %v", err)
	}
	if len(launcher.armed) != 0 {
		t.Fatal("nothing to fence without a live controller")
	}
	owner.Activity.State = domain.ActivityActive
	st.sessions[owner.ID] = owner
	if err := m.RelinquishExecutor(context.Background(), owner.ID); !errors.Is(err, ports.ErrPipelineExecutionUncertain) {
		t.Fatalf("an agent still reporting activity is not quiescent even without a controller: %v", err)
	}
	if err := m.RelinquishExecutor(context.Background(), "ghost"); !errors.Is(err, ports.ErrPipelineExecutionUncertain) {
		t.Fatalf("a missing session cannot be proven stopped: %v", err)
	}
}

func TestStopStageOnlyStopsTheController(t *testing.T) {
	m, _, launcher, ws, _ := newPipelineStageManager(t)
	if err := m.StopStage(context.Background(), "mer-att-9"); err != nil {
		t.Fatal(err)
	}
	if len(launcher.stopped) != 1 || launcher.stopped[0] != "mer-att-9" || ws.destroyed != 0 {
		t.Fatalf("stopped=%v destroyed=%d", launcher.stopped, ws.destroyed)
	}
}

func TestPipelineGateBlocksSendsAndRestoresButNotItsOwnDeliveries(t *testing.T) {
	m, _, launcher, _, owner := newPipelineStageManager(t)
	gate := &fixedGate{admit: false, reason: `running stage "test" in a different conversation`}
	m.SetPipelineGate(gate)

	err := m.Send(context.Background(), owner.ID, "wake up", nil)
	if !errors.Is(err, ports.ErrPipelineExecutionOwned) || !strings.Contains(err.Error(), "different conversation") {
		t.Fatalf("a message cannot wake a stage that is not executing: %v", err)
	}
	if len(launcher.relayed) != 0 {
		t.Fatalf("nothing may reach the controller: %v", launcher.relayed)
	}
	if err := m.Send(ports.WithPipelineBypass(context.Background()), owner.ID, "stage instructions", nil); err != nil {
		t.Fatalf("the pipeline's own delivery must pass: %v", err)
	}
	if len(launcher.relayed) != 1 {
		t.Fatalf("bypassed delivery: %v", launcher.relayed)
	}

	// Restore/resume of a non-executing session would start a second writer.
	project := domain.ProjectRecord{ID: "mer", Config: testRoleAgents()}
	if _, err := m.resumeChatController(context.Background(), "restore", owner, project, ports.WorkspaceInfo{Path: owner.Metadata.WorkspacePath}, false, false, "", ""); !errors.Is(err, ports.ErrPipelineExecutionOwned) {
		t.Fatalf("restore must be refused by the gate: %v", err)
	}
	if len(launcher.started) != 0 {
		t.Fatal("no controller may start behind the gate")
	}

	gate.admit = true
	if err := m.Send(context.Background(), owner.ID, "ok now", nil); err != nil {
		t.Fatalf("the active executor is admitted: %v", err)
	}
}

func TestSessionsWithoutAGateBehaveAsBefore(t *testing.T) {
	m, _, launcher, _, owner := newPipelineStageManager(t)
	if err := m.Send(context.Background(), owner.ID, "hello", nil); err != nil {
		t.Fatalf("no gate, no change: %v", err)
	}
	if len(launcher.relayed) != 1 {
		t.Fatalf("relayed: %v", launcher.relayed)
	}
}
