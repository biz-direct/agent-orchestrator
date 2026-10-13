package pipelineruns_test

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/pipelineruns"
)

type roleGateStore struct {
	attached map[domain.SessionID]domain.SessionID
	roles    map[domain.SessionID]string
}

func (s roleGateStore) GetSessionAttachedTo(_ context.Context, id domain.SessionID) (domain.SessionID, error) {
	return s.attached[id], nil
}
func (s roleGateStore) GetSessionCrewRole(_ context.Context, id domain.SessionID) (string, error) {
	return s.roles[id], nil
}
func (roleGateStore) GetActivePipelineRunBySession(context.Context, domain.SessionID) (domain.PipelineRun, bool, error) {
	return domain.PipelineRun{}, false, nil
}
func (roleGateStore) ListPipelineStageAttempts(context.Context, string) ([]domain.PipelineStageAttempt, error) {
	return nil, nil
}

// A crew role session is conversed with by the human at any time; an attached
// pipeline stage that is not executing is still refused.
func TestGateAdmitsCrewRoleSessionsButStillRefusesIdleStages(t *testing.T) {
	gate := pipelineruns.NewStoreGate(roleGateStore{
		attached: map[domain.SessionID]domain.SessionID{"role": "w", "stage": "w"},
		roles:    map[domain.SessionID]string{"role": "builder"},
	}, nil)
	ctx := context.Background()
	if ok, reason := gate.AdmitSessionExecution(ctx, "role"); !ok {
		t.Fatalf("a role session must accept human messages, approvals, and restores: %s", reason)
	}
	if ok, _ := gate.AdmitSessionExecution(ctx, "stage"); ok {
		t.Fatal("an attached pipeline stage that is not executing stays refused")
	}
	if ok, _ := gate.AdmitSessionExecution(ctx, "w"); !ok {
		t.Fatal("an ordinary worker without a pipeline is admitted")
	}
}
