package sessionmanager

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// crewStore adds role lookup to the attached-session fake, reading the same
// attachment marker the real store does.
type crewStore struct{ *attachedStore }

func (s crewStore) GetSessionCrewRole(_ context.Context, id domain.SessionID) (string, error) {
	for marker, sid := range s.forAttempt {
		if sid == id {
			role, _ := domain.CrewRoleFromMarker(marker)
			return role, nil
		}
	}
	return "", nil
}

func (s crewStore) ListLiveCrewRoleSessionIDs(context.Context) ([]domain.SessionID, error) {
	var ids []domain.SessionID
	for marker, sid := range s.forAttempt {
		if _, ok := domain.CrewRoleFromMarker(marker); ok && !s.sessions[sid].IsTerminated {
			ids = append(ids, sid)
		}
	}
	return ids, nil
}

func newCrewRoleManager(t *testing.T) (*Manager, *attachedStore, *recordingLauncher, *fakeWorkspace, domain.SessionRecord) {
	t.Helper()
	m, st, launcher, ws, owner := newPipelineStageManager(t)
	m.store = crewStore{st}
	return m, st, launcher, ws, owner
}

func startRole(t *testing.T, m *Manager, owner domain.SessionRecord, role string) domain.SessionRecord {
	t.Helper()
	rec, err := m.StartCrewRole(context.Background(), ports.CrewRoleStart{
		Owner: owner.ID, RoleID: role, Instructions: "INSTRUCTIONS-" + role, Harness: domain.HarnessClaudeCode,
	})
	if err != nil {
		t.Fatalf("start %s: %v", role, err)
	}
	return rec
}

func TestStartCrewRoleRunsInTheWorkersWorktreeWithItsOwnConversationAndCrewPrompt(t *testing.T) {
	m, st, launcher, ws, owner := newCrewRoleManager(t)
	st.sessions[owner.ID] = withTaskPrompt(st.sessions[owner.ID], "Fix the login bug")

	builder := startRole(t, m, owner, "builder")
	tester := startRole(t, m, owner, "tester")

	if len(launcher.started) != 2 {
		t.Fatalf("each role gets its own controller: %d", len(launcher.started))
	}
	cfg := launcher.started[0]
	if cfg.SessionID != builder.ID || cfg.SessionID == owner.ID || cfg.WorkspacePath != owner.Metadata.WorkspacePath {
		t.Fatalf("a role runs in the worker's own worktree with its own conversation: %+v", cfg)
	}
	if len(launcher.relayed) != 0 || len(launcher.turns) != 0 {
		t.Fatalf("a role waits for the human; nothing is sent on its behalf: %v %v", launcher.relayed, launcher.turns)
	}
	for _, want := range []string{"Crew role: builder", "INSTRUCTIONS-builder", "Fix the login bug", "feat/x", string(owner.ID), "owns the worktree, branch, and PR",
		// Replying to a teammate takes `ao crew send`; chat text never reaches them.
		"you MUST run `ao crew send --to <role> --message", "your teammates never see it", "[crew message from <role>"} {
		if !strings.Contains(cfg.SystemPrompt, want) {
			t.Errorf("system prompt missing %q:\n%s", want, cfg.SystemPrompt)
		}
	}
	// The second role is told about the first; the first, rebuilt now, about the second.
	if !strings.Contains(launcher.started[1].SystemPrompt, "builder (session "+string(builder.ID)+")") {
		t.Errorf("tester must be told about its teammate builder:\n%s", launcher.started[1].SystemPrompt)
	}
	rebuilt, err := m.buildSystemPrompt(context.Background(), domain.KindWorker, "mer", builder.ID, "", true)
	if err != nil || !strings.Contains(rebuilt, "tester (session "+string(tester.ID)+")") || !strings.Contains(rebuilt, "INSTRUCTIONS-builder") {
		t.Fatalf("a resume keeps the role profile and the current crew: err=%v\n%s", err, rebuilt)
	}
	if st.attached[builder.ID] != owner.ID || st.sessions[builder.ID].Mode != domain.SessionModeChat {
		t.Fatalf("role rows are attached Chat sessions of the worker: %v", st.attached)
	}
	if ws.createCount != 0 || ws.destroyed != 0 {
		t.Fatalf("a role never creates or destroys a workspace: %d %d", ws.createCount, ws.destroyed)
	}
}

func withTaskPrompt(rec domain.SessionRecord, prompt string) domain.SessionRecord {
	rec.Metadata.Prompt = prompt
	return rec
}

func TestStartCrewRoleRefusesUnsupportedHarnessWithoutCreatingAnything(t *testing.T) {
	m, st, launcher, _, owner := newCrewRoleManager(t)
	before := len(st.sessions)

	launcher.preflightErr = errors.New("not signed in")
	_, err := m.StartCrewRole(context.Background(), ports.CrewRoleStart{Owner: owner.ID, RoleID: "builder", Harness: domain.HarnessClaudeCode})
	if !errors.Is(err, ports.ErrCrewRoleUnsupported) {
		t.Fatalf("a harness that cannot run Chat is refused: %v", err)
	}
	launcher.preflightErr = nil
	_, err = m.StartCrewRole(context.Background(), ports.CrewRoleStart{Owner: owner.ID, RoleID: "builder", Harness: domain.AgentHarness("nope")})
	if !errors.Is(err, ports.ErrCrewRoleUnsupported) {
		t.Fatalf("unknown harness: %v", err)
	}
	if len(st.sessions) != before || len(launcher.started) != 0 {
		t.Fatalf("a refused role must leave no session or terminal fallback behind: %d sessions, %d controllers", len(st.sessions), len(launcher.started))
	}
}

func TestStartCrewRoleRefusesDuplicatesAndInvalidOwners(t *testing.T) {
	m, st, _, _, owner := newCrewRoleManager(t)
	role := startRole(t, m, owner, "builder")

	_, err := m.StartCrewRole(context.Background(), ports.CrewRoleStart{Owner: owner.ID, RoleID: "builder", Harness: domain.HarnessClaudeCode})
	if !errors.Is(err, ports.ErrCrewRoleExists) {
		t.Fatalf("one live session per role: %v", err)
	}
	_, err = m.StartCrewRole(context.Background(), ports.CrewRoleStart{Owner: role.ID, RoleID: "tester", Harness: domain.HarnessClaudeCode})
	if !errors.Is(err, ports.ErrCrewOwnerInvalid) {
		t.Fatalf("a role session cannot host roles: %v", err)
	}
	dead := st.sessions[owner.ID]
	dead.IsTerminated = true
	st.sessions[owner.ID] = dead
	_, err = m.StartCrewRole(context.Background(), ports.CrewRoleStart{Owner: owner.ID, RoleID: "tester", Harness: domain.HarnessClaudeCode})
	if !errors.Is(err, ports.ErrCrewOwnerInvalid) {
		t.Fatalf("a finished task cannot host roles: %v", err)
	}
}

func TestStopCrewRoleEndsOnlyThatConversationAndAllowsReAdding(t *testing.T) {
	m, st, launcher, ws, owner := newCrewRoleManager(t)
	builder := startRole(t, m, owner, "builder")
	tester := startRole(t, m, owner, "tester")

	if err := m.StopCrewRole(context.Background(), owner.ID, "builder"); err != nil {
		t.Fatal(err)
	}
	if !st.sessions[builder.ID].IsTerminated || st.sessions[tester.ID].IsTerminated || st.sessions[owner.ID].IsTerminated {
		t.Fatal("only the removed role's conversation ends")
	}
	if len(launcher.stopped) != 1 || launcher.stopped[0] != builder.ID || ws.destroyed != 0 {
		t.Fatalf("stopped=%v destroyed=%d", launcher.stopped, ws.destroyed)
	}
	if err := m.StopCrewRole(context.Background(), owner.ID, "builder"); !errors.Is(err, ports.ErrCrewRoleNotFound) {
		t.Fatalf("a removed role is gone: %v", err)
	}
	again := startRole(t, m, owner, "builder")
	if again.ID == builder.ID {
		t.Fatal("re-adding a role starts a fresh conversation")
	}
}

func TestKillingTheWorkerEndsItsRoleSessionsBeforeTheWorktreeGoes(t *testing.T) {
	m, st, launcher, ws, owner := newCrewRoleManager(t)
	builder := startRole(t, m, owner, "builder")
	tester := startRole(t, m, owner, "tester")
	var endedFirst bool
	ws.destroyHook = func() {
		endedFirst = st.sessions[builder.ID].IsTerminated && st.sessions[tester.ID].IsTerminated && len(launcher.stopped) >= 2
	}

	if _, err := m.Kill(context.Background(), owner.ID); err != nil {
		t.Fatal(err)
	}
	if ws.destroyed != 1 || !endedFirst {
		t.Fatalf("no role may run in a deleted directory: destroyed=%d endedFirst=%v stopped=%v", ws.destroyed, endedFirst, launcher.stopped)
	}
}

func TestKillingARoleSessionNeverTouchesTheWorkersWorktree(t *testing.T) {
	m, st, _, ws, owner := newCrewRoleManager(t)
	builder := startRole(t, m, owner, "builder")

	if _, err := m.Kill(context.Background(), builder.ID); err != nil {
		t.Fatal(err)
	}
	if ws.destroyed != 0 || st.sessions[owner.ID].IsTerminated || !st.sessions[builder.ID].IsTerminated {
		t.Fatalf("destroyed=%d owner terminated=%v role terminated=%v", ws.destroyed, st.sessions[owner.ID].IsTerminated, st.sessions[builder.ID].IsTerminated)
	}
}

func TestOrdinaryWorkersAndPipelineStagesGetNoCrewPrompt(t *testing.T) {
	m, st, _, _, owner := newCrewRoleManager(t)
	stage := addAttachedStage(st, owner)
	for name, id := range map[string]domain.SessionID{"worker": owner.ID, "pipeline stage": stage.ID, "new spawn": "mer-99"} {
		prompt, err := m.buildSystemPrompt(context.Background(), domain.KindWorker, "mer", id, "", true)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(prompt, "Crew role") {
			t.Errorf("%s must not receive a crew section:\n%s", name, prompt)
		}
	}
}

// A kill that already owns the worker refuses an overlapping add, so a role can
// never start in a worktree that teardown is about to remove.
func TestStartCrewRoleIsRefusedWhileTheWorkerIsBeingTornDown(t *testing.T) {
	m, st, launcher, _, owner := newCrewRoleManager(t)
	if err := m.beginAgentOperation(context.Background(), owner.ID, agentOperationKill); err != nil {
		t.Fatal(err)
	}
	defer m.endAgentOperation(owner.ID, agentOperationKill)

	before := len(st.sessions)
	_, err := m.StartCrewRole(context.Background(), ports.CrewRoleStart{Owner: owner.ID, RoleID: "builder", Harness: domain.HarnessClaudeCode})
	if !errors.Is(err, ports.ErrCrewOwnerBusy) {
		t.Fatalf("an add overlapping teardown must be refused: %v", err)
	}
	if len(st.sessions) != before || len(launcher.started) != 0 {
		t.Fatalf("a refused add must create nothing: %d sessions, %d controllers", len(st.sessions), len(launcher.started))
	}
}

// A kill arriving while a role is still starting is refused until the role is
// registered, so the cascade cannot snapshot the attachments without it.
func TestKillArrivingDuringRoleCreationCannotMissTheRole(t *testing.T) {
	m, st, launcher, ws, owner := newCrewRoleManager(t)
	var killErr error
	launcher.beforeStart = func(ChatStart) {
		_, killErr = m.Kill(context.Background(), owner.ID)
	}
	role := startRole(t, m, owner, "builder")
	if killErr == nil {
		t.Fatal("a kill must not run while a role is being created")
	}
	if ws.destroyed != 0 || st.sessions[owner.ID].IsTerminated || st.sessions[role.ID].IsTerminated {
		t.Fatalf("nothing is torn down by the refused kill: destroyed=%d", ws.destroyed)
	}
	launcher.beforeStart = nil
	if _, err := m.Kill(context.Background(), owner.ID); err != nil {
		t.Fatal(err)
	}
	if !st.sessions[role.ID].IsTerminated || ws.destroyed != 1 {
		t.Fatalf("a later kill ends the role before the worktree goes: destroyed=%d", ws.destroyed)
	}
}

func TestReconnectCrewRolesAdoptsLiveRoleHostsAfterRestart(t *testing.T) {
	m, st, launcher, _, owner := newCrewRoleManager(t)
	live := startRole(t, m, owner, "builder")
	ended := startRole(t, m, owner, "tester")
	if err := m.StopCrewRole(context.Background(), owner.ID, "tester"); err != nil {
		t.Fatal(err)
	}
	// A restarted daemon has no controller for the role; its host may still run.
	launcher.live = false
	launcher.started = nil

	m.ReconnectCrewRoles(context.Background())

	if len(launcher.started) != 1 || launcher.started[0].SessionID != live.ID || !launcher.started[0].ReconnectOnly {
		t.Fatalf("only the live role is adopted, and only by reconnecting (never a fresh conversation): %+v", launcher.started)
	}
	if st.sessions[ended.ID].IsTerminated != true {
		t.Fatal("an ended role stays ended")
	}
}

type failingStopChat struct {
	*recordingLauncher
	failFor domain.SessionID
}

func (l failingStopChat) StopChat(ctx context.Context, id domain.SessionID) error {
	if id == l.failFor {
		return context.DeadlineExceeded
	}
	return l.recordingLauncher.StopChat(ctx, id)
}

// A role whose provider cannot be confirmed stopped is not removed: it stays
// live and retryable, and the worker's worktree is not deleted underneath it.
func TestFailedRoleShutdownIsNotATeardown(t *testing.T) {
	m, st, launcher, ws, owner := newCrewRoleManager(t)
	role := startRole(t, m, owner, "builder")
	m.chat = failingStopChat{recordingLauncher: launcher, failFor: role.ID}

	if err := m.StopCrewRole(context.Background(), owner.ID, "builder"); err == nil {
		t.Fatal("removal must fail when the provider cannot be stopped")
	}
	if st.sessions[role.ID].IsTerminated {
		t.Fatal("an unconfirmed shutdown must leave the role live")
	}
	if _, found, _ := m.findCrewRole(context.Background(), m.store.(crewRoleStore), owner.ID, "builder"); !found {
		t.Fatal("the role must stay discoverable by name for a retry")
	}

	if _, err := m.Kill(context.Background(), owner.ID); err == nil {
		t.Fatal("the worker kill must not report success while a role provider may still run")
	}
	if ws.destroyed != 0 || st.sessions[owner.ID].IsTerminated {
		t.Fatalf("the shared worktree must be preserved: destroyed=%d", ws.destroyed)
	}

	m.chat = launcher
	if err := m.StopCrewRole(context.Background(), owner.ID, "builder"); err != nil || !st.sessions[role.ID].IsTerminated {
		t.Fatalf("a retry succeeds once the provider stops: %v", err)
	}
}

// Shutdown and durable termination are one exclusive operation on the role: a
// resume already in progress refuses removal and the worker kill, so a provider
// can never be started between the stop and the termination.
func TestRoleRemovalAndOwnerKillRefuseWhileTheRoleIsBeingResumed(t *testing.T) {
	m, st, launcher, ws, owner := newCrewRoleManager(t)
	role := startRole(t, m, owner, "builder")
	if err := m.beginAgentOperation(context.Background(), role.ID, agentOperationResume); err != nil {
		t.Fatal(err)
	}
	stoppedBefore := len(launcher.stopped)

	if err := m.StopCrewRole(context.Background(), owner.ID, "builder"); !errors.Is(err, ErrSwitchInProgress) {
		t.Fatalf("removal must be refused while the role is being resumed: %v", err)
	}
	if _, err := m.Kill(context.Background(), owner.ID); !errors.Is(err, ErrSwitchInProgress) {
		t.Fatalf("the worker kill must be refused too: %v", err)
	}
	if len(launcher.stopped) != stoppedBefore || st.sessions[role.ID].IsTerminated || ws.destroyed != 0 || st.sessions[owner.ID].IsTerminated {
		t.Fatalf("nothing may be stopped or deleted: stopped=%v destroyed=%d", launcher.stopped, ws.destroyed)
	}

	m.endAgentOperation(role.ID, agentOperationResume)
	if err := m.StopCrewRole(context.Background(), owner.ID, "builder"); err != nil || !st.sessions[role.ID].IsTerminated {
		t.Fatalf("removal succeeds once the resume is over: %v", err)
	}
}

// Removal and a direct kill of the role are refused while the role is still
// being created, and no provider starts for a role that was removed.
func TestRoleRemovalAndDirectKillAreRefusedWhileTheRoleIsStarting(t *testing.T) {
	m, st, launcher, ws, owner := newCrewRoleManager(t)
	var removeErr, killErr error
	var roleID domain.SessionID
	launcher.beforeStart = func(cfg ChatStart) {
		roleID = cfg.SessionID
		removeErr = m.StopCrewRole(context.Background(), owner.ID, "builder")
		_, killErr = m.Kill(context.Background(), cfg.SessionID)
	}
	role := startRole(t, m, owner, "builder")

	if !errors.Is(removeErr, ErrSwitchInProgress) || !errors.Is(killErr, ErrSwitchInProgress) {
		t.Fatalf("both must be refused mid-creation: remove=%v kill=%v", removeErr, killErr)
	}
	if role.ID != roleID || st.sessions[role.ID].IsTerminated || len(launcher.started) != 1 || ws.destroyed != 0 {
		t.Fatalf("the role survives and starts exactly once: terminated=%v started=%d", st.sessions[role.ID].IsTerminated, len(launcher.started))
	}
}

func TestStartCrewRoleDoesNotStartAProviderForARoleRemovedDuringCreation(t *testing.T) {
	m, st, launcher, _, owner := newCrewRoleManager(t)
	st.afterAttachedCreate = func(rec domain.SessionRecord) {
		rec.IsTerminated = true
		st.sessions[rec.ID] = rec
	}
	_, err := m.StartCrewRole(context.Background(), ports.CrewRoleStart{Owner: owner.ID, RoleID: "builder", Harness: domain.HarnessClaudeCode})
	if !errors.Is(err, ports.ErrCrewRoleNotFound) || len(launcher.started) != 0 {
		t.Fatalf("a role removed mid-creation must not get a provider: err=%v started=%d", err, len(launcher.started))
	}
	if _, found := st.forAttempt[domain.CrewRoleMarker(owner.ID, "builder")]; found {
		t.Fatal("the failed creation must release the role's claim so it can be added again")
	}
}

// If startup fails after the provider may have launched and the provider cannot
// be proven stopped, the role stays live and retryable and the worker's
// worktree is not removed underneath it.
func TestFailedStartupWithFailedShutdownKeepsTheRoleRetryableAndBlocksWorkerKill(t *testing.T) {
	m, st, launcher, ws, owner := newCrewRoleManager(t)
	launcher.startErr = errors.New("host failed after launch")
	var roleID domain.SessionID
	launcher.beforeStart = func(cfg ChatStart) {
		roleID = cfg.SessionID
		m.chat = failingStopChat{recordingLauncher: launcher, failFor: cfg.SessionID}
	}
	_, err := m.StartCrewRole(context.Background(), ports.CrewRoleStart{Owner: owner.ID, RoleID: "builder", Harness: domain.HarnessClaudeCode})
	if err == nil {
		t.Fatal("startup must fail")
	}
	if st.sessions[roleID].IsTerminated {
		t.Fatal("an unproven shutdown must not mark the role terminated")
	}
	if _, found, _ := m.findCrewRole(context.Background(), m.store.(crewRoleStore), owner.ID, "builder"); !found {
		t.Fatal("the role must stay discoverable by name so it can be retried")
	}
	if _, err := m.Kill(context.Background(), owner.ID); err == nil || ws.destroyed != 0 {
		t.Fatalf("the worker kill must be blocked while the role may be running: err=%v destroyed=%d", err, ws.destroyed)
	}

	m.chat = launcher
	if err := m.StopCrewRole(context.Background(), owner.ID, "builder"); err != nil || !st.sessions[roleID].IsTerminated {
		t.Fatalf("a retry succeeds once the provider stops: %v", err)
	}
}

type failingTerminateLCM struct {
	lifecycleRecorder
	fail bool
}

func (l *failingTerminateLCM) MarkTerminated(ctx context.Context, id domain.SessionID) error {
	if l.fail {
		return errors.New("db unavailable")
	}
	return l.lifecycleRecorder.MarkTerminated(ctx, id)
}

// A failed startup whose row cannot be durably ended keeps the role's claim, so
// the role stays discoverable and retryable rather than orphaned and unreachable.
func TestFailedStartupKeepsTheClaimWhenTheRowCannotBeEnded(t *testing.T) {
	m, st, launcher, _, owner := newCrewRoleManager(t)
	lcm := &failingTerminateLCM{lifecycleRecorder: m.lcm, fail: true}
	m.lcm = lcm
	launcher.startErr = errors.New("host failed")

	if _, err := m.StartCrewRole(context.Background(), ports.CrewRoleStart{Owner: owner.ID, RoleID: "builder", Harness: domain.HarnessClaudeCode}); err == nil {
		t.Fatal("startup must fail")
	}
	if _, found := st.forAttempt[domain.CrewRoleMarker(owner.ID, "builder")]; !found {
		t.Fatal("the claim must be kept while the row is still live")
	}
	lcm.fail = false
	launcher.startErr = nil
	if err := m.StopCrewRole(context.Background(), owner.ID, "builder"); err != nil {
		t.Fatalf("the role is retryable by name: %v", err)
	}
}

// Roles are Chat-only: the public interface-transition surface refuses a role
// before anything is fenced, while ordinary workers keep their normal switching.
func TestCrewRolesCannotSwitchInterfaceButWorkersCan(t *testing.T) {
	m, _, launcher, _, owner := newCrewRoleManager(t)
	role := startRole(t, m, owner, "builder")
	launcher.armed = nil

	status, err := m.InterfaceTransitionStatus(context.Background(), role.ID)
	if err != nil || status.Supported || !strings.Contains(status.Reason, "builder role runs only as a Chat conversation") {
		t.Fatalf("status must refuse with a visible reason: %+v err=%v", status, err)
	}
	_, err = m.StartInterfaceTransition(context.Background(), role.ID, domain.SessionModeTUI,
		domain.SessionInterfaceTransitionDrain, domain.SessionInterfaceTransitionHistoryStrict)
	if !errors.Is(err, ErrInterfaceHandoffUnsupported) || !strings.Contains(err.Error(), "builder crew role") {
		t.Fatalf("start must refuse a role: %v", err)
	}
	if len(launcher.armed) != 0 {
		t.Fatalf("the role's controller must not be fenced: %v", launcher.armed)
	}

	ownerStatus, err := m.InterfaceTransitionStatus(context.Background(), owner.ID)
	if err != nil || strings.Contains(ownerStatus.Reason, "crew role") {
		t.Fatalf("ordinary workers are unaffected: %+v err=%v", ownerStatus, err)
	}
}
