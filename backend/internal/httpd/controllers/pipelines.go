package controllers

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	pipelinessvc "github.com/aoagents/agent-orchestrator/backend/internal/service/pipelines"
)

// PipelinesController owns the /projects/{id}/pipelines routes: the validated
// repository-defined catalog and the project-default pipeline selection. A nil
// Mgr keeps routes registered but returns OpenAPI-backed 501s.
type PipelinesController struct {
	Mgr     pipelinessvc.Manager
	Callers PipelineCallerAuthority
}

// Register mounts the pipeline routes on the supplied router.
func (c *PipelinesController) Register(r chi.Router) {
	r.Get("/projects/{id}/pipelines", c.catalog)
	r.Get("/projects/{id}/pipelines/default", c.getDefault)
	r.Put("/projects/{id}/pipelines/default", c.setDefault)
	r.Put("/projects/{id}/pipelines/command-trust", c.setCommandTrust)
}

func (c *PipelinesController) catalog(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{id}/pipelines")
		return
	}
	out, err := c.Mgr.Catalog(r.Context(), projectID(r))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

func (c *PipelinesController) getDefault(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/projects/{id}/pipelines/default")
		return
	}
	out, err := c.Mgr.GetDefault(r.Context(), projectID(r))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

func (c *PipelinesController) setDefault(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, "PUT", "/api/v1/projects/{id}/pipelines/default")
		return
	}
	var in pipelinessvc.SetDefaultInput
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

func (c *PipelinesController) setCommandTrust(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, "PUT", "/api/v1/projects/{id}/pipelines/command-trust")
		return
	}
	var in pipelinessvc.SetCommandTrustInput
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	// Authorizing repository commands is a person's decision: a request the
	// daemon can attribute to an AO session is bound to "orchestrator" whatever
	// the body claims.
	fromSession, err := c.Callers.SessionCaller(r)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	if fromSession {
		in.RequestedBy = string(domain.PipelineRequestedByOrchestrator)
	}
	out, err := c.Mgr.SetCommandTrust(r.Context(), projectID(r), in)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}
