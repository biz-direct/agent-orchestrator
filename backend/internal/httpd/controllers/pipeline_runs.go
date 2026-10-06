package controllers

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apispec"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/envelope"
	runssvc "github.com/aoagents/agent-orchestrator/backend/internal/service/pipelineruns"
)

// PipelineRunsController owns the /sessions/{sessionId}/pipeline routes: start,
// read, and stage-result submission for a pipeline run attached to a worker. A
// nil Mgr keeps routes registered but returns OpenAPI-backed 501s.
type PipelineRunsController struct {
	Mgr runssvc.Manager
}

// Register mounts the pipeline run routes.
func (c *PipelineRunsController) Register(r chi.Router) {
	r.Get("/sessions/{sessionId}/pipeline", c.get)
	r.Post("/sessions/{sessionId}/pipeline", c.start)
	r.Post("/sessions/{sessionId}/pipeline/results", c.submit)
}

func (c *PipelineRunsController) get(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, "GET", "/api/v1/sessions/{sessionId}/pipeline")
		return
	}
	out, err := c.Mgr.Get(r.Context(), sessionID(r))
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}

func (c *PipelineRunsController) start(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/sessions/{sessionId}/pipeline")
		return
	}
	var in runssvc.StartInput
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	in.SessionID = sessionID(r)
	out, err := c.Mgr.Start(r.Context(), in)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusCreated, runssvc.RunEnvelope{Run: &out})
}

func (c *PipelineRunsController) submit(w http.ResponseWriter, r *http.Request) {
	if c.Mgr == nil {
		apispec.NotImplemented(w, r, "POST", "/api/v1/sessions/{sessionId}/pipeline/results")
		return
	}
	var in runssvc.SubmitInput
	if err := decodeJSONStrict(r, &in); err != nil {
		envelope.WriteAPIError(w, r, http.StatusBadRequest, "bad_request", "INVALID_JSON", "Invalid JSON body", nil)
		return
	}
	in.SessionID = sessionID(r)
	out, err := c.Mgr.Submit(r.Context(), in)
	if err != nil {
		envelope.WriteError(w, r, err)
		return
	}
	envelope.WriteJSON(w, http.StatusOK, out)
}
