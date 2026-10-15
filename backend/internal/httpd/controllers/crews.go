package controllers

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	crewssvc "github.com/aoagents/agent-orchestrator/backend/internal/service/crews"
)

// CrewsController owns the /projects/{id}/crew routes: the validated
// repository-defined role and workflow catalog and the project-default crew
// workflow selection. A nil Mgr keeps routes registered but returns
// OpenAPI-backed 501s.
type CrewsController struct {
	Mgr crewssvc.Manager
	// Callers attributes a request to an AO session. Moving the write baton is
	// the human override, so any request that provably comes from a session is
	// refused.
	Callers PipelineCallerAuthority
}

// Register mounts the crew routes on the supplied router.
func (c *CrewsController) Register(r chi.Router) {
	r.Get("/projects/{id}/crew", c.catalog)
	r.Get("/projects/{id}/crew/default", c.getDefault)
	r.Put("/projects/{id}/crew/default", c.setDefault)
	r.Get("/sessions/{sessionId}/crew/roles", c.listRoles)
	r.Post("/sessions/{sessionId}/crew/roles", c.addRole)
	r.Delete("/sessions/{sessionId}/crew/roles/{role}", c.removeRole)
	r.Get("/sessions/{sessionId}/crew/baton", c.getBaton)
	r.Put("/sessions/{sessionId}/crew/baton", c.moveBaton)
	r.Get("/sessions/{sessionId}/crew/messages", c.listMessages)
	r.Post("/sessions/{sessionId}/crew/messages", c.sendMessage)
	r.Patch("/sessions/{sessionId}/crew/messages/{messageId}", c.editMessage)
	r.Delete("/sessions/{sessionId}/crew/messages/{messageId}", c.dropMessage)
	r.Post("/sessions/{sessionId}/crew/messages/{messageId}/release", c.releaseMessage)
}

func (c *CrewsController) getBaton(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/sessions/{sessionId}/crew/baton")
		return
	}
	out, err := c.Mgr.GetBaton(r.Context(), sessionID(r))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

func (c *CrewsController) moveBaton(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, "PUT", "/api/v1/sessions/{sessionId}/crew/baton")
		return
	}
	fromSession, err := c.Callers.SessionCaller(r)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	if fromSession {
		envelope.WriteError(w, r, apierr.Forbidden("CREW_BATON_HUMAN_ONLY", "Only a person can move the write baton; orchestrators and roles cannot"))
		return
	}
	var in crewssvc.MoveBatonInput
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	out, err := c.Mgr.MoveBaton(r.Context(), sessionID(r), in)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

// CrewRoleList is the body of GET /sessions/{sessionId}/crew/roles.
type CrewRoleList struct {
	Roles []crewssvc.RoleSession `json:"roles"`
}

// CrewRoleParam is the {role} path parameter.
type CrewRoleParam struct {
	Role string `path:"role" doc:"Role id"`
}

func (c *CrewsController) listRoles(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/sessions/{sessionId}/crew/roles")
		return
	}
	out, err := c.Mgr.ListRoles(r.Context(), sessionID(r))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, CrewRoleList{Roles: out})
}

func (c *CrewsController) addRole(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/sessions/{sessionId}/crew/roles")
		return
	}
	var in crewssvc.AddRoleInput
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	out, err := c.Mgr.AddRole(r.Context(), sessionID(r), in)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusCreated, out)
}

func (c *CrewsController) removeRole(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, "DELETE", "/api/v1/sessions/{sessionId}/crew/roles/{role}")
		return
	}
	if err := c.Mgr.RemoveRole(r.Context(), sessionID(r), chi.URLParam(r, "role")); err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (c *CrewsController) catalog(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{id}/crew")
		return
	}
	out, err := c.Mgr.Catalog(r.Context(), projectID(r))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

func (c *CrewsController) getDefault(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{id}/crew/default")
		return
	}
	out, err := c.Mgr.GetDefault(r.Context(), projectID(r))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

func (c *CrewsController) setDefault(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, "PUT", "/api/v1/projects/{id}/crew/default")
		return
	}
	var in crewssvc.SetDefaultInput
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	out, err := c.Mgr.SetDefault(r.Context(), projectID(r), in)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

// CrewMessageList is the body of GET /sessions/{sessionId}/crew/messages.
type CrewMessageList struct {
	Messages []crewssvc.CrewMessage `json:"messages"`
}

// CrewMessageParam is the {messageId} path parameter.
type CrewMessageParam struct {
	MessageID string `path:"messageId" doc:"Crew message id"`
}

func (c *CrewsController) listMessages(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/sessions/{sessionId}/crew/messages")
		return
	}
	out, err := c.Mgr.ListMessages(r.Context(), sessionID(r))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, CrewMessageList{Messages: out})
}

func (c *CrewsController) sendMessage(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/sessions/{sessionId}/crew/messages")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2*crewssvc.MaxMessageBytes)
	var in crewssvc.SendMessageInput
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	out, err := c.Mgr.SendMessage(r.Context(), sessionID(r), in)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	status := http.StatusCreated
	if out.Duplicate {
		status = http.StatusOK
	}
	envelope.WriteJSON(w, status, out)
}

func (c *CrewsController) editMessage(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, "PATCH", "/api/v1/sessions/{sessionId}/crew/messages/{messageId}")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2*crewssvc.MaxMessageBytes)
	var in crewssvc.EditMessageInput
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	out, err := c.Mgr.EditMessage(r.Context(), sessionID(r), chi.URLParam(r, "messageId"), in)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

func (c *CrewsController) dropMessage(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, "DELETE", "/api/v1/sessions/{sessionId}/crew/messages/{messageId}")
		return
	}
	out, err := c.Mgr.DropMessage(r.Context(), sessionID(r), chi.URLParam(r, "messageId"))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

func (c *CrewsController) releaseMessage(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/sessions/{sessionId}/crew/messages/{messageId}/release")
		return
	}
	out, err := c.Mgr.ReleaseMessage(r.Context(), sessionID(r), chi.URLParam(r, "messageId"))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}
