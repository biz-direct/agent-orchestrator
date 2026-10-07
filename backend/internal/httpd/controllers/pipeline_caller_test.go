package controllers_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/pipelineruns"
	pipelinessvc "github.com/aoagents/agent-orchestrator/backend/internal/service/pipelines"
)

type capturingRuns struct {
	pipelineruns.Manager
	start   []pipelineruns.StartInput
	control []pipelineruns.ControlInput
}

func (c *capturingRuns) Start(_ context.Context, in pipelineruns.StartInput) (pipelineruns.RunView, error) {
	c.start = append(c.start, in)
	return pipelineruns.RunView{}, nil
}

func (c *capturingRuns) Control(_ context.Context, in pipelineruns.ControlInput) (pipelineruns.ControlResult, error) {
	c.control = append(c.control, in)
	return pipelineruns.ControlResult{}, nil
}

type capturingTrust struct {
	pipelinessvc.Manager
	got []pipelinessvc.SetCommandTrustInput
}

func (c *capturingTrust) SetCommandTrust(_ context.Context, _ domain.ProjectID, in pipelinessvc.SetCommandTrustInput) (pipelinessvc.CommandTrust, error) {
	c.got = append(c.got, in)
	return pipelinessvc.CommandTrust{}, nil
}

type callerSessions struct{ terminated map[domain.SessionID]bool }

func (s callerSessions) Get(_ context.Context, id domain.SessionID) (domain.Session, error) {
	if id != "orch-1" {
		return domain.Session{}, errors.New("not found")
	}
	var sess domain.Session
	sess.IsTerminated = s.terminated[id]
	sess.Metadata.BrowserCapabilityVerifier = "verifier-orch-1"
	return sess, nil
}

type callerCapabilities struct{}

func (callerCapabilities) Valid(id domain.SessionID, token, verifier string) bool {
	return id == "orch-1" && token == "secret-orch-1" && verifier == "verifier-orch-1"
}

func callerRouter(runs *capturingRuns, trust *capturingTrust, terminated ...domain.SessionID) http.Handler {
	callers := controllers.PipelineCallerAuthority{Sessions: callerSessions{terminated: map[domain.SessionID]bool{}}, Capabilities: callerCapabilities{}}
	for _, id := range terminated {
		callers.Sessions.(callerSessions).terminated[id] = true
	}
	r := chi.NewRouter()
	(&controllers.PipelineRunsController{Mgr: runs, Callers: callers}).Register(r)
	(&controllers.PipelinesController{Mgr: trust, Callers: callers}).Register(r)
	return r
}

func callerDo(t *testing.T, h http.Handler, method, path, body string, headers map[string]string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out, _ := io.ReadAll(rec.Result().Body)
	return rec.Code, string(out)
}

var orchestratorHeaders = map[string]string{"X-AO-Caller-Session": "orch-1", "X-AO-Browser-Capability": "secret-orch-1"}

// A request the daemon can attribute to an AO session is bound to "orchestrator"
// whatever the body claims, so a session cannot pose as the user.
func TestPipelineRequesterIsBoundServerSideForSessionOriginatedRequests(t *testing.T) {
	runs, trust := &capturingRuns{}, &capturingTrust{}
	h := callerRouter(runs, trust)

	if code, body := callerDo(t, h, "POST", "/sessions/w-1/pipeline", `{"workflowId":"w","requestedBy":"user"}`, orchestratorHeaders); code != http.StatusCreated {
		t.Fatalf("start: %d %s", code, body)
	}
	if code, body := callerDo(t, h, "POST", "/sessions/w-1/pipeline/control", `{"runId":"r","action":"resume","requestedBy":"user"}`, orchestratorHeaders); code != http.StatusOK {
		t.Fatalf("control: %d %s", code, body)
	}
	if code, body := callerDo(t, h, "PUT", "/projects/p/pipelines/command-trust", `{"trusted":true,"requestedBy":"user"}`, orchestratorHeaders); code != http.StatusOK {
		t.Fatalf("trust: %d %s", code, body)
	}
	if runs.start[0].RequestedBy != "orchestrator" || runs.control[0].RequestedBy != "orchestrator" || trust.got[0].RequestedBy != "orchestrator" {
		t.Fatalf("a session-originated request must be bound to orchestrator: start=%+v control=%+v trust=%+v", runs.start, runs.control, trust.got)
	}
}

// Without a capability the daemon cannot tell, so the declared requester stands:
// the guard is cooperative on the unauthenticated loopback listener.
func TestPipelineRequesterIsCooperativeWhenTheDaemonCannotAttributeTheCaller(t *testing.T) {
	runs, trust := &capturingRuns{}, &capturingTrust{}
	h := callerRouter(runs, trust)
	if code, _ := callerDo(t, h, "POST", "/sessions/w-1/pipeline", `{"workflowId":"w","requestedBy":"user"}`, nil); code != http.StatusCreated {
		t.Fatalf("start: %d", code)
	}
	if code, _ := callerDo(t, h, "PUT", "/projects/p/pipelines/command-trust", `{"trusted":true,"requestedBy":"user"}`, nil); code != http.StatusOK {
		t.Fatalf("trust: %d", code)
	}
	if runs.start[0].RequestedBy != "user" || trust.got[0].RequestedBy != "user" {
		t.Fatalf("an unattributed request keeps its declared requester: %+v %+v", runs.start, trust.got)
	}
}

// A capability the daemon cannot verify fails closed instead of being read as anonymous.
func TestPipelineRequestsWithAnInvalidSessionCapabilityAreRefused(t *testing.T) {
	runs, trust := &capturingRuns{}, &capturingTrust{}
	for name, headers := range map[string]map[string]string{
		"wrong secret":     {"X-AO-Caller-Session": "orch-1", "X-AO-Browser-Capability": "nope"},
		"unknown session":  {"X-AO-Caller-Session": "ghost", "X-AO-Browser-Capability": "secret-orch-1"},
		"capability alone": {"X-AO-Browser-Capability": "secret-orch-1"},
	} {
		t.Run(name, func(t *testing.T) {
			h := callerRouter(runs, trust)
			body, code := func() (string, int) {
				c, b := callerDo(t, h, "PUT", "/projects/p/pipelines/command-trust", `{"trusted":true,"requestedBy":"user"}`, headers)
				return b, c
			}()
			assertErrorCode(t, []byte(body), code, http.StatusForbidden, "PIPELINE_CALLER_INVALID")
		})
	}
	h := callerRouter(runs, trust, "orch-1")
	body, code := func() (string, int) {
		c, b := callerDo(t, h, "POST", "/sessions/w-1/pipeline", `{"workflowId":"w","requestedBy":"user"}`, orchestratorHeaders)
		return b, c
	}()
	assertErrorCode(t, []byte(body), code, http.StatusForbidden, "PIPELINE_CALLER_INVALID")
	if len(runs.start) != 0 || len(trust.got) != 0 {
		t.Fatalf("a refused caller reaches no service: %+v %+v", runs.start, trust.got)
	}
}
