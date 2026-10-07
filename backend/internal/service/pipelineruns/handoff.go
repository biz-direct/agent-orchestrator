package pipelineruns

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/pipeline"
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
			if err := s.StartPendingIntents(ctx); err != nil && ctx.Err() == nil {
				s.logger.Error("pipeline: starting selected pipelines failed", "err", err)
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
	if activeReviewAttempt(run, attempts) != nil {
		return s.driveReviewAttempt(ctx, run.ID)
	}
	var pending, validating *domain.PipelineStageAttempt
	byID := map[string]domain.PipelineStageAttempt{}
	for i := range attempts {
		byID[attempts[i].ID] = attempts[i]
		switch attempts[i].State {
		case domain.PipelineAttemptHandoff:
			pending = &attempts[i]
		case domain.PipelineAttemptValidating:
			validating = &attempts[i]
		}
	}
	if validating != nil && validating.StageID == run.CurrentStageID {
		snap, err := parseSnapshot(run.Snapshot)
		if err != nil {
			return err
		}
		vctx, cancel := context.WithCancel(ctx)
		s.validations.Store(run.ID, cancel)
		defer func() {
			s.validations.Delete(run.ID)
			cancel()
		}()
		return s.driveValidation(vctx, run, *validating, snap)
	}
	if pending == nil || pending.StageID != run.CurrentStageID {
		return nil
	}
	// A repair hands off from the failed attempt that sent the task back; every
	// other handoff is from an accepted predecessor.
	// A resumed attempt continues the interrupted one it names: the same
	// executor, the same conversation, no replay.
	isRetry := pending.RetryOfAttemptID != ""
	isRepair := pending.RepairSourceAttemptID != "" && !isRetry
	predID, wantStates := pending.PredecessorAttemptID, []domain.PipelineAttemptState{domain.PipelineAttemptAccepted}
	switch {
	case isRetry:
		predID, wantStates = pending.RetryOfAttemptID, []domain.PipelineAttemptState{domain.PipelineAttemptInterrupted, domain.PipelineAttemptFailed, domain.PipelineAttemptCancelled}
	case isRepair:
		predID, wantStates = pending.RepairSourceAttemptID, []domain.PipelineAttemptState{domain.PipelineAttemptFailed}
	}
	pred, ok := byID[predID]
	if !ok || !slices.Contains(wantStates, pred.State) {
		return s.pauseHandoff(ctx, run, pending, PauseHandoffUncertain, "The handoff has no settled predecessor to hand off from")
	}
	// The brief a successor reads names the accepted work it builds on, which
	// for a retry is the original predecessor, not the interrupted attempt.
	promptPred := pred
	if isRetry {
		if orig, found := byID[pending.PredecessorAttemptID]; found {
			promptPred = orig
		}
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
	if stage.Kind == pipeline.StageReview && s.reviews == nil {
		return s.pauseHandoff(ctx, run, pending, PauseStageUnsupported, "This build cannot drive AO's built-in review")
	}
	if stage.Kind == pipeline.StageSpecialist {
		if err := s.executor.PreflightStage(ctx, domain.AgentHarness(stage.Harness)); err != nil {
			return s.pauseHandoff(ctx, run, pending, PauseStageUnsupported, fmt.Sprintf("Stage %q cannot run as a Chat specialist: %v", stage.ID, err))
		}
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
	// A Review predecessor has no executor to fence: the writer was stopped when
	// Review began and stays stopped until the repair resumes it below.
	if pred.StageKind != string(pipeline.StageReview) && pred.ExecutorSessionID != "" {
		if err := s.executor.RelinquishExecutor(ctx, pred.ExecutorSessionID); err != nil {
			reason, detail := PauseHandoffUncertain, fmt.Sprintf("Could not prove stage %q stopped executing: %v", pred.StageID, err)
			if errors.Is(err, ports.ErrPipelineExecutionUncertain) {
				detail = fmt.Sprintf("Stage %q may still be executing (%v); the handoff is paused instead of risking two writers", pred.StageID, err)
			}
			return s.pauseHandoff(ctx, run, pending, reason, detail)
		}
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
	case isRetry && stage.Kind != pipeline.StageReview:
		// The same executor continues its own work: uncommitted edits are its own
		// and the head may have moved on, but history must still descend from the
		// stage's input revision.
		if state.Head != pending.InputCommit {
			descends, derr := s.git.IsAncestor(ctx, owner.Metadata.WorkspacePath, pending.InputCommit, state.Head)
			if derr != nil || !descends {
				return s.pauseHandoff(ctx, run, pending, PauseUnexpectedChanges, fmt.Sprintf("HEAD %s does not descend from stage %q's input revision %s; the stage cannot continue safely", shortCommit(state.Head), stage.ID, shortCommit(pending.InputCommit)))
			}
		}
	case isRetry:
		if state.TrackedTotal > 0 || state.Head != pending.InputCommit {
			return s.pauseHandoff(ctx, run, pending, PauseUnexpectedChanges, fmt.Sprintf("The workspace no longer matches the checkpoint %s the review resumes at", shortCommit(pending.InputCommit)))
		}
	case state.DirtyTotal > 0:
		return s.pauseHandoff(ctx, run, pending, PauseUnexpectedChanges, fmt.Sprintf("The workspace has %d uncommitted path(s) after stage %q was accepted; they are preserved, not reset", state.DirtyTotal, pred.StageID))
	case state.Head != pred.OutputCommit:
		return s.pauseHandoff(ctx, run, pending, PauseUnexpectedChanges, fmt.Sprintf("HEAD moved to %s after stage %q was accepted at %s", shortCommit(state.Head), pred.StageID, shortCommit(pred.OutputCommit)))
	}

	// 3. Start, or resume, the stage's own conversation. The prompt carries the
	// task, input revision, and handoff summary; it does not pretend the
	// executor inherited anyone else's reasoning. A stage that already has a
	// conversation resumes exactly that one (the original Builder after a repair,
	// the same Tester on a re-run); if it cannot be resumed safely the run asks a
	// person rather than silently starting a fresh conversation in its place.
	// Every prompt this handoff delivers carries a key that is stable for the
	// attempt, so redelivery after a crash cannot create a second provider turn.
	ctx = ports.WithPipelineDeliveryKey(ctx, "pipeline-attempt:"+pending.ID)
	var started ports.PipelineStageStarted
	resumed := false
	switch existing := s.retainedStageSession(attempts, stage.ID); {
	case stage.Kind == pipeline.StageReview:
		// Review has no executor: it becomes active once the writer stopped and
		// the worktree is the checkpoint, and AO's reviewer takes it from there.
	case stage.Kind == pipeline.StageBuild:
		resumed = true
		prompt := s.repairPromptFor(ctx, run, *pending)
		if isRetry {
			prompt = resumeNote(run, *pending) + s.buildResumePrompt(ctx, run, *pending, snap, stage)
		}
		started, err = s.executor.ResumeExecutor(ctx, run.SessionID, prompt)
	case existing != "":
		resumed = true
		prompt := specialistPrompt(owner, run, promptPred, *pending, stage)
		if isRetry {
			prompt = resumeNote(run, *pending) + prompt
		}
		started, err = s.executor.ResumeExecutor(ctx, existing, prompt)
	default:
		started, err = s.executor.StartStage(ctx, ports.PipelineStageStart{
			RunID: run.ID, StageID: stage.ID, AttemptID: pending.ID, Owner: run.SessionID,
			Harness: domain.AgentHarness(stage.Harness), Model: stage.Model,
			SystemPrompt: specialistSystemPrompt(snap, stage, run),
			Prompt:       specialistPrompt(owner, run, promptPred, *pending, stage),
		})
	}
	if err != nil {
		reason := PauseStageStartFailed
		switch {
		case errors.Is(err, ports.ErrPipelineResumeUnsafe):
			reason = PauseRecoveryDecision
			return s.pauseHandoff(ctx, run, pending, reason, fmt.Sprintf("Stage %q's own conversation cannot be resumed safely (%v). AO will not start a fresh conversation in its place; a recovery decision is needed", stage.ID, err))
		case errors.Is(err, ports.ErrPipelineStageUnsupported):
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
		Events:   []domain.PipelineEvent{{AttemptID: pending.ID, Kind: eventHandoffActivated, Detail: eventDetail{Message: fmt.Sprintf("Stage %q is now executing in its own conversation (attempt %d)", stage.ID, pending.AttemptNo)}.marshal()}},
	})
	if errors.Is(err, domain.ErrPipelineConflict) {
		// The run was paused or cancelled while we were starting the executor.
		// A conversation we just created is stopped; one we only resumed is fenced
		// again. Either way the workspace is never touched.
		if stage.Kind == pipeline.StageReview {
			return nil
		}
		cleanup := context.WithoutCancel(ctx)
		// Another driver may have confirmed this very attempt with this very
		// session first (it adopted the same conversation). That session belongs
		// to the winner: stopping or discarding it would kill the live stage.
		if latest, lerr := s.store.ListPipelineStageAttempts(cleanup, run.ID); lerr == nil {
			for _, a := range latest {
				if a.ID == pending.ID && a.State == domain.PipelineAttemptActive && a.ExecutorSessionID == started.SessionID {
					return nil
				}
			}
		}
		var cleanErr error
		switch {
		case resumed:
			cleanErr = s.executor.RelinquishExecutor(cleanup, started.SessionID)
		default:
			// A conversation created for this attempt never ran. Discard it
			// (end the row and release the attempt) so a later resume starts a
			// fresh stage instead of finding a stopped one it must not adopt.
			if discarder, ok := s.executor.(ports.PipelineStageDiscarder); ok {
				cleanErr = discarder.DiscardStage(cleanup, started.SessionID)
			} else {
				cleanErr = s.executor.StopStage(cleanup, started.SessionID)
			}
		}
		if cleanErr != nil {
			s.logger.Error("pipeline: clean up stale stage executor failed", "run_id", run.ID, "session_id", started.SessionID, "err", cleanErr)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.store.SetPipelineAttemptInstructionDelivery(ctx, pending.ID, "delivered"); err != nil {
		s.logger.Error("pipeline: record stage prompt delivery failed", "run_id", run.ID, "err", err)
	}
	if stage.Kind == pipeline.StageReview {
		s.wake() // evaluate the Review gate now instead of at the next poll
	}
	return nil
}

// pauseHandoff pauses a run whose successor never became active.
func (s *Service) pauseHandoff(ctx context.Context, run domain.PipelineRun, pending *domain.PipelineStageAttempt, reason domain.PipelinePauseReason, detail string) error {
	// A daemon that is shutting down is not evidence about the run: leave the
	// handoff pending for the next process instead of recording a pause that
	// only reflects the cancelled context.
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	if err := s.pauseOrYield(ctx, run, pending, reason, detail); err != nil {
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
			b.WriteString("\n### Change scope\nChange only paths matching: " + strings.Join(p.AllowedPaths, ", ") + ". This is a hand-off constraint checked on your commits, not a sandbox. It is judged on the net diff from the input revision to your final commit, so a reverted out-of-scope commit would pass but would still remain in the pull request history: never commit an out-of-scope change, even temporarily.\n")
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
	b.WriteString("You did not inherit the previous stage's reasoning or conversation; rely on the repository and this brief.\n")
	if attempt.AttemptNo > 1 {
		fmt.Fprintf(&b, "This is attempt %d of this stage. Build repaired an earlier finding, so any result you reported for an earlier revision does NOT cover %s: run your checks again against this exact revision.\n", attempt.AttemptNo, shortCommit(attempt.InputCommit))
	}
	b.WriteString("\n")
	b.WriteString("When you are done, commit your work so the tree is clean, write your structured report as JSON, then run:\n")
	b.WriteString("  ao pipeline submit --outcome succeeded --summary \"<what you verified or changed>\" --report-file report.json\n")
	b.WriteString("Report shape: {\"findings\":[{\"criterion\":\"...\",\"status\":\"met|unmet|not_applicable|unverified\",\"evidence\":\"...\"}],\"commands\":[{\"command\":\"...\",\"exitCode\":0,\"summary\":\"...\"}],\"remainingIssues\":[\"...\"],\"defects\":[]}.\n")
	b.WriteString("A passing report needs at least one finding and none unmet. If you find a bug in production code, do NOT fix it: commit only your allowed changes and submit --outcome production_defect with each defect described under \"defects\". Use --outcome failed if you cannot complete the stage. `ao report` does not complete a stage.\n")
	b.WriteString("Keep report.json out of your commits (write it outside the worktree, for example in the system temp directory).\n")
	return b.String()
}

// repairPromptFor renders the revision-bound feedback the original Build
// conversation receives for a counted repair.
func (s *Service) repairPromptFor(ctx context.Context, run domain.PipelineRun, attempt domain.PipelineStageAttempt) string {
	fb := parseFeedback(attempt.FeedbackJSON)
	if fb == nil {
		fb = &Feedback{Summary: "A later stage sent this task back for repair.", Revision: attempt.InputCommit}
	}
	repair := domain.PipelineRepair{Ordinal: run.RepairsUsed, SourceStageID: fb.SourceStageID}
	if repairs, err := s.store.ListPipelineRepairs(ctx, run.ID); err == nil {
		for _, r := range repairs {
			if r.SourceAttemptID == attempt.RepairSourceAttemptID {
				repair = r
			}
		}
	}
	return repairPrompt(run, repair, *fb)
}

// resumeNote opens the prompt of a resumed stage: it says what happened, that
// the earlier turn is not repeated, and that the repository is the source of truth.
func resumeNote(run domain.PipelineRun, attempt domain.PipelineStageAttempt) string {
	return fmt.Sprintf("AO paused this pipeline run (%s) and has now resumed stage %q as attempt %d (id %s). Your earlier turn is not replayed. Look at the repository (git status and git log) to see what you already did, continue from there, and submit your result when the stage is complete.\n\n", run.ID, attempt.StageID, attempt.AttemptNo, attempt.ID)
}

// buildResumePrompt is the Build stage's own instruction for a resumed attempt:
// the repair feedback when it is a repair, otherwise the stage instruction.
func (s *Service) buildResumePrompt(ctx context.Context, run domain.PipelineRun, attempt domain.PipelineStageAttempt, snap Snapshot, stage SnapshotStage) string {
	if attempt.FeedbackJSON != "" {
		return s.repairPromptFor(ctx, run, attempt)
	}
	return stageInstruction(run, attempt, snap, stage)
}
