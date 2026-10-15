package controllers_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/controllers"
	crewssvc "github.com/aoagents/agent-orchestrator/backend/internal/service/crews"
)

type capturingBaton struct {
	crewssvc.Manager
	moves []crewssvc.MoveBatonInput
}

func (c *capturingBaton) MoveBaton(_ context.Context, _ domain.SessionID, in crewssvc.MoveBatonInput) (crewssvc.BatonStatus, error) {
	c.moves = append(c.moves, in)
	return crewssvc.BatonStatus{}, nil
}

// The human override is for people: a request the daemon can attribute to an AO
// session (an orchestrator or a role) is refused, and nothing moves.
func TestMovingTheBatonIsHumanOnly(t *testing.T) {
	mgr := &capturingBaton{}
	r := chi.NewRouter()
	callers := controllers.PipelineCallerAuthority{Sessions: callerSessions{terminated: map[domain.SessionID]bool{}}, Capabilities: callerCapabilities{}}
	(&controllers.CrewsController{Mgr: mgr, Callers: callers}).Register(r)
	body := `{"to":{"kind":"human"},"reason":"mine"}`

	if code, out := callerDo(t, r, "PUT", "/sessions/w-1/crew/baton", body, orchestratorHeaders); code != http.StatusForbidden {
		t.Fatalf("a session caller must be refused: %d %s", code, out)
	}
	forged := map[string]string{"X-AO-Caller-Session": "orch-1", "X-AO-Browser-Capability": "wrong"}
	if code, out := callerDo(t, r, "PUT", "/sessions/w-1/crew/baton", body, forged); code != http.StatusForbidden {
		t.Fatalf("a forged capability must be refused: %d %s", code, out)
	}
	if len(mgr.moves) != 0 {
		t.Fatalf("refused requests must not move the baton: %+v", mgr.moves)
	}
	if code, out := callerDo(t, r, "PUT", "/sessions/w-1/crew/baton", body, nil); code != http.StatusOK {
		t.Fatalf("a person moves the baton: %d %s", code, out)
	}
	if len(mgr.moves) != 1 || mgr.moves[0].To.Kind != domain.BatonHolderHuman || mgr.moves[0].Reason != "mine" {
		t.Fatalf("moves = %+v", mgr.moves)
	}
}
