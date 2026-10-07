package pipelineruns

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Review stage pause reasons. Each is actionable: the run keeps the evidence it
// was paused on and nothing here touches the worktree.
const (
	// PauseHeadChanged: the revision under review is no longer the pull
	// request's or the workspace's head, so earlier approvals and CI results do
	// not speak for it.
	PauseHeadChanged domain.PipelinePauseReason = "head_changed"
	// PauseReviewChangesRequested: AO's reviewer requested changes on the
	// current head. Review repair routing is a later slice; until then a person
	// decides what happens next.
	PauseReviewChangesRequested domain.PipelinePauseReason = "review_changes_requested"
	// PauseReviewOperational: the reviewer failed, was cancelled, or finished
	// without a verdict. It is never retried behind the run's back.
	PauseReviewOperational domain.PipelinePauseReason = "review_operational"
	// PauseCIFailing: a CI check failed and AO cannot prove it is not required.
	PauseCIFailing domain.PipelinePauseReason = "ci_failing"
	// PausePullRequestClosed: the task's pull request was merged or closed.
	PausePullRequestClosed domain.PipelinePauseReason = "pull_request_closed"
	// PausePullRequestAmbiguous: several open pull requests claim the branch.
	PausePullRequestAmbiguous domain.PipelinePauseReason = "pull_request_ambiguous"
	// PauseReviewUnverifiable: AO could not inspect the facts it needs.
	PauseReviewUnverifiable domain.PipelinePauseReason = "review_unverifiable"
)

// Review gate codes shown while the run waits. Waiting needs no decision: the
// gate re-evaluates on its own as soon as the missing fact appears.
const (
	WaitAwaitingPullRequest = "awaiting_pull_request"
	WaitAwaitingPRHead      = "awaiting_pull_request_head"
	WaitAwaitingManual      = "awaiting_manual_review"
	WaitAwaitingAutoReview  = "awaiting_auto_review"
	WaitReviewRunning       = "review_running"
	WaitAwaitingCI          = "awaiting_ci"
	WaitAwaitingCIStatus    = "awaiting_ci_status"
	// GateReady means every completion condition holds for the current head.
	GateReady = "ready"
)

// Review gate states for API consumers.
const (
	// GateStateWaiting: the run proceeds by itself once a fact arrives.
	GateStateWaiting = "waiting"
	// GateStateReady: approval and CI both hold for the current head.
	GateStateReady = "ready"
	// GateStateBlocked: a person has to act; the run is, or is about to be, paused.
	GateStateBlocked = "blocked"
)

// CI assessments recorded as evidence.
const (
	CIPassing         = "passing"
	CINoChecks        = "no_checks"
	CINonRequiredOnly = "non_required_failing"
	CIPending         = "pending"
	CIFailing         = "failing"
	CIUnknown         = "unknown"
)

// ReviewFacts are the read-only facts the Review gate decides on. They are
// gathered by the daemon from the SCM observer's stored state and the review
// subsystem's runs; nothing in them comes from an agent's report.
type ReviewFacts struct {
	PRs    []domain.PullRequest
	Checks map[string][]domain.PullRequestCheck
	Runs   []domain.ReviewRun
}

// ReviewGateway is the pipeline's narrow view of AO's built-in review
// subsystem. Review is an adapter over it, not a second reviewer.
type ReviewGateway interface {
	// Facts returns the stored pull request, check, and review facts of a task.
	Facts(ctx context.Context, id domain.SessionID) (ReviewFacts, error)
	// TriggerAuto asks the built-in reviewer for the same automatic pass AO's
	// auto-review would start. A non-empty skip reason means the task is not
	// eligible right now (for example not idle long enough); it is not an error.
	TriggerAuto(ctx context.Context, id domain.SessionID) (skipReason string, err error)
}

type gateKind int

const (
	gateWait gateKind = iota
	gateTrigger
	gatePause
	gateComplete
)

// gateInput is everything the pure decision needs.
type gateInput struct {
	Checkpoint  string
	Branch      string
	Git         GitState
	GitErr      error
	AutoEnabled bool
	Link        *domain.PipelineReviewLink
	Facts       ReviewFacts
}

// gateDecision is the outcome of one evaluation.
type gateDecision struct {
	Kind   gateKind
	Code   string
	Detail string
	// Pause is the run pause reason for gatePause.
	Pause domain.PipelinePauseReason
	// FinishFailed closes the attempt as a failed result carrying the verdict
	// instead of interrupting it.
	FinishFailed bool
	Outcome      string

	PR     *domain.PullRequest
	Run    *domain.ReviewRun
	CI     ciAssessment
	Linked bool
}

// ciAssessment is the authoritative required-CI judgement for one head.
type ciAssessment struct {
	State  string // CI* constants
	Wait   bool
	Fail   bool
	Detail string
	// Unproven reports that the provider's merge state does not confirm that
	// every required check reported for this head, so a pass or "no checks"
	// rests on the reported checks alone.
	Unproven bool
}

// evaluateReview decides what a Review attempt should do now. It is pure so the
// driver, the API view, and the tests share one rule. Every doubt resolves to
// waiting or pausing with a reason; nothing unknown ever counts as a pass.
func evaluateReview(in gateInput) gateDecision {
	pause := func(reason domain.PipelinePauseReason, detail string) gateDecision {
		return gateDecision{Kind: gatePause, Code: string(reason), Pause: reason, Detail: detail}
	}
	wait := func(code, detail string) gateDecision {
		return gateDecision{Kind: gateWait, Code: code, Detail: detail}
	}

	// 1. The revision under review is the accepted checkpoint, and nothing may
	// have changed the workspace since the executors stopped.
	switch {
	case in.GitErr != nil:
		return pause(PauseReviewUnverifiable, fmt.Sprintf("The workspace could not be inspected: %v", in.GitErr))
	case in.Git.Branch != in.Branch:
		return pause(PauseUnexpectedChanges, fmt.Sprintf("The workspace is on %q, expected %q", branchLabel(in.Git.Branch), in.Branch))
	case in.Git.TrackedTotal > 0:
		// Untracked files cannot change the head under review; edits to tracked
		// files mean the worktree no longer matches what is being approved.
		return pause(PauseUnexpectedChanges, fmt.Sprintf("%d tracked path(s) have uncommitted changes while the review is evaluating %s; they are preserved, not reset", in.Git.TrackedTotal, shortCommit(in.Checkpoint)))
	case in.Git.Head != in.Checkpoint:
		return pause(PauseHeadChanged, fmt.Sprintf("HEAD moved to %s while the review was evaluating %s; approvals and CI results for the earlier revision do not cover it", shortCommit(in.Git.Head), shortCommit(in.Checkpoint)))
	}

	// 2. The task's own open pull request.
	var open []domain.PullRequest
	closed := false
	for _, pr := range in.Facts.PRs {
		if pr.SourceBranch != in.Branch {
			continue
		}
		if pr.Merged || pr.Closed {
			closed = true
			continue
		}
		open = append(open, pr)
	}
	switch {
	case len(open) > 1:
		return pause(PausePullRequestAmbiguous, fmt.Sprintf("%d open pull requests track branch %s; AO will not guess which one to review", len(open), in.Branch))
	case len(open) == 0 && closed:
		return pause(PausePullRequestClosed, fmt.Sprintf("The pull request for branch %s was merged or closed before the review finished", in.Branch))
	case len(open) == 0:
		return wait(WaitAwaitingPullRequest, fmt.Sprintf("No pull request is tracked for branch %s yet. Open one for revision %s; the review starts when it appears", in.Branch, shortCommit(in.Checkpoint)))
	}
	pr := open[0]
	d := gateDecision{PR: &pr}

	// 3. The pull request head is the checkpoint. Before the review is linked a
	// lagging head just waits; after, a different head is a different revision.
	if in.Link != nil && (in.Link.PRURL != pr.URL || !sameCommit(in.Link.HeadSHA, pr.HeadSHA)) {
		d2 := pause(PauseHeadChanged, fmt.Sprintf("The pull request head moved from %s to %s after the review was linked; approvals and CI results for the earlier head do not cover it", shortCommit(in.Link.HeadSHA), shortCommit(pr.HeadSHA)))
		d2.PR = &pr
		return d2
	}
	if pr.HeadSHA == "" || !sameCommit(pr.HeadSHA, in.Checkpoint) {
		d2 := wait(WaitAwaitingPRHead, fmt.Sprintf("The pull request head is %s but the pipeline is reviewing %s. Push the checkpoint to the pull request; AO will not review a different revision", shortCommit(pr.HeadSHA), shortCommit(in.Checkpoint)))
		d2.PR = &pr
		return d2
	}
	d.Linked = true

	// 4. The review pass for exactly this head.
	run := latestRunForHead(in.Facts.Runs, pr.URL, in.Checkpoint)
	d.Run = run
	if run == nil {
		if in.AutoEnabled {
			d.Kind, d.Code, d.Detail = gateTrigger, WaitAwaitingAutoReview, "Auto review is on; AO is starting the built-in review for this revision"
			return d
		}
		d.Kind, d.Code, d.Detail = gateWait, WaitAwaitingManual, "Auto review is off. Trigger the review for this revision from the task's review controls"
		return d
	}
	switch {
	case run.Status == domain.ReviewRunRunning:
		d.Kind, d.Code, d.Detail = gateWait, WaitReviewRunning, "The built-in review is running for this revision"
		return d
	case run.Status == domain.ReviewRunFailed || run.Status == domain.ReviewRunCancelled:
		return withPR(pause(PauseReviewOperational, fmt.Sprintf("The built-in review %s for revision %s. AO does not retry it behind the run's back; fix the reviewer and trigger it again", run.Status, shortCommit(in.Checkpoint))), d)
	case run.Verdict == domain.VerdictChangesRequested:
		out := withPR(pause(PauseReviewChangesRequested, fmt.Sprintf("The built-in review requested changes on revision %s. This workflow has no repair route, so resuming would re-evaluate the same revision; address the findings, then cancel this run and start a new one on the new head", shortCommit(in.Checkpoint))), d)
		out.FinishFailed, out.Outcome = true, "changes_requested"
		return out
	case run.Verdict != domain.VerdictApproved:
		return withPR(pause(PauseReviewOperational, fmt.Sprintf("The built-in review finished for revision %s without a verdict", shortCommit(in.Checkpoint))), d)
	}

	// 5. Required CI for exactly this head.
	d.CI = assessCI(pr, in.Facts.Checks[pr.URL], in.Checkpoint)
	switch {
	case d.CI.Fail:
		return withPR(pause(PauseCIFailing, d.CI.Detail), d)
	case d.CI.Wait:
		code := WaitAwaitingCIStatus
		if d.CI.State == CIPending {
			code = WaitAwaitingCI
		}
		d.Kind, d.Code, d.Detail = gateWait, code, d.CI.Detail
		return d
	}
	d.Kind, d.Code = gateComplete, GateReady
	d.Detail = fmt.Sprintf("Approved by the built-in review for %s; CI: %s", shortCommit(in.Checkpoint), d.CI.State)
	if d.CI.Unproven {
		d.Detail += " (required-check coverage unproven: " + d.CI.Detail + ")"
	}
	return d
}

func withPR(out, base gateDecision) gateDecision {
	out.PR, out.Run, out.Linked = base.PR, base.Run, base.Linked
	return out
}

// sameCommit compares full or abbreviated commit ids case-insensitively.
func sameCommit(a, b string) bool {
	a, b = strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	if a == "" || b == "" {
		return false
	}
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

// latestRunForHead returns the newest AO review pass for the pull request at
// exactly this head. A pass for any other head is stale by definition.
func latestRunForHead(runs []domain.ReviewRun, prURL, head string) *domain.ReviewRun {
	var matching []domain.ReviewRun
	for _, r := range runs {
		if r.PRURL == prURL && sameCommit(r.TargetSHA, head) {
			matching = append(matching, r)
		}
	}
	if len(matching) == 0 {
		return nil
	}
	sort.SliceStable(matching, func(i, j int) bool { return matching[i].CreatedAt.After(matching[j].CreatedAt) })
	return &matching[0]
}

// providerUnstable is GitHub's merge state for a pull request whose only
// non-passing checks are not required by branch protection.
const providerUnstable = "UNSTABLE"

// providerBlocked is GitHub's merge state when a requirement of the base
// branch's rules is unmet: a required check that has not reported for the head
// (it is absent from the check rollup, so the reported checks can all pass) or
// another rule such as a required approval.
const providerBlocked = "BLOCKED"

// requiredChecksConfirmed reports whether the provider's merge state positively
// confirms that the base branch's required checks are satisfied.
func requiredChecksConfirmed(mergeState string) bool {
	switch strings.ToUpper(strings.TrimSpace(mergeState)) {
	case "CLEAN", "HAS_HOOKS", providerUnstable:
		return true
	}
	return false
}

// assessCI decides whether required CI holds for head. AO has no per-check
// "required" flag; the authoritative signal is the provider's merge state: an
// UNSTABLE pull request is mergeable with only non-required checks failing or
// pending. A repository with no checks at all has no CI requirement, and AO does
// not invent one. Anything it cannot classify waits or pauses with a reason.
func assessCI(pr domain.PullRequest, checks []domain.PullRequestCheck, head string) ciAssessment {
	for _, c := range checks {
		if c.CommitHash != "" && !sameCommit(c.CommitHash, head) {
			return ciAssessment{State: CIUnknown, Wait: true, Detail: fmt.Sprintf("CI results are still for %s, not the reviewed revision %s; waiting for the provider to report on the current head", shortCommit(c.CommitHash), shortCommit(head))}
		}
	}
	nonRequiredOnly := strings.EqualFold(strings.TrimSpace(pr.ProviderMergeStateStatus), providerUnstable)
	blocked := strings.EqualFold(strings.TrimSpace(pr.ProviderMergeStateStatus), providerBlocked)
	unproven := !requiredChecksConfirmed(pr.ProviderMergeStateStatus)
	switch pr.CI {
	case domain.CIPassing:
		if blocked {
			// The rollup only lists checks that reported. A required check that
			// never started for this head is absent from it, so "all reported
			// checks pass" can hold while the provider still reports the pull
			// request as blocked. BLOCKED is ambiguous (it also covers unmet
			// approvals), but passing cannot be proven, so AO keeps waiting.
			return ciAssessment{State: CIUnknown, Wait: true, Unproven: true, Detail: "All reported checks pass for the current head, but the provider's merge state is BLOCKED: a required check may not have reported for this head (or another merge requirement, such as a required approval, is unmet). AO cannot prove required CI is satisfied and will not treat the partial check set as passing"}
		}
		if unproven {
			return ciAssessment{State: CIPassing, Unproven: true, Detail: "All reported checks pass for the current head; the provider's merge state does not confirm that every required check reported"}
		}
		return ciAssessment{State: CIPassing, Detail: "All reported checks pass for the current head"}
	case domain.CIFailing:
		if nonRequiredOnly {
			return ciAssessment{State: CINonRequiredOnly, Detail: "Failing checks are not required by the provider's merge rules (merge state UNSTABLE)"}
		}
		return ciAssessment{State: CIFailing, Fail: true, Detail: "A CI check is failing for the current head and AO cannot prove it is not required (the provider's merge state is not UNSTABLE)"}
	case domain.CIPending:
		if nonRequiredOnly {
			return ciAssessment{State: CINonRequiredOnly, Detail: "Pending checks are not required by the provider's merge rules (merge state UNSTABLE)"}
		}
		return ciAssessment{State: CIPending, Wait: true, Detail: "Required CI is still running for the current head"}
	}
	// Unknown: either the provider reports no checks for this repository, or AO
	// has not observed CI yet. Only a recorded observation with no checks counts
	// as "no CI to wait for".
	if len(checks) == 0 && !pr.CIObservedAt.IsZero() {
		detail := "The provider reports no checks for the current head, so there is no CI requirement to wait for"
		if blocked {
			detail += "; its merge state is BLOCKED, which may mean a required check has not reported, but AO does not invent a CI requirement"
		}
		return ciAssessment{State: CINoChecks, Unproven: blocked, Detail: detail}
	}
	return ciAssessment{State: CIUnknown, Wait: true, Detail: "CI status for the current head is not known yet; AO will not treat that as passing"}
}
