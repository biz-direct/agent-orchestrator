package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/rulesfile"
	settingssvc "github.com/aoagents/agent-orchestrator/backend/internal/service/settings"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
)

type fakePromptService struct {
	effectiveErr error
	gotProject   domain.ProjectID
	gotRole      string
	gotMode      string
}

func (f *fakePromptService) BuiltinPrompts() sessionmanager.BuiltinPrompts {
	return sessionmanager.BuiltinPrompts{
		Sample:       sessionmanager.PromptSample{ProjectID: "sample-project"},
		Orchestrator: []sessionmanager.PromptSection{{Layer: "builtin", Title: "AO Orchestrator Role", Text: "## AO Orchestrator Role"}},
	}
}

func (f *fakePromptService) EffectivePrompt(_ context.Context, id domain.ProjectID, role, mode string) ([]sessionmanager.PromptSection, string, error) {
	f.gotProject, f.gotRole, f.gotMode = id, role, mode
	if f.effectiveErr != nil {
		return nil, "", f.effectiveErr
	}
	if mode == "" {
		mode = "chat"
	}
	return []sessionmanager.PromptSection{{Layer: "builtin", Text: "A"}, {Layer: "global", Text: "B"}}, mode, nil
}

func (f *fakePromptService) PromptSessions(_ context.Context, id domain.ProjectID, role string) ([]sessionmanager.PromptSession, error) {
	f.gotProject, f.gotRole = id, role
	return []sessionmanager.PromptSession{{SessionID: "p-1", ProjectID: "p", Role: role, Freshness: "stale"}}, nil
}

func promptsRouter(svc PromptService) http.Handler {
	r := chi.NewRouter()
	(&PromptsController{Svc: svc}).Register(r)
	return r
}

func promptsGet(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	return w
}

func TestPromptsBuiltin(t *testing.T) {
	w := promptsGet(t, promptsRouter(&fakePromptService{}), "/prompts/builtin")
	var got BuiltinPromptsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK {
		t.Fatalf("status=%d err=%v body=%s", w.Code, err, w.Body)
	}
	if got.Sample.ProjectID != "sample-project" || len(got.Orchestrator) != 1 || got.Orchestrator[0].Layer != "builtin" {
		t.Fatalf("unexpected response: %+v", got)
	}
}

func TestPromptsEffective(t *testing.T) {
	svc := &fakePromptService{}
	w := promptsGet(t, promptsRouter(svc), "/projects/mer/effective-prompt?role=worker")
	var got EffectivePromptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK {
		t.Fatalf("status=%d err=%v body=%s", w.Code, err, w.Body)
	}
	if svc.gotProject != "mer" || svc.gotRole != "worker" || got.Text != "A\n\nB" || len(got.Sections) != 2 || got.Mode != "chat" {
		t.Fatalf("got %+v (project %q role %q)", got, svc.gotProject, svc.gotRole)
	}
}

func TestPromptsEffectivePassesMode(t *testing.T) {
	svc := &fakePromptService{}
	w := promptsGet(t, promptsRouter(svc), "/projects/mer/effective-prompt?role=worker&mode=tui")
	var got EffectivePromptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK || svc.gotMode != "tui" || got.Mode != "tui" {
		t.Fatalf("status=%d mode=%q resp=%+v err=%v", w.Code, svc.gotMode, got, err)
	}
}

func TestPromptsRoleValidation(t *testing.T) {
	h := promptsRouter(&fakePromptService{})
	for _, target := range []string{"/projects/mer/effective-prompt", "/projects/mer/effective-prompt?role=reviewer", "/prompts/sessions?role="} {
		if w := promptsGet(t, h, target); w.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400", target, w.Code)
		}
	}
}

func TestPromptsEffectiveErrors(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"bad mode", sessionmanager.ErrUnknownPromptMode, http.StatusBadRequest, "PROMPT_MODE_INVALID"},
		{"unknown project", sessionmanager.ErrUnknownPromptProject, http.StatusNotFound, "PROJECT_NOT_FOUND"},
		{"broken worker rules file", &rulesfile.Error{Field: rulesfile.FieldAgentRulesFile, Path: "x.md", Reason: rulesfile.ReasonNotFound}, http.StatusBadRequest, "AGENT_RULES_FILE_INVALID"},
		{"broken orchestrator rules file", &rulesfile.Error{Field: rulesfile.FieldOrchestratorRulesFile, Path: "x.md", Reason: rulesfile.ReasonTooLarge}, http.StatusBadRequest, "ORCHESTRATOR_RULES_FILE_INVALID"},
	}
	for _, tc := range cases {
		w := promptsGet(t, promptsRouter(&fakePromptService{effectiveErr: tc.err}), "/projects/mer/effective-prompt?role=worker")
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.code) {
			t.Fatalf("%s: status=%d body=%s", tc.name, w.Code, w.Body)
		}
	}
}

func TestPromptsSessions(t *testing.T) {
	svc := &fakePromptService{}
	w := promptsGet(t, promptsRouter(svc), "/prompts/sessions?role=orchestrator&projectId=p")
	var got PromptSessionsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK {
		t.Fatalf("status=%d err=%v body=%s", w.Code, err, w.Body)
	}
	if svc.gotProject != "p" || svc.gotRole != "orchestrator" || len(got.Sessions) != 1 || got.Sessions[0].Freshness != "stale" {
		t.Fatalf("got %+v (project %q role %q)", got, svc.gotProject, svc.gotRole)
	}
}

func TestPromptsNilServiceIs501(t *testing.T) {
	if w := promptsGet(t, promptsRouter(nil), "/prompts/builtin"); w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", w.Code)
	}
}

type workerRulesSettingsService struct {
	SettingsService
	err   error
	saved string
}

func (s *workerRulesSettingsService) SetGlobalWorkerRules(_ context.Context, rules string) (settingssvc.Snapshot, error) {
	if s.err != nil {
		return settingssvc.Snapshot{}, s.err
	}
	s.saved = rules
	return settingssvc.Snapshot{GlobalWorkerRules: rules}, nil
}
func (s *workerRulesSettingsService) ChatHarnesses([]domain.AgentHarness) []domain.AgentHarness {
	return nil
}
func (s *workerRulesSettingsService) Offering() settingssvc.Offering { return settingssvc.Offering{} }

func patchWorkerRules(c *SettingsController, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c.setGlobalWorkerRules(w, httptest.NewRequest(http.MethodPatch, "/api/v1/settings/global-worker-rules", strings.NewReader(body)))
	return w
}

func TestSettingsGlobalWorkerRules(t *testing.T) {
	svc := &workerRulesSettingsService{}
	c := &SettingsController{Svc: svc}

	if w := patchWorkerRules(c, `{}`); w.Code != http.StatusBadRequest {
		t.Fatalf("missing field status = %d, want 400", w.Code)
	}
	w := patchWorkerRules(c, `{"globalWorkerRules":"Be kind."}`)
	var got SettingsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK || got.GlobalWorkerRules != "Be kind." || svc.saved != "Be kind." {
		t.Fatalf("status=%d response=%+v err=%v", w.Code, got, err)
	}

	svc.err = domain.ErrWorkerRulesTooLarge
	w = patchWorkerRules(c, `{"globalWorkerRules":"x"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "WORKER_RULES_TOO_LARGE") {
		t.Fatalf("too large: status=%d body=%s", w.Code, w.Body)
	}
	svc.err = errors.New("db down")
	if w := patchWorkerRules(c, `{"globalWorkerRules":"x"}`); w.Code < 500 {
		t.Fatalf("store failure status = %d, want 5xx", w.Code)
	}
}
