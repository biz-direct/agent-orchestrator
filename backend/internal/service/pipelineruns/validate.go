package pipelineruns

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/pipeline"
)

// Validation pause reasons. Operational ones never consume the repair budget
// and never count as a verdict on the code.
const (
	// PauseCommandsNotAuthorized: the repository declares validation commands
	// but the user has not authorized AO to run repository-controlled commands.
	PauseCommandsNotAuthorized domain.PipelinePauseReason = "commands_not_authorized"
	// PauseValidationFailed: a mandatory check genuinely failed against the
	// committed checkpoint. Implementation repair belongs to Build.
	PauseValidationFailed domain.PipelinePauseReason = "validation_failed"
	// PauseValidationOperational: a check could not give a verdict (launch,
	// setup, credentials, timeout, cancellation, or an unknown failure).
	PauseValidationOperational domain.PipelinePauseReason = "validation_operational"
	// PauseValidationInterrupted: commands were running when AO stopped. Their
	// outcome is unknown and is never inferred or blindly retried.
	PauseValidationInterrupted domain.PipelinePauseReason = "validation_interrupted"
	// PauseValidationMutated: running the checks changed tracked files, HEAD, or
	// the branch. The changes are preserved and the stage cannot advance.
	PauseValidationMutated domain.PipelinePauseReason = "validation_mutated_workspace"

	eventValidationStarted  = "validation_started"
	eventValidationFinished = "validation_finished"
)

// profileCommands returns the snapshotted validation commands of a stage.
func profileCommands(snap Snapshot, stageID string) []pipeline.ValidationCommand {
	stage, _, ok := snap.stage(stageID)
	if !ok {
		return nil
	}
	for _, p := range snap.Profiles {
		if p.ID == stage.Profile {
			return p.Validation
		}
	}
	return nil
}

// needsValidation reports whether a specialist's passing result must be backed
// by AO's own execution of the stage's validation commands before it advances.
// A defect or failure report is already a non-advancing outcome.
func needsValidation(snap Snapshot, attempt domain.PipelineStageAttempt, in SubmitInput) bool {
	return attempt.StageKind == string(pipeline.StageSpecialist) && in.Outcome == OutcomeSucceeded && len(profileCommands(snap, attempt.StageID)) > 0
}

// beginValidation parks a verified specialist result in `validating`: the agent
// has claimed success and the daemon has checked the commit, but the stage may
// not advance until the snapshotted checks pass. The executor is relinquished
// and the worktree reserved for AO's runner by the validation driver.
func (s *Service) beginValidation(ctx context.Context, run domain.PipelineRun, attempt domain.PipelineStageAttempt, in SubmitInput, head string, noChange bool) (SubmitResult, error) {
	now := s.clock()
	_, err := s.store.CommitPipelineTransition(ctx, domain.PipelineTransition{
		RunID: run.ID, ExpectedRevision: run.Revision, At: now,
		Run: &domain.PipelineRunUpdate{State: domain.PipelineRunRunning, CurrentStageID: run.CurrentStageID},
		Validate: &domain.PipelineAttemptValidation{
			ID: attempt.ID, OutputCommit: head, NoChange: noChange, Outcome: OutcomeSucceeded, Summary: in.Summary,
			ResultKey: in.IdempotencyKey, ResultJSON: reportJSON(in.Report),
		},
		Events: []domain.PipelineEvent{{AttemptID: attempt.ID, Kind: eventValidationStarted, Detail: eventDetail{Message: fmt.Sprintf("Stage %q reported success at %s; AO will now run its validation checks itself before advancing", attempt.StageID, shortCommit(head))}.marshal()}},
	})
	if err == nil {
		s.wake()
	}
	return s.finishSubmit(ctx, run, attempt, in, err, false)
}

// classify turns a command outcome into a durable status and explanation. Only
// a clean exit is a pass and only an ordinary non-zero exit is a genuine
// failure; everything that stops a command from giving a verdict is
// operational and pauses instead of being read as a code defect.
func classify(o CommandOutcome) (domain.PipelineCommandStatus, string) {
	switch {
	case o.LaunchErr != nil:
		return domain.PipelineCommandOperational, fmt.Sprintf("the command could not be started: %v", o.LaunchErr)
	case o.TimedOut:
		return domain.PipelineCommandTimeout, "the command exceeded its timeout and its process group was killed"
	case o.Cancelled:
		return domain.PipelineCommandCancelled, "the command was cancelled and its process group was killed"
	case o.ExitCode == 0:
		return domain.PipelineCommandPassed, ""
	case o.ExitCode == 126:
		return domain.PipelineCommandOperational, "the command was found but is not executable (permission problem)"
	case o.ExitCode == 127:
		return domain.PipelineCommandOperational, "the command or its interpreter was not found (setup problem)"
	case o.ExitCode < 0:
		return domain.PipelineCommandOperational, "the command ended without an exit status"
	default:
		return domain.PipelineCommandFailed, fmt.Sprintf("the command exited with status %d", o.ExitCode)
	}
}

// driveValidation runs the stage's snapshotted validation commands against the
// committed checkpoint and decides the stage. It is called with the run's
// driver slot held, so no other validator or handoff overlaps it.
func (s *Service) driveValidation(ctx context.Context, run domain.PipelineRun, attempt domain.PipelineStageAttempt, snap Snapshot) error {
	commands := profileCommands(snap, attempt.StageID)
	owner, ok, err := s.store.GetSession(ctx, run.SessionID)
	if err != nil {
		return err
	}
	if !ok || owner.IsTerminated {
		return s.pauseOrYield(ctx, run, &attempt, domain.PipelinePauseSessionTerminated, "The task ended before validation finished")
	}

	// Repository-controlled commands only run with the user's explicit
	// authorization, checked now so revoking it takes effect immediately.
	project, ok, err := s.store.GetProject(ctx, string(run.ProjectID))
	if err != nil {
		return err
	}
	if !ok || !project.Config.TrustPipelineCommands {
		return s.pauseOrYield(ctx, run, &attempt, PauseCommandsNotAuthorized, fmt.Sprintf("Stage %q declares %d validation command(s) from repository files. AO runs repository-controlled commands only after you authorize them in the project's Pipeline settings (or `ao pipeline trust`).", attempt.StageID, len(commands)))
	}

	// Reserve the worktree: the executor must be proven stopped before AO's own
	// processes write to it.
	if s.executor != nil && attempt.ExecutorSessionID != "" {
		if err := s.executor.RelinquishExecutor(ctx, attempt.ExecutorSessionID); err != nil {
			return s.pauseOrYield(ctx, run, &attempt, PauseHandoffUncertain, fmt.Sprintf("Could not prove stage %q stopped executing before validation: %v", attempt.StageID, err))
		}
	}
	workspace := owner.Metadata.WorkspacePath
	if reason := s.workspaceDrift(ctx, workspace, run, attempt.OutputCommit, true); reason != "" {
		return s.pauseOrYield(ctx, run, &attempt, PauseUnexpectedChanges, reason)
	}

	round := s.nextRound(ctx, attempt.ID)
	env := commandEnv()
	var results []domain.PipelineCommandResult
	stop := false
	for i, c := range commands {
		res := domain.PipelineCommandResult{
			AttemptID: attempt.ID, Round: round, Ordinal: i, CommandID: c.ID, Command: c.Command, Required: c.Required,
			Revision: attempt.OutputCommit, StartedAt: s.clock(),
		}
		if stop {
			res.Status, res.Detail = domain.PipelineCommandSkipped, "an earlier mandatory check did not pass"
			t := s.clock()
			res.FinishedAt = &t
			id, cerr := s.store.CreatePipelineCommandResult(ctx, res)
			if cerr != nil {
				return cerr
			}
			if ferr := s.store.FinishPipelineCommandResult(ctx, id, res); ferr != nil {
				return ferr
			}
			results = append(results, res)
			continue
		}
		res.Status = domain.PipelineCommandRunning
		id, cerr := s.store.CreatePipelineCommandResult(ctx, res)
		if cerr != nil {
			return cerr
		}
		outcome := s.runner.Run(ctx, CommandSpec{Dir: workspace, Command: c.Command, Timeout: time.Duration(c.TimeoutSeconds) * time.Second, Env: env})
		res.Status, res.Detail = classify(outcome)
		res.ExitCode, res.Log, res.LogTruncated = outcome.ExitCode, outcome.Output, outcome.Truncated
		fin := s.clock()
		res.FinishedAt = &fin
		// Evidence is written even when the daemon is shutting down.
		if ferr := s.store.FinishPipelineCommandResult(context.WithoutCancel(ctx), id, res); ferr != nil {
			return ferr
		}
		results = append(results, res)
		if res.Status != domain.PipelineCommandPassed && (res.Required || res.Status != domain.PipelineCommandFailed) {
			stop = true
		}
	}

	// The checks may have run for a while; the worktree must still be the
	// checkpoint, with no tracked file changed and nothing committed. A cancelled
	// round skips this: its commands were killed and it will not advance anyway.
	if ctx.Err() == nil {
		if reason := s.workspaceDrift(ctx, workspace, run, attempt.OutputCommit, false); reason != "" {
			return s.pauseOrYield(context.WithoutCancel(ctx), run, &attempt, PauseValidationMutated, reason+"; the changes are preserved")
		}
	}

	var operational, requiredFailed []string
	for _, r := range results {
		switch r.Status {
		case domain.PipelineCommandOperational, domain.PipelineCommandTimeout, domain.PipelineCommandCancelled, domain.PipelineCommandUnknown:
			operational = append(operational, fmt.Sprintf("%s (%s)", r.CommandID, r.Detail))
		case domain.PipelineCommandFailed:
			if r.Required {
				requiredFailed = append(requiredFailed, fmt.Sprintf("%s (exit %d)", r.CommandID, r.ExitCode))
			}
		}
	}
	pauseCtx := context.WithoutCancel(ctx)
	switch {
	case len(operational) > 0:
		reason := PauseValidationOperational
		if ctx.Err() != nil {
			reason = PauseValidationInterrupted
		}
		return s.pauseOrYield(pauseCtx, run, &attempt, reason, "Validation could not give a verdict: "+strings.Join(operational, "; ")+". This is not a code defect and did not use the repair budget")
	case len(requiredFailed) > 0:
		now := s.clock()
		detail := "Mandatory validation failed against " + shortCommit(attempt.OutputCommit) + ": " + strings.Join(requiredFailed, ", ")
		t := domain.PipelineTransition{
			RunID: run.ID, ExpectedRevision: run.Revision, At: now,
			Attempt: &domain.PipelineAttemptFinish{ID: attempt.ID, State: domain.PipelineAttemptFailed, OutputCommit: attempt.OutputCommit, NoChange: attempt.NoChange, Outcome: "validation_failed", Summary: attempt.Summary, ResultKey: attempt.ResultKey, ResultJSON: attempt.ResultJSON, FinishedAt: now},
			Events:  []domain.PipelineEvent{{AttemptID: attempt.ID, Kind: eventValidationFinished, Detail: eventDetail{Code: "validation_failed", Message: detail}.marshal()}},
		}
		fb := validationFeedback(attempt, attempt.OutputCommit, results)
		plan, exhausted := s.planRepair(pauseCtx, run, snap, attempt, domain.PipelineRepairValidationFailed, fb, attempt.OutputCommit)
		s.applyRepairOrPause(&t, run, plan, exhausted, PauseValidationFailed, detail, fb)
		_, err := s.store.CommitPipelineTransition(pauseCtx, t)
		if errors.Is(err, domain.ErrPipelineConflict) {
			return nil
		}
		if err == nil && plan != nil {
			s.wake()
		}
		return err
	}
	// Every mandatory check passed at the committed checkpoint: the structured
	// outcome and the independent evidence now agree, so the stage may advance.
	now := s.clock()
	err = s.commitAcceptance(pauseCtx, run, attempt, snap, &domain.PipelineAttemptFinish{
		ID: attempt.ID, State: domain.PipelineAttemptAccepted, OutputCommit: attempt.OutputCommit, NoChange: attempt.NoChange,
		Outcome: OutcomeSucceeded, Summary: attempt.Summary, ResultKey: attempt.ResultKey, ResultJSON: attempt.ResultJSON, FinishedAt: now,
	})
	if errors.Is(err, domain.ErrPipelineConflict) {
		return nil
	}
	return err
}

// workspaceDrift explains how the worktree no longer matches the checkpoint, or
// returns "" when it matches. Before the checks every path must be clean; after
// them untracked build output is tolerated but tracked changes are not.
func (s *Service) workspaceDrift(ctx context.Context, workspace string, run domain.PipelineRun, head string, requireFullyClean bool) string {
	state, err := s.git.Inspect(ctx, workspace)
	switch {
	case err != nil:
		return fmt.Sprintf("The workspace could not be inspected: %v", err)
	case state.Branch != run.ExpectedBranch:
		return fmt.Sprintf("The workspace is on %q, expected %q", branchLabel(state.Branch), run.ExpectedBranch)
	case state.Head != head:
		return fmt.Sprintf("HEAD is %s but the checkpoint under validation is %s", shortCommit(state.Head), shortCommit(head))
	case requireFullyClean && state.DirtyTotal > 0:
		return fmt.Sprintf("The workspace has %d uncommitted path(s) before validation", state.DirtyTotal)
	case !requireFullyClean && state.TrackedTotal > 0:
		return fmt.Sprintf("Validation left %d tracked file(s) modified", state.TrackedTotal)
	}
	return ""
}

func (s *Service) nextRound(ctx context.Context, attemptID string) int {
	results, err := s.store.ListPipelineCommandResults(ctx, attemptID)
	if err != nil {
		return 1
	}
	n := 0
	for _, r := range results {
		if r.Round > n {
			n = r.Round
		}
	}
	return n + 1
}
