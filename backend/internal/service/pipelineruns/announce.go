package pipelineruns

import (
	"context"
	"fmt"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// notifyingStore reports a run's meaningful transitions once the transaction
// that made them has committed, so every path through the service (Submit,
// drivers, controls) is covered without each remembering to announce.
type notifyingStore struct {
	Store
	svc *Service
}

// CommitPipelineTransition implements Store.
func (n *notifyingStore) CommitPipelineTransition(ctx context.Context, t domain.PipelineTransition) (domain.PipelineRun, error) {
	run, err := n.Store.CommitPipelineTransition(ctx, t)
	if err == nil && t.Run != nil {
		n.svc.announce(ctx, run)
	}
	return run, err
}

const maxAnnouncementRunes = 900

// announce sends the task's orchestrator an AO-authored report. Task-level
// completion is reported only from validated pipeline completion; a pause is
// reported only when somebody has to decide (a pause somebody just asked for
// needs no announcement). Delivery failures never affect the run.
func (s *Service) announce(ctx context.Context, run domain.PipelineRun) {
	var state domain.ReportState
	var note string
	switch {
	case run.State == domain.PipelineRunCompleted:
		state = domain.ReportDone
		note = fmt.Sprintf("Pipeline %q completed: every stage was accepted and every gate passed for the final revision. This is validated pipeline completion; it does not merge anything or imply host approvals, branch protection, or publishing were satisfied.", run.WorkflowID)
	case run.State == domain.PipelineRunPaused && run.PauseReason != PauseByUser && run.PauseReason != PauseByOrchestrator:
		state = domain.ReportNeedsInput
		note = fmt.Sprintf("Pipeline %q is paused (%s) at stage %q: %s. Inspect it with `ao pipeline status --session %s`.", run.WorkflowID, run.PauseReason, run.CurrentStageID, run.PauseDetail, run.SessionID)
		if humanOnlyPause(run.PauseReason) {
			note += " This is a decision for a person; an orchestrator cannot resume it."
		}
	default:
		return
	}
	if r := []rune(note); len(r) > maxAnnouncementRunes {
		note = string(r[:maxAnnouncementRunes]) + "…"
	}
	if err := s.reporter.Report(ports.WithPipelineBypass(context.WithoutCancel(ctx)), run.SessionID, state, note); err != nil {
		s.logger.Error("pipeline: report to the orchestrator failed", "run_id", run.ID, "err", err)
	}
}
