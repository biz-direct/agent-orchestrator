package controllers

import (
	"net/http"

	"github.com/go-chi/chi/v5"

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
}

// Register mounts the crew routes on the supplied router.
func (c *CrewsController) Register(r chi.Router) {
	r.Get("/projects/{id}/crew", c.catalog)
	r.Get("/projects/{id}/crew/default", c.getDefault)
	r.Put("/projects/{id}/crew/default", c.setDefault)
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
