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
	for _, want := range []string{"Crew role: builder", "INSTRUCTIONS-builder", "Fix the login bug", "feat/x", string(owner.ID), "owns the worktree, branch, and PR"} {
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
