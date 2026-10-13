package crews_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/crew"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/crews"
)

type fakeSessions struct {
	recs     map[domain.SessionID]domain.SessionRecord
	attached map[domain.SessionID]domain.SessionID
	roles    map[domain.SessionID]string
}

func (f *fakeSessions) GetSession(_ context.Context, id domain.SessionID) (domain.SessionRecord, bool, error) {
	rec, ok := f.recs[id]
	return rec, ok, nil
}
func (f *fakeSessions) GetSessionAttachedTo(_ context.Context, id domain.SessionID) (domain.SessionID, error) {
	return f.attached[id], nil
}
func (f *fakeSessions) GetSessionCrewRole(_ context.Context, id domain.SessionID) (string, error) {
	return f.roles[id], nil
}
func (f *fakeSessions) ListAttachedSessionIDs(_ context.Context, owner domain.SessionID) ([]domain.SessionID, error) {
	var ids []domain.SessionID
	for id, o := range f.attached {
		if o == owner {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

type fakeHost struct {
	sessions     *fakeSessions
	preflightErr error
	started      []ports.CrewRoleStart
	stopped      []string
}

func (h *fakeHost) PreflightCrewRole(context.Context, domain.AgentHarness) error {
	return h.preflightErr
}
func (h *fakeHost) StartCrewRole(_ context.Context, s ports.CrewRoleStart) (domain.SessionRecord, error) {
	h.started = append(h.started, s)
	rec := domain.SessionRecord{ID: domain.SessionID("p-att-" + s.RoleID), Harness: s.Harness, Metadata: domain.SessionMetadata{Model: s.Model}}
	h.sessions.recs[rec.ID] = rec
	h.sessions.attached[rec.ID] = s.Owner
	h.sessions.roles[rec.ID] = s.RoleID
	return rec, nil
}
func (h *fakeHost) StopCrewRole(_ context.Context, _ domain.SessionID, role string) error {
	h.stopped = append(h.stopped, role)
	return nil
}

func newRoleFixture(t *testing.T, files map[string]string) (*crews.Service, *fakeHost, *fakeSessions) {
	t.Helper()
	svc, _ := newFixture(t, files)
	sessions := &fakeSessions{
		recs: map[domain.SessionID]domain.SessionRecord{
			"p-1": {ID: "p-1", ProjectID: "p", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode},
		},
		attached: map[domain.SessionID]domain.SessionID{}, roles: map[domain.SessionID]string{},
	}
	host := &fakeHost{sessions: sessions}
	return svc.WithRoleSessions(sessions, host), host, sessions
}

func TestAddRoleStartsTheRolesProfileInTheTask(t *testing.T) {
	svc, host, _ := newRoleFixture(t, allFiles)
	got, err := svc.AddRole(context.Background(), "p-1", crews.AddRoleInput{Role: "builder"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Role != "builder" || got.OwnerSessionID != "p-1" || got.SessionID == "" || got.Harness != string(domain.HarnessClaudeCode) {
		t.Fatalf("role session: %+v", got)
	}
	if len(host.started) != 1 || host.started[0].Instructions != "build things" || host.started[0].Owner != "p-1" {
		t.Fatalf("the role's instructions reach the session: %+v", host.started)
	}
	list, err := svc.ListRoles(context.Background(), "p-1")
	if err != nil || len(list) != 1 || list[0].Role != "builder" {
		t.Fatalf("list: %v %+v", err, list)
	}
	if err := svc.RemoveRole(context.Background(), "p-1", "builder"); err != nil || len(host.stopped) != 1 {
		t.Fatalf("remove: %v %v", err, host.stopped)
	}
}

func TestAddRoleRefusesWhatCannotRunVisibly(t *testing.T) {
	svc, host, sessions := newRoleFixture(t, map[string]string{
		crew.RolesDir + "/builder.yaml": builderYAML,
		crew.RolesDir + "/broken.yaml":  "version: 1\nid: broken\n",
	})
	ctx := context.Background()
	cases := map[string]struct {
		owner domain.SessionID
		in    crews.AddRoleInput
		want  string
	}{
		"missing role": {"p-1", crews.AddRoleInput{Role: "ghost"}, "CREW_ROLE_NOT_FOUND"},
		"invalid role": {"p-1", crews.AddRoleInput{Role: "broken"}, "CREW_ROLE_INVALID"},
		"no role":      {"p-1", crews.AddRoleInput{}, "INVALID_CREW_ROLE"},
		"unknown task": {"p-9", crews.AddRoleInput{Role: "builder"}, "SESSION_NOT_FOUND"},
	}
	for name, tc := range cases {
		if _, err := svc.AddRole(ctx, tc.owner, tc.in); apiCode(t, err) != tc.want {
			t.Errorf("%s: err=%v want %s", name, err, tc.want)
		}
	}
	sessions.recs["p-2"] = domain.SessionRecord{ID: "p-2", ProjectID: "p", Kind: domain.KindOrchestrator}
	if _, err := svc.AddRole(ctx, "p-2", crews.AddRoleInput{Role: "builder"}); apiCode(t, err) != "CREW_OWNER_INVALID" {
		t.Errorf("orchestrators host no roles: %v", err)
	}
	sessions.recs["p-3"] = domain.SessionRecord{ID: "p-3", ProjectID: "p", Kind: domain.KindWorker}
	sessions.attached["p-3"] = "p-1"
	if _, err := svc.AddRole(ctx, "p-3", crews.AddRoleInput{Role: "builder"}); apiCode(t, err) != "CREW_OWNER_INVALID" {
		t.Errorf("attached sessions host no roles: %v", err)
	}

	host.preflightErr = errors.Join(ports.ErrCrewRoleUnsupported, errors.New("no Chat driver"))
	_, err := svc.AddRole(ctx, "p-1", crews.AddRoleInput{Role: "builder"})
	if apiCode(t, err) != "CREW_ROLE_HARNESS_UNSUPPORTED" || len(host.started) != 0 {
		t.Fatalf("an unsupported harness is refused visibly and nothing starts: %v", err)
	}
}

func TestAddRoleHarnessPrecedenceIsExplicitThenRoleThenTask(t *testing.T) {
	files := map[string]string{
		crew.RolesDir + "/builder.yaml": "version: 1\nid: builder\ndescription: B\ninstructions: x\nharness: codex\nmodel: m1\n",
	}
	svc, host, _ := newRoleFixture(t, files)
	if _, err := svc.AddRole(context.Background(), "p-1", crews.AddRoleInput{Role: "builder"}); err != nil {
		t.Fatal(err)
	}
	if h := host.started[0]; h.Harness != domain.HarnessCodex || h.Model != "m1" {
		t.Fatalf("role default: %+v", h)
	}
	host.sessions.roles, host.sessions.attached = map[domain.SessionID]string{}, map[domain.SessionID]domain.SessionID{}
	if _, err := svc.AddRole(context.Background(), "p-1", crews.AddRoleInput{Role: "builder", Harness: "claude-code", Model: "m2"}); err != nil {
		t.Fatal(err)
	}
	if h := host.started[1]; h.Harness != domain.HarnessClaudeCode || h.Model != "m2" {
		t.Fatalf("explicit choice wins: %+v", h)
	}
}

func TestRoleSessionsUnavailableWithoutAHost(t *testing.T) {
	svc, _ := newFixture(t, allFiles)
	if _, err := svc.AddRole(context.Background(), "p-1", crews.AddRoleInput{Role: "builder"}); apiCode(t, err) != "CREW_ROLES_UNAVAILABLE" {
		t.Fatalf("err = %v", err)
	}
}
