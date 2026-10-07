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
	attached   map[domain.SessionID]domain.SessionID
	forAttempt map[string]domain.SessionID
}

func (s *attachedStore) FindAttachedSessionForAttempt(_ context.Context, attemptID string) (domain.SessionID, bool, error) {
	id, ok := s.forAttempt[attemptID]
	return id, ok, nil
}

func (s *attachedStore) CreateAttachedSessionForAttempt(ctx context.Context, rec domain.SessionRecord, owner domain.SessionID, attemptID string) (domain.SessionRecord, error) {
	out, err := s.CreateAttachedSession(ctx, rec, owner)
	if err == nil && attemptID != "" {
		if s.forAttempt == nil {
			s.forAttempt = map[string]domain.SessionID{}
		}
		s.forAttempt[attemptID] = out.ID
	}
	return out, err
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

func (s *attachedStore) GetSessionAttachedTo(_ context.Context, id domain.SessionID) (domain.SessionID, error) {
	return s.attached[id], nil
}

func (s *attachedStore) ListAttachedSessionIDs(_ context.Context, owner domain.SessionID) ([]domain.SessionID, error) {
	var ids []domain.SessionID
	for id, o := range s.attached {
		if o == owner {
			ids = append(ids, id)
		}
	}
	return ids, nil
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
	if len(launcher.relayed) != 1 || launcher.relayed[0] != "PROMPT" || launcher.relayIDs[0] != "pipeline-stage:a1" {
		t.Fatalf("the stage prompt is one turn under a key that is stable for the attempt: %v %v", launcher.relayed, launcher.relayIDs)
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

// A crash after the session was created but before the attempt was confirmed
// must not launch a second specialist: the surviving one is adopted and the
// prompt is redelivered under the same idempotent key.
func TestStartStageAdoptsTheSessionCreatedForTheSameAttemptInsteadOfDuplicatingIt(t *testing.T) {
	m, st, launcher, _, owner := newPipelineStageManager(t)
	start := ports.PipelineStageStart{RunID: "r1", StageID: "test", AttemptID: "a1", Owner: owner.ID, Harness: domain.HarnessClaudeCode, SystemPrompt: "S", Prompt: "PROMPT"}
	first, err := m.StartStage(context.Background(), start)
	if err != nil {
		t.Fatal(err)
	}
	// "Restart": the same attempt is started again.
	second, err := m.StartStage(context.Background(), start)
	if err != nil {
		t.Fatal(err)
	}
	if second.SessionID != first.SessionID {
		t.Fatalf("the surviving session is adopted: %s vs %s", second.SessionID, first.SessionID)
	}
	if len(launcher.started) != 1 || len(st.attached) != 1 {
		t.Fatalf("no duplicate controller or session: controllers=%d sessions=%d", len(launcher.started), len(st.attached))
	}
	if len(launcher.relayed) != 2 || launcher.relayIDs[0] != launcher.relayIDs[1] || launcher.relayIDs[0] == "" {
		t.Fatalf("redelivery must reuse the idempotent key so no second turn can start: %v", launcher.relayIDs)
	}
	// A different attempt gets its own session.
	other := start
	other.AttemptID = "a2"
	third, err := m.StartStage(context.Background(), other)
	if err != nil || third.SessionID == first.SessionID || len(launcher.started) != 2 {
		t.Fatalf("another attempt starts its own conversation: %+v err=%v", third, err)
	}
}

func TestStartStageNeverReplacesALostControllerWithAFreshConversation(t *testing.T) {
	m, st, launcher, _, owner := newPipelineStageManager(t)
	start := ports.PipelineStageStart{RunID: "r1", StageID: "test", AttemptID: "a1", Owner: owner.ID, Harness: domain.HarnessClaudeCode, Prompt: "PROMPT"}
	if _, err := m.StartStage(context.Background(), start); err != nil {
		t.Fatal(err)
	}
	launcher.live = false // the daemon restarted and the controller did not come back
	_, err := m.StartStage(context.Background(), start)
	if !errors.Is(err, ports.ErrPipelineResumeUnsafe) {
		t.Fatalf("a lost controller asks for a recovery decision: %v", err)
	}
	if len(launcher.started) != 1 || len(st.attached) != 1 {
		t.Fatalf("no fresh conversation: controllers=%d sessions=%d", len(launcher.started), len(st.attached))
	}
}

func TestInterruptExecutorInterruptsThenProvesQuiescenceAndReleaseReopensIntake(t *testing.T) {
	m, st, launcher, _, owner := newPipelineStageManager(t)
	if err := m.InterruptExecutor(context.Background(), owner.ID); err != nil {
		t.Fatalf("an idle executor is quiescent after the interrupt: %v", err)
	}
	if len(launcher.armPolicy) != 1 || launcher.armPolicy[0] != domain.SessionInterfaceTransitionInterrupt || launcher.preparePolicy[0] != domain.SessionInterfaceTransitionInterrupt {
		t.Fatalf("pause uses the interrupt policy: arm=%v prepare=%v", launcher.armPolicy, launcher.preparePolicy)
	}
	// A pending permission request is never answered: the interrupt cannot be
	// reported as a confirmed stop.
	waiting := st.sessions[owner.ID]
	waiting.Activity.State = domain.ActivityWaitingInput
	st.sessions[owner.ID] = waiting
	err := m.InterruptExecutor(context.Background(), owner.ID)
	if !errors.Is(err, ports.ErrPipelineExecutionUncertain) || !strings.Contains(err.Error(), "permission or input request") {
		t.Fatalf("an unanswered prompt must be reported as unconfirmed: %v", err)
	}
	if len(launcher.relayed) != 0 {
		t.Fatalf("AO must not deliver anything on the user's behalf: %v", launcher.relayed)
	}
	if err := m.ReleaseExecutor(context.Background(), owner.ID); err != nil || len(launcher.aborted) != 1 || launcher.aborted[0] != owner.ID {
		t.Fatalf("release reopens the fenced intake without delivering a turn: %v %v", err, launcher.aborted)
	}
}

func TestReconnectExecutorOnlyAdoptsWhatIsAlreadyRunning(t *testing.T) {
	m, st, launcher, _, owner := newPipelineStageManager(t)
	launcher.live = true
	if ok, err := m.ReconnectExecutor(context.Background(), owner.ID); err != nil || !ok {
		t.Fatalf("a live controller is adopted as is: %v %v", ok, err)
	}
	// No controller and no recorded conversation: nothing to reconnect to, and
	// nothing is launched to find out.
	launcher.live = false
	if ok, err := m.ReconnectExecutor(context.Background(), owner.ID); err != nil || ok {
		t.Fatalf("an unknown host is reported as not reconnected, not as an error: %v %v", ok, err)
	}
	gone := st.sessions[owner.ID]
	gone.IsTerminated = true
	st.sessions[owner.ID] = gone
	if ok, err := m.ReconnectExecutor(context.Background(), owner.ID); err != nil || ok {
		t.Fatalf("a terminated session is never reconnected: %v %v", ok, err)
	}
	if len(launcher.started) != 0 {
		t.Fatalf("reconnecting must never start a controller: %d", len(launcher.started))
	}
}

func TestRestoreExecutorOnlyActsOnAControllerThatIsGone(t *testing.T) {
	m, _, launcher, _, owner := newPipelineStageManager(t)
	launcher.live = true
	if err := m.RestoreExecutor(context.Background(), owner.ID); err != nil {
		t.Fatalf("a live controller needs nothing: %v", err)
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

func TestResumeExecutorReopensTheFencedConversationAndDeliversThePrompt(t *testing.T) {
	m, st, launcher, _, owner := newPipelineStageManager(t)
	// The gate would refuse the worker (another stage owns execution); the
	// coordinator's own resume must still get through.
	m.SetPipelineGate(&fixedGate{admit: false, reason: "handoff in progress"})
	started, err := m.ResumeExecutor(context.Background(), owner.ID, "REPAIR PROMPT")
	if err != nil {
		t.Fatal(err)
	}
	if started.SessionID != owner.ID || started.ControllerGeneration != owner.Metadata.ControllerGeneration {
		t.Fatalf("the original conversation resumes under its own generation: %+v", started)
	}
	if len(launcher.aborted) != 1 || launcher.aborted[0] != owner.ID {
		t.Fatalf("the fence must be lifted: %v", launcher.aborted)
	}
	if len(launcher.relayed) != 1 || launcher.relayed[0] != "REPAIR PROMPT" {
		t.Fatalf("the prompt is delivered as a turn: %v", launcher.relayed)
	}
	if len(launcher.started) != 0 {
		t.Fatal("resuming must never launch a new controller")
	}
	_ = st
}

func TestResumeExecutorRefusesWhatWouldNeedAFreshController(t *testing.T) {
	m, st, launcher, _, owner := newPipelineStageManager(t)
	launcher.live = false
	if _, err := m.ResumeExecutor(context.Background(), owner.ID, "P"); !errors.Is(err, ports.ErrPipelineResumeUnsafe) {
		t.Fatalf("a controller that is not running cannot be resumed safely: %v", err)
	}
	if len(launcher.started) != 0 || len(launcher.relayed) != 0 {
		t.Fatal("nothing may start or be sent")
	}
	launcher.live = true
	owner.IsTerminated = true
	st.sessions[owner.ID] = owner
	if _, err := m.ResumeExecutor(context.Background(), owner.ID, "P"); !errors.Is(err, ports.ErrPipelineResumeUnsafe) {
		t.Fatalf("a terminated session: %v", err)
	}
	if _, err := m.ResumeExecutor(context.Background(), "ghost", "P"); !errors.Is(err, ports.ErrPipelineResumeUnsafe) {
		t.Fatalf("a missing session: %v", err)
	}
}

// addAttachedStage seeds an attached stage row that mirrors the owner's shared
// workspace, plus a restore marker that must survive the stage ending.
func addAttachedStage(st *attachedStore, owner domain.SessionRecord) domain.SessionRecord {
	stage := owner
	stage.ID = "mer-att-1"
	stage.IsTerminated = false
	st.sessions[stage.ID] = stage
	if st.attached == nil {
		st.attached = map[domain.SessionID]domain.SessionID{}
	}
	st.attached[stage.ID] = owner.ID
	st.worktrees[stage.ID] = []domain.SessionWorktreeRecord{{SessionID: stage.ID}}
	return stage
}

func TestKillOfAnAttachedStageSessionOnlyStopsItsControllerAndEndsTheRow(t *testing.T) {
	m, st, launcher, ws, owner := newPipelineStageManager(t)
	stage := addAttachedStage(st, owner)

	freed, err := m.Kill(context.Background(), stage.ID)
	if err != nil {
		t.Fatal(err)
	}
	if freed || ws.destroyed != 0 {
		t.Fatalf("a stage session must never remove the owner's worktree: freed=%v destroyed=%d", freed, ws.destroyed)
	}
	if len(launcher.stopped) != 1 || launcher.stopped[0] != stage.ID {
		t.Fatalf("the stage's controller must be stopped: %v", launcher.stopped)
	}
	if !st.sessions[stage.ID].IsTerminated {
		t.Fatal("the stage row must end up terminated")
	}
	if st.sessions[owner.ID].IsTerminated || st.sessions[owner.ID].Metadata.WorkspacePath != owner.Metadata.WorkspacePath {
		t.Fatalf("the owner must be untouched: %+v", st.sessions[owner.ID])
	}
	if len(st.worktrees[stage.ID]) != 1 {
		t.Fatal("restore markers are not the stage's to delete")
	}
}

func TestKillOfTheOwnerStopsAndTerminatesItsAttachedStagesBeforeTheWorkspaceGoes(t *testing.T) {
	m, st, launcher, ws, owner := newPipelineStageManager(t)
	stage := addAttachedStage(st, owner)
	var stoppedBeforeDestroy bool
	ws.destroyHook = func() {
		stoppedBeforeDestroy = len(launcher.stopped) > 0 && launcher.stopped[0] == stage.ID && st.sessions[stage.ID].IsTerminated
	}

	if _, err := m.Kill(context.Background(), owner.ID); err != nil {
		t.Fatal(err)
	}
	if !st.sessions[owner.ID].IsTerminated || ws.destroyed != 1 {
		t.Fatalf("the owner is killed normally: terminated=%v destroyed=%d", st.sessions[owner.ID].IsTerminated, ws.destroyed)
	}
	if !st.sessions[stage.ID].IsTerminated {
		t.Fatal("the attached stage leaked: its row is still live")
	}
	if !stoppedBeforeDestroy {
		t.Fatalf("the stage must be stopped and ended before the workspace is removed: stopped=%v", launcher.stopped)
	}
}

func TestRetireForReplacementTreatsAttachedStagesLikeKill(t *testing.T) {
	m, st, launcher, ws, owner := newPipelineStageManager(t)
	stage := addAttachedStage(st, owner)
	if err := m.RetireForReplacement(context.Background(), stage.ID); err != nil {
		t.Fatal(err)
	}
	if ws.destroyed != 0 || !st.sessions[stage.ID].IsTerminated || len(launcher.stopped) != 1 || len(st.worktrees[stage.ID]) != 1 {
		t.Fatalf("retiring a stage must only stop it: destroyed=%d stopped=%v", ws.destroyed, launcher.stopped)
	}

	m, st, _, _, owner = newPipelineStageManager(t)
	stage = addAttachedStage(st, owner)
	if err := m.RetireForReplacement(context.Background(), owner.ID); err != nil {
		t.Fatal(err)
	}
	if !st.sessions[stage.ID].IsTerminated {
		t.Fatal("retiring the owner must terminate its attached stages")
	}
}

func TestCleanupNeverReclaimsTheWorkspaceThroughAnAttachedStage(t *testing.T) {
	m, st, _, ws, owner := newPipelineStageManager(t)
	stage := addAttachedStage(st, owner)
	stage.IsTerminated = true
	st.sessions[stage.ID] = stage
	owner.IsTerminated = false
	res, err := m.Cleanup(context.Background(), owner.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if ws.destroyed != 0 || len(res.Cleaned) != 0 || len(res.Skipped) != 0 {
		t.Fatalf("cleanup must leave the owner's workspace alone: destroyed=%d result=%+v", ws.destroyed, res)
	}
}
