package pipelineruns

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// Handoff pause reasons. Each is operational: none consumes the repair budget,
// and none touches the worktree.
const (
	// PauseHandoffUncertain: the source executor could not be proven quiescent
	// (a running turn, queued work, a pending approval, background work).
	PauseHandoffUncertain domain.PipelinePauseReason = "handoff_uncertain"
	// PauseUnexpectedChanges: the worktree no longer matches the accepted
	// checkpoint once the source stopped.
	PauseUnexpectedChanges domain.PipelinePauseReason = "unexpected_changes"
	// PauseStageUnsupported: the stage's harness/mode cannot run as a Chat
	// specialist. There is no Terminal fallback.
	PauseStageUnsupported domain.PipelinePauseReason = "stage_unsupported"
	// PauseProductionDefect: a specialist reported a defect in production code
	// for Build to fix; an actionable result until repair routing runs it.
	PauseProductionDefect domain.PipelinePauseReason = "production_defect"
	// PauseStageStartFailed: the specialist's controller could not be started.
	PauseStageStartFailed domain.PipelinePauseReason = "stage_start_failed"
)

const handoffPollInterval = 5 * time.Second

// ownerOf maps a session id to the worker that owns the task: an attached
// specialist maps to its owner, every other session to itself.
func (s *Service) ownerOf(ctx context.Context, id domain.SessionID) (domain.SessionID, error) {
	owner, err := s.store.GetSessionAttachedTo(ctx, id)
	if err != nil {
		return "", apierr.Internal("SESSION_LOAD_FAILED", "Failed to load session")
	}
	if owner != "" {
		return owner, nil
	}
	return id, nil
}

// wake asks the background driver to look for pending handoffs now.
func (s *Service) wake() {
	select {
	case s.wakeCh <- struct{}{}:
	default:
	}
}

// Run drives pending handoffs until ctx ends: on every wake and on a slow tick,
// so a handoff interrupted by a daemon restart is picked up again.
func (s *Service) Run(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(handoffPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.wakeCh:
			case <-ticker.C:
			}
			if err := s.DriveHandoffs(ctx); err != nil {
				s.logger.Error("pipeline: handoff pass failed", "err", err)
			}
		}
	}()
	return done
}

// DriveHandoffs advances every running run that has a successor waiting in
// `handoff`. It is safe to call concurrently: at most one driver runs per run.
func (s *Service) DriveHandoffs(ctx context.Context) error {
	runs, err := s.store.ListUnfinishedPipelineRuns(ctx)
	if err != nil {
		return err
	}
	var firstErr error
	for _, run := range runs {
		if run.State != domain.PipelineRunRunning {
			continue
		}
		if err := s.driveRun(ctx, run.ID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// driveRun performs one handoff: prove the predecessor stopped, verify the
// worktree is exactly the accepted checkpoint, start the successor's executor,
// and only then mark the successor active. Any failure to prove a step pauses
// the run with the successor still in `handoff`; nothing is guessed.
func (s *Service) driveRun(ctx context.Context, runID string) error {
	if _, busy := s.driving.LoadOrStore(runID, struct{}{}); busy {
		return nil
	}
	defer s.driving.Delete(runID)

	run, ok, err := s.store.GetPipelineRun(ctx, runID)
	if err != nil || !ok || run.State != domain.PipelineRunRunning {
		return err
	}
	attempts, err := s.store.ListPipelineStageAttempts(ctx, runID)
	if err != nil {
		return err
	}
	var pending *domain.PipelineStageAttempt
	byID := map[string]domain.PipelineStageAttempt{}
	for i := range attempts {
		byID[attempts[i].ID] = attempts[i]
		if attempts[i].State == domain.PipelineAttemptHandoff {
			pending = &attempts[i]
		}
	}
	if pending == nil || pending.StageID != run.CurrentStageID {
		return nil
	}
	pred, ok := byID[pending.PredecessorAttemptID]
	if !ok || pred.State != domain.PipelineAttemptAccepted {
		return s.pauseHandoff(ctx, run, pending, PauseHandoffUncertain, "The handoff has no accepted predecessor to hand off from")
	}
	snap, err := parseSnapshot(run.Snapshot)
	if err != nil {
		return err
	}
	stage, _, found := snap.stage(pending.StageID)
	if !found {
		return s.pauseHandoff(ctx, run, pending, PauseStageUnsupported, fmt.Sprintf("Stage %q is not in the run's snapshot", pending.StageID))
	}
	if s.executor == nil {
		return s.pauseHandoff(ctx, run, pending, PauseStageUnsupported, "This build has no executor for attached specialist stages")
	}
	if err := s.executor.PreflightStage(ctx, domain.AgentHarness(stage.Harness)); err != nil {
		return s.pauseHandoff(ctx, run, pending, PauseStageUnsupported, fmt.Sprintf("Stage %q cannot run as a Chat specialist: %v", stage.ID, err))
	}

	owner, ok, err := s.store.GetSession(ctx, run.SessionID)
	if err != nil {
		return err
	}
	if !ok || owner.IsTerminated {
		return s.pauseHandoff(ctx, run, pending, domain.PipelinePauseSessionTerminated, "The task ended before the handoff completed")
	}

	// 1. The predecessor must have relinquished execution. An idle-looking
	// executor is not proof: the executor itself fences intake and verifies no
	// turn, queue, approval, or background work remains.
	if err := s.executor.RelinquishExecutor(ctx, pred.ExecutorSessionID); err != nil {
		reason, detail := PauseHandoffUncertain, fmt.Sprintf("Could not prove stage %q stopped executing: %v", pred.StageID, err)
		if errors.Is(err, ports.ErrPipelineExecutionUncertain) {
			detail = fmt.Sprintf("Stage %q may still be executing (%v); the handoff is paused instead of risking two writers", pred.StageID, err)
		}
		return s.pauseHandoff(ctx, run, pending, reason, detail)
	}

	// 2. With the writer stopped, the worktree must still be exactly the
	// accepted checkpoint: same branch, clean, HEAD equal to the output commit.
	state, err := s.git.Inspect(ctx, owner.Metadata.WorkspacePath)
	if err != nil {
		return s.pauseHandoff(ctx, run, pending, PauseUnexpectedChanges, fmt.Sprintf("The workspace could not be inspected after the handoff fence: %v", err))
	}
	switch {
	case state.Branch != run.ExpectedBranch:
		return s.pauseHandoff(ctx, run, pending, PauseUnexpectedChanges, fmt.Sprintf("The workspace is on %q, expected %q", branchLabel(state.Branch), run.ExpectedBranch))
	case state.DirtyTotal > 0:
		return s.pauseHandoff(ctx, run, pending, PauseUnexpectedChanges, fmt.Sprintf("The workspace has %d uncommitted path(s) after stage %q was accepted; they are preserved, not reset", state.DirtyTotal, pred.StageID))
	case state.Head != pred.OutputCommit:
		return s.pauseHandoff(ctx, run, pending, PauseUnexpectedChanges, fmt.Sprintf("HEAD moved to %s after stage %q was accepted at %s", shortCommit(state.Head), pred.StageID, shortCommit(pred.OutputCommit)))
	}

	// 3. Start (or resume) the stage's own conversation. The prompt carries the
	// task, input revision, and handoff summary; it does not pretend the
	// specialist inherited the previous executor's reasoning.
	existing := s.retainedStageSession(attempts, stage.ID)
	started, err := s.executor.StartStage(ctx, ports.PipelineStageStart{
		RunID: run.ID, StageID: stage.ID, AttemptID: pending.ID, Owner: run.SessionID, ExistingSessionID: existing,
		Harness: domain.AgentHarness(stage.Harness), Model: stage.Model,
		SystemPrompt: specialistSystemPrompt(snap, stage, run),
		Prompt:       specialistPrompt(owner, run, pred, *pending, stage),
	})
	if err != nil {
		reason := PauseStageStartFailed
		if errors.Is(err, ports.ErrPipelineStageUnsupported) {
			reason = PauseStageUnsupported
		}
		return s.pauseHandoff(ctx, run, pending, reason, fmt.Sprintf("Stage %q could not be started: %v", stage.ID, err))
	}

	// 4. Confirm: the successor becomes the single active executor.
	now := s.clock()
	_, err = s.store.CommitPipelineTransition(ctx, domain.PipelineTransition{
		RunID: run.ID, ExpectedRevision: run.Revision, At: now,
		Run:      &domain.PipelineRunUpdate{State: domain.PipelineRunRunning, CurrentStageID: run.CurrentStageID},
		Activate: &domain.PipelineAttemptActivation{ID: pending.ID, ExecutorSessionID: started.SessionID, ControllerGeneration: started.ControllerGeneration, At: now},
		Events:   []domain.PipelineEvent{{AttemptID: pending.ID, Kind: eventHandoffActivated, Detail: eventDetail{Message: fmt.Sprintf("Stage %q is now executing in its own conversation", stage.ID)}.marshal()}},
	})
	if errors.Is(err, domain.ErrPipelineConflict) {
		// The run was paused or cancelled while we were starting the executor.
		// Stop what we started; the workspace is never touched.
		if stopErr := s.executor.StopStage(context.WithoutCancel(ctx), started.SessionID); stopErr != nil {
			s.logger.Error("pipeline: stop stale stage executor failed", "run_id", run.ID, "session_id", started.SessionID, "err", stopErr)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.store.SetPipelineAttemptInstructionDelivery(ctx, pending.ID, "delivered"); err != nil {
		s.logger.Error("pipeline: record stage prompt delivery failed", "run_id", run.ID, "err", err)
	}
	return nil
}

// pauseHandoff pauses a run whose successor never became active.
func (s *Service) pauseHandoff(ctx context.Context, run domain.PipelineRun, pending *domain.PipelineStageAttempt, reason domain.PipelinePauseReason, detail string) error {
	if err := s.pause(ctx, run, pending, reason, detail); err != nil {
		return err
	}
	return nil
}

// retainedStageSession returns the attached session an earlier attempt of the
// same stage used, so a retried stage resumes its own conversation.
func (s *Service) retainedStageSession(attempts []domain.PipelineStageAttempt, stageID string) domain.SessionID {
	var id domain.SessionID
	for _, a := range attempts {
		if a.StageID == stageID && a.ExecutorSessionID != "" {
			id = a.ExecutorSessionID
		}
	}
	return id
}

func specialistSystemPrompt(snap Snapshot, stage SnapshotStage, run domain.PipelineRun) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## AO Pipeline Specialist\n\nYou are the %q specialist for stage %q of workflow %q.\n", stage.Profile, stage.ID, snap.Workflow.ID)
	fmt.Fprintf(&b, "You share the task's git worktree and branch (%s) with the worker that owns the task. Work only in the current worktree: do not create or switch branches, do not push, do not open or claim a pull request, and never delete or reset the worktree.\n", run.ExpectedBranch)
	b.WriteString("Only one stage executes at a time; you are the active stage until you submit your result.\n")
	for _, p := range snap.Profiles {
		if p.ID != stage.Profile {
			continue
		}
		b.WriteString("\n### Role instructions (mandatory)\n")
		b.WriteString(strings.TrimSpace(p.Instructions))
		b.WriteString("\n")
		if len(p.AllowedPaths) > 0 {
			b.WriteString("\n### Change scope\nChange only paths matching: " + strings.Join(p.AllowedPaths, ", ") + ". This is a hand-off constraint checked on your commits, not a sandbox.\n")
		}
	}
	if snap.Workflow.Instructions != "" {
		b.WriteString("\n### Workflow instructions (mandatory)\n")
		b.WriteString(strings.TrimSpace(snap.Workflow.Instructions))
		b.WriteString("\n")
	}
	return b.String()
}

func specialistPrompt(owner domain.SessionRecord, run domain.PipelineRun, pred, attempt domain.PipelineStageAttempt, stage SnapshotStage) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are starting stage %q (attempt %d, id %s) of this task's pipeline.\n\n", stage.ID, attempt.AttemptNo, attempt.ID)
	if task := strings.TrimSpace(owner.Metadata.Prompt); task != "" {
		fmt.Fprintf(&b, "Task given to the Build worker (acceptance criteria are whatever it states):\n%s\n\n", task)
	}
	fmt.Fprintf(&b, "Input revision: %s on branch %s. Start from this committed revision.\n", attempt.InputCommit, run.ExpectedBranch)
	fmt.Fprintf(&b, "Handoff from stage %q: ", pred.StageID)
	if pred.Summary != "" {
		fmt.Fprintf(&b, "%s\n", pred.Summary)
	} else {
		b.WriteString("(no summary was provided)\n")
	}
	b.WriteString("You did not inherit the previous stage's reasoning or conversation; rely on the repository and this brief.\n\n")
	b.WriteString("When you are done, commit your work so the tree is clean, write your structured report as JSON, then run:\n")
	b.WriteString("  ao pipeline submit --outcome succeeded --summary \"<what you verified or changed>\" --report-file report.json\n")
	b.WriteString("Report shape: {\"findings\":[{\"criterion\":\"...\",\"status\":\"met|unmet|not_applicable|unverified\",\"evidence\":\"...\"}],\"commands\":[{\"command\":\"...\",\"exitCode\":0,\"summary\":\"...\"}],\"remainingIssues\":[\"...\"],\"defects\":[]}.\n")
	b.WriteString("A passing report needs at least one finding and none unmet. If you find a bug in production code, do NOT fix it: commit only your allowed changes and submit --outcome production_defect with each defect described under \"defects\". Use --outcome failed if you cannot complete the stage. `ao report` does not complete a stage.\n")
	b.WriteString("Keep report.json out of your commits (write it outside the worktree, for example in the system temp directory).\n")
	return b.String()
}
