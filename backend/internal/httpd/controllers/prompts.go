package controllers

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	"github.com/aoagents/agent-orchestrator/backend/internal/rulesfile"
	sessionmanager "github.com/aoagents/agent-orchestrator/backend/internal/session_manager"
)

// PromptService renders the prompts agents receive. The session manager
// implements it, so every view runs the code a spawn runs.
type PromptService interface {
	BuiltinPrompts() sessionmanager.BuiltinPrompts
	EffectivePrompt(ctx context.Context, projectID domain.ProjectID, role string) ([]sessionmanager.PromptSection, error)
	PromptSessions(ctx context.Context, projectID domain.ProjectID, role string) ([]sessionmanager.PromptSession, error)
}

// PromptsController serves read-only views of AO's built-in prompts and of the
// assembled prompt a new session would get.
type PromptsController struct {
	Svc PromptService
}

// Register mounts the prompt routes.
func (c *PromptsController) Register(r chi.Router) {
	r.Get("/prompts/builtin", c.builtin)
	r.Get("/prompts/sessions", c.sessions)
	r.Get("/projects/{id}/effective-prompt", c.effective)
}

func (c *PromptsController) builtin(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/prompts/builtin")
		return
	}
	b := c.Svc.BuiltinPrompts()
	envelope.WriteJSON(w, http.StatusOK, BuiltinPromptsResponse{Sample: b.Sample, Orchestrator: b.Orchestrator, Worker: b.Worker, StandaloneWorker: b.StandaloneWorker})
}

func (c *PromptsController) effective(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{id}/effective-prompt")
		return
	}
	role, ok := promptRole(w, r)
	if !ok {
		return
	}
	sections, err := c.Svc.EffectivePrompt(r.Context(), projectID(r), role)
	if err != nil {
		writePromptError(w, r, err)
		return
	}
	texts := make([]string, 0, len(sections))
	for _, section := range sections {
		texts = append(texts, section.Text)
	}
	envelope.WriteJSON(w, http.StatusOK, EffectivePromptResponse{Role: role, Sections: sections, Text: strings.Join(texts, "\n\n")})
}

func (c *PromptsController) sessions(w http.ResponseWriter, r *http.Request) {
	if c.Svc == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/prompts/sessions")
		return
	}
	role, ok := promptRole(w, r)
	if !ok {
		return
	}
	sessions, err := c.Svc.PromptSessions(r.Context(), domain.ProjectID(r.URL.Query().Get("projectId")), role)
	if err != nil {
		writePromptError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, PromptSessionsResponse{Sessions: sessions})
}

func promptRole(w http.ResponseWriter, r *http.Request) (string, bool) {
	role := r.URL.Query().Get("role")
	if role != sessionmanager.PromptRoleOrchestrator && role != sessionmanager.PromptRoleWorker {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "validation",
			"PROMPT_ROLE_INVALID", "role must be orchestrator or worker", nil)
		return "", false
	}
	return role, true
}

func writePromptError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, sessionmanager.ErrUnknownPromptRole) {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "validation",
			"PROMPT_ROLE_INVALID", err.Error(), nil)
		return
	}
	if errors.Is(err, sessionmanager.ErrUnknownPromptProject) {
		envelope.WriteAPIError(w, r, http.StatusNotFound, "not_found",
			"PROJECT_NOT_FOUND", "Unknown project", nil)
		return
	}
	// A broken rules file fails the preview the way it would fail a spawn.
	var rfe *rulesfile.Error
	if errors.As(err, &rfe) {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "validation", rfe.Code(), rfe.Error(), rfe.Details())
		return
	}
	envelope.WriteError(w, r, err)
}
