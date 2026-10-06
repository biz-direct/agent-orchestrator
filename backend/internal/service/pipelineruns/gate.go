package pipelineruns

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// GateStore is what the execution gate reads.
type GateStore interface {
	GetSessionAttachedTo(ctx context.Context, id domain.SessionID) (domain.SessionID, error)
	GetActivePipelineRunBySession(ctx context.Context, id domain.SessionID) (domain.PipelineRun, bool, error)
	ListPipelineStageAttempts(ctx context.Context, runID string) ([]domain.PipelineStageAttempt, error)
}

// StoreGate implements ports.PipelineExecutionGate from durable run facts. It
// is the single answer to "may this session execute right now?": only the
// active stage's executor is admitted, so completion events, side messages,
// lifecycle nudges, restores, and approvals can never wake an inactive stage or
// create overlapping execution. A session with no pipeline is always admitted.
type StoreGate struct {
	store  GateStore
	logger *slog.Logger
}

var _ ports.PipelineExecutionGate = (*StoreGate)(nil)

// NewStoreGate returns a gate backed by store.
func NewStoreGate(store GateStore, logger *slog.Logger) *StoreGate {
	if logger == nil {
		logger = slog.Default()
	}
	return &StoreGate{store: store, logger: logger}
}

// AdmitSessionExecution implements ports.PipelineExecutionGate. It fails
// closed: an undeterminable answer refuses rather than risking a second writer.
func (g *StoreGate) AdmitSessionExecution(ctx context.Context, id domain.SessionID) (bool, string) {
	if ports.PipelineBypass(ctx) {
		return true, ""
	}
	owner, err := g.store.GetSessionAttachedTo(ctx, id)
	if err != nil {
		g.logger.Error("pipeline gate: cannot resolve session; refusing", "session_id", id, "err", err)
		return false, "pipeline execution state could not be determined"
	}
	attached := owner != ""
	ownerID := owner
	if !attached {
		ownerID = id
	}
	run, ok, err := g.store.GetActivePipelineRunBySession(ctx, ownerID)
	if err != nil {
		g.logger.Error("pipeline gate: cannot load run; refusing", "session_id", id, "err", err)
		return false, "pipeline execution state could not be determined"
	}
	if !ok {
		if attached {
			return false, "this pipeline stage is no longer executing"
		}
		return true, ""
	}
	attempts, err := g.store.ListPipelineStageAttempts(ctx, run.ID)
	if err != nil {
		g.logger.Error("pipeline gate: cannot load attempts; refusing", "session_id", id, "err", err)
		return false, "pipeline execution state could not be determined"
	}
	var current *domain.PipelineStageAttempt
	for i := range attempts {
		if attempts[i].StageID == run.CurrentStageID {
			current = &attempts[i]
		}
	}
	if current == nil {
		return !attached, "this task's pipeline has no executing stage"
	}
	switch current.State {
	case domain.PipelineAttemptActive, domain.PipelineAttemptFailed, domain.PipelineAttemptInterrupted, domain.PipelineAttemptCancelled:
		// A paused run keeps its last executor reachable so a person can unblock it.
		if current.ExecutorSessionID == id {
			return true, ""
		}
		if current.StageKind == "review" {
			return false, fmt.Sprintf("this task is in pipeline stage %q: AO's review is evaluating the current revision and nothing may change it", current.StageID)
		}
		return false, fmt.Sprintf("this task is running pipeline stage %q in a different conversation", current.StageID)
	case domain.PipelineAttemptValidating:
		return false, fmt.Sprintf("AO is running independent validation checks for stage %q; nothing may write the worktree meanwhile", current.StageID)
	case domain.PipelineAttemptHandoff:
		return false, fmt.Sprintf("this task is handing off to pipeline stage %q; no stage is executing yet", current.StageID)
	default:
		return false, "this task's pipeline has no executing stage"
	}
}
