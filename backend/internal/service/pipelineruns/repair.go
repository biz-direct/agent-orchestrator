package pipelineruns

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Repair pause reasons.
const (
	// PauseRepairBudgetExhausted: the run's shared budget of automatic returns
	// to Build is spent. It stays paused until a human explicitly authorizes
	// more; ordinary resume never grants attempts.
	PauseRepairBudgetExhausted domain.PipelinePauseReason = "repair_budget_exhausted"
	// PauseRecoveryDecision: a conversation that must be resumed cannot be
	// resumed safely (its controller is gone), and AO will not silently start a
	// fresh one in its place.
	PauseRecoveryDecision domain.PipelinePauseReason = "recovery_decision_required"

	eventRepairRouted = "repair_routed"

	feedbackLogExcerpt = 2000
)

// Feedback is the revision-bound explanation of why a stage was sent back. It
// is durable on the repair attempt and shown to the repairer, so a retried
// delivery says exactly what the first one did.
type Feedback struct {
	Kind            string `json:"kind" enum:"production_defect,validation_failed,review_feedback"`
	SourceStageID   string `json:"sourceStageId"`
	SourceAttemptID string `json:"sourceAttemptId"`
	// Revision is the commit the finding is about; it says nothing about later ones.
	Revision        string          `json:"revision"`
	Summary         string          `json:"summary"`
	Defects         []ReportDefect  `json:"defects"`
	FailedChecks    []FeedbackCheck `json:"failedChecks"`
	RemainingIssues []string        `json:"remainingIssues"`
}

// FeedbackCheck is one failed independent check.
type FeedbackCheck struct {
	CommandID string `json:"commandId"`
	Command   string `json:"command"`
	ExitCode  int    `json:"exitCode"`
	Revision  string `json:"revision"`
	// Output is the tail of the bounded, sanitized log.
	Output string `json:"output,omitempty"`
}

func (f Feedback) marshal() string {
	data, err := json.Marshal(f)
	if err != nil {
		return ""
	}
	return string(data)
}

func parseFeedback(raw string) *Feedback {
	if raw == "" {
		return nil
	}
	var f Feedback
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		return nil
	}
	return &f
}

// defectFeedback builds feedback from a specialist's production-defect report.
func defectFeedback(src domain.PipelineStageAttempt, head string, report *StageReport) Feedback {
	fb := Feedback{Kind: string(domain.PipelineRepairProductionDefect), SourceStageID: src.StageID, SourceAttemptID: src.ID, Revision: head, Defects: []ReportDefect{}, FailedChecks: []FeedbackCheck{}, RemainingIssues: []string{}}
	if report != nil {
		fb.Defects = append(fb.Defects, report.Defects...)
		fb.RemainingIssues = append(fb.RemainingIssues, report.RemainingIssues...)
	}
	fb.Summary = fmt.Sprintf("Stage %q found %d production defect(s) at %s.", src.StageID, len(fb.Defects), shortCommit(head))
	return fb
}

// validationFeedback builds feedback from genuinely failed required checks.
func validationFeedback(src domain.PipelineStageAttempt, head string, results []domain.PipelineCommandResult) Feedback {
	fb := Feedback{Kind: string(domain.PipelineRepairValidationFailed), SourceStageID: src.StageID, SourceAttemptID: src.ID, Revision: head, Defects: []ReportDefect{}, FailedChecks: []FeedbackCheck{}, RemainingIssues: []string{}}
	for _, r := range results {
		if r.Status != domain.PipelineCommandFailed || !r.Required {
			continue
		}
		fb.FailedChecks = append(fb.FailedChecks, FeedbackCheck{CommandID: r.CommandID, Command: r.Command, ExitCode: r.ExitCode, Revision: r.Revision, Output: tailRunes(r.Log, feedbackLogExcerpt)})
	}
	fb.Summary = fmt.Sprintf("%d mandatory check(s) failed against %s after stage %q.", len(fb.FailedChecks), shortCommit(head), src.StageID)
	return fb
}

func tailRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return "…" + string(r[len(r)-n:])
}

// repairPlan is everything one counted return to Build changes, built so the
// caller can commit it in the same atomic transition that closes the failing
// attempt. Counting it there is what makes "exactly once" hold across
// duplicate feedback, restarts, and concurrent transitions.
type repairPlan struct {
	repair domain.PipelineRepair
	build  domain.PipelineStageAttempt
	update domain.PipelineRunUpdate
	event  domain.PipelineEvent
}

// planRepair decides whether a failing stage may be sent back to Build. It
// returns (nil, "") when the stage declares no repair route (the caller pauses
// as before), (nil, detail) when the shared budget is spent, and a plan
// otherwise. This is the single accounting contract every repair source uses:
// test defects, validation failures, and later review feedback all spend the
// same budget through here.
func (s *Service) planRepair(ctx context.Context, run domain.PipelineRun, snap Snapshot, src domain.PipelineStageAttempt, kind domain.PipelineRepairKind, fb Feedback, head string) (*repairPlan, string) {
	stage, _, found := snap.stage(src.StageID)
	if !found || stage.RepairTo == "" {
		return nil, ""
	}
	if run.RepairsUsed >= run.RepairBudget {
		return nil, fmt.Sprintf("Stage %q needs repair but the run has already used all %d automatic return(s) to Build. Nothing was reset; a person must decide how to continue", stage.ID, run.RepairBudget)
	}
	target, _, ok := snap.stage(stage.RepairTo)
	if !ok {
		return nil, ""
	}
	now := s.clock()
	ordinal := run.RepairsUsed + 1
	build := domain.PipelineStageAttempt{
		ID: s.newID("pstg"), RunID: run.ID, StageID: target.ID, StageKind: string(target.Kind),
		AttemptNo: s.nextAttemptNo(ctx, run.ID, target.ID), State: domain.PipelineAttemptHandoff,
		InputCommit: head, PredecessorAttemptID: src.ID, StartedAt: now,
		RepairSourceAttemptID: src.ID, ReturnStageID: stage.ID, FeedbackJSON: fb.marshal(),
	}
	remaining := run.RepairBudget - ordinal
	return &repairPlan{
		repair: domain.PipelineRepair{ID: s.newID("prep"), RunID: run.ID, Ordinal: ordinal, SourceAttemptID: src.ID, SourceStageID: stage.ID, Kind: kind, TargetStageID: target.ID, ReturnStageID: stage.ID},
		build:  build,
		update: domain.PipelineRunUpdate{State: domain.PipelineRunRunning, CurrentStageID: target.ID, RepairsDelta: 1},
		event:  domain.PipelineEvent{AttemptID: build.ID, Kind: eventRepairRouted, Detail: eventDetail{Code: string(kind), Message: fmt.Sprintf("Stage %q sent back to %q (repair %d of %d, %d remaining): %s", stage.ID, target.ID, ordinal, run.RepairBudget, remaining, fb.Summary)}.marshal()},
	}, ""
}

// repairPrompt is what the original Build conversation receives.
func repairPrompt(run domain.PipelineRun, repair domain.PipelineRepair, fb Feedback) string {
	var b strings.Builder
	remaining := run.RepairBudget - repair.Ordinal
	fmt.Fprintf(&b, "Stage %q sent this task back to you for repair %d of %d (%d remaining before the pipeline pauses for a person).\n\n", repair.SourceStageID, repair.Ordinal, run.RepairBudget, remaining)
	fmt.Fprintf(&b, "%s\nThis finding is about revision %s only.\n", fb.Summary, fb.Revision)
	for _, d := range fb.Defects {
		fmt.Fprintf(&b, "\nDefect: %s\n", d.Description)
		if len(d.Paths) > 0 {
			fmt.Fprintf(&b, "  Paths: %s\n", strings.Join(d.Paths, ", "))
		}
	}
	for _, c := range fb.FailedChecks {
		fmt.Fprintf(&b, "\nFailed check %q (%s) exited %d at %s.\n", c.CommandID, c.Command, c.ExitCode, shortCommit(c.Revision))
		if c.Output != "" {
			fmt.Fprintf(&b, "Output (tail):\n%s\n", c.Output)
		}
	}
	for _, issue := range fb.RemainingIssues {
		fmt.Fprintf(&b, "Known remaining issue: %s\n", issue)
	}
	b.WriteString("\nFix the production code, commit so the working tree is clean, and submit:\n")
	b.WriteString("  ao pipeline submit --outcome succeeded --summary \"<what you fixed>\"\n")
	fmt.Fprintf(&b, "After you submit, stage %q runs again against your new commit in its own conversation; stop working once you have submitted.\n", repair.SourceStageID)
	return b.String()
}
