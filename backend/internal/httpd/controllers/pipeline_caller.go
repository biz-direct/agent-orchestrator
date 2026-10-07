package controllers

import (
	"context"
	"net/http"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
)

// callerSessionHeader names the session a request claims to originate from.
// It is only meaningful together with the session's browser capability
// (X-AO-Browser-Capability), which the daemon injects into that session alone.
const callerSessionHeader = "X-AO-Caller-Session"

// PipelineCallerAuthority tells whether a pipeline request provably comes from
// inside an AO session. It reuses the per-session capability the daemon already
// issues; it is not an authentication system.
//
// The loopback API is unauthenticated, so this can only prove a request IS
// session-originated, never that it is NOT: a caller that omits both headers is
// indistinguishable from a person at a shell. Human-only pipeline operations
// are therefore enforced for every request the daemon can attribute to a
// session, and are cooperative for the rest (see docs/pipelines.md).
type PipelineCallerAuthority struct {
	Sessions interface {
		Get(ctx context.Context, id domain.SessionID) (domain.Session, error)
	}
	Capabilities SessionCapabilityValidator
}

// SessionCaller reports whether the request carries a valid session capability.
// A request that presents a capability the daemon cannot verify is refused
// rather than treated as anonymous, so a forged or stale claim fails closed.
func (a PipelineCallerAuthority) SessionCaller(r *http.Request) (bool, error) {
	token := strings.TrimSpace(r.Header.Get(browserCapabilityHeader))
	callerID := strings.TrimSpace(r.Header.Get(callerSessionHeader))
	if token == "" && callerID == "" {
		return false, nil
	}
	if a.Sessions == nil || a.Capabilities == nil {
		return false, nil
	}
	invalid := apierr.Forbidden("PIPELINE_CALLER_INVALID", "The session capability on this request is missing or invalid")
	if token == "" || callerID == "" {
		return false, invalid
	}
	sess, err := a.Sessions.Get(r.Context(), domain.SessionID(callerID))
	if err != nil || sess.IsTerminated {
		return false, invalid
	}
	if !a.Capabilities.Valid(domain.SessionID(callerID), token, sess.Metadata.BrowserCapabilityVerifier) {
		return false, invalid
	}
	return true, nil
}
