package workgate

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// Review pause reasons. Each is actionable: callers keep the evidence the gate
// paused on and nothing here touches the worktree.
const (
	// ReasonHeadChanged: the revision under review is no longer the pull
	// request's or the workspace's head, so earlier approvals and CI results do
	// not speak for it.
	ReasonHeadChanged = "head_changed"
	// ReasonReviewChangesRequested: AO's reviewer requested changes on the
	// current head. Review repair routing is a later slice; until then a person
	// decides what happens next.
	ReasonReviewChangesRequested = "review_changes_requested"
	// ReasonReviewOperational: the reviewer failed, was cancelled, or finished
	// without a verdict. It is never retried behind the run's back.
	ReasonReviewOperational = "review_operational"
	// ReasonCIFailing: a CI check failed and AO cannot prove it is not required.
	ReasonCIFailing = "ci_failing"
	// ReasonPullRequestClosed: the task's pull request was merged or closed.
	ReasonPullRequestClosed = "pull_request_closed"
	// ReasonPullRequestAmbiguous: several open pull requests claim the branch.
	ReasonPullRequestAmbiguous = "pull_request_ambiguous"
	// ReasonReviewUnverifiable: AO could not inspect the facts it needs.
	ReasonReviewUnverifiable = "review_unverifiable"
	// ReasonUnexpectedChanges: the workspace no longer matches the reviewed
	// checkpoint (another branch, or tracked edits).
	ReasonUnexpectedChanges = "unexpected_changes"
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

// ReviewGateway is the narrow view of AO's built-in review
// subsystem. Review is an adapter over it, not a second reviewer.
type ReviewGateway interface {
	// Facts returns the stored pull request, check, and review facts of a task.
	Facts(ctx context.Context, id domain.SessionID) (ReviewFacts, error)
	// TriggerAuto asks the built-in reviewer for the same automatic pass AO's
	// auto-review would start. A non-empty skip reason means the task is not
	// eligible right now (for example not idle long enough); it is not an error.
	TriggerAuto(ctx context.Context, id domain.SessionID) (skipReason string, err error)
}

// ReviewKind is what a review evaluation asks its caller to do.
type ReviewKind int

// Review decisions: wait for a missing fact, start the built-in review, pause
// for a person, or complete because approval and CI hold for the head.
const (
	ReviewWait ReviewKind = iota
	ReviewTrigger
	ReviewPause
	ReviewComplete
)

// ReviewInput is everything the pure decision needs.
type ReviewInput struct {
	Checkpoint  string
	Branch      string
	Git         GitState
	GitErr      error
	AutoEnabled bool
	Link        *ReviewLink
	Facts       ReviewFacts
}

// ReviewDecision is the outcome of one evaluation.
type ReviewDecision struct {
	Kind   ReviewKind
	Code   string
	Detail string
	// Pause is the run pause reason for ReviewPause.
	Pause string
	// FinishFailed closes the attempt as a failed result carrying the verdict
	// instead of interrupting it.
	FinishFailed bool
	Outcome      string

	PR     *domain.PullRequest
	Run    *domain.ReviewRun
	CI     CIAssessment
	Linked bool
}

// CIAssessment is the authoritative required-CI judgement for one head.
type CIAssessment struct {
	State  string // CI* constants
	Wait   bool
	Fail   bool
	Detail string
	// Unproven reports that the provider's merge state does not confirm that
	// every required check reported for this head, so a pass or "no checks"
	// rests on the reported checks alone.
	Unproven bool
}

// EvaluateReview decides what a Review attempt should do now. It is pure so the
// driver, the API view, and the tests share one rule. Every doubt resolves to
// waiting or pausing with a reason; nothing unknown ever counts as a pass.
func EvaluateReview(in ReviewInput) ReviewDecision {
	pause := func(reason, detail string) ReviewDecision {
		return ReviewDecision{Kind: ReviewPause, Code: reason, Pause: reason, Detail: detail}
	}
	wait := func(code, detail string) ReviewDecision {
		return ReviewDecision{Kind: ReviewWait, Code: code, Detail: detail}
	}

	// 1. The revision under review is the accepted checkpoint, and nothing may
	// have changed the workspace since the executors stopped.
	switch {
	case in.GitErr != nil:
		return pause(ReasonReviewUnverifiable, fmt.Sprintf("The workspace could not be inspected: %v", in.GitErr))
	case in.Git.Branch != in.Branch:
		return pause(ReasonUnexpectedChanges, fmt.Sprintf("The workspace is on %q, expected %q", BranchLabel(in.Git.Branch), in.Branch))
	case in.Git.TrackedTotal > 0:
		// Untracked files cannot change the head under review; edits to tracked
		// files mean the worktree no longer matches what is being approved.
		return pause(ReasonUnexpectedChanges, fmt.Sprintf("%d tracked path(s) have uncommitted changes while the review is evaluating %s; they are preserved, not reset", in.Git.TrackedTotal, ShortCommit(in.Checkpoint)))
	case in.Git.Head != in.Checkpoint:
		return pause(ReasonHeadChanged, fmt.Sprintf("HEAD moved to %s while the review was evaluating %s; approvals and CI results for the earlier revision do not cover it", ShortCommit(in.Git.Head), ShortCommit(in.Checkpoint)))
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
		return pause(ReasonPullRequestAmbiguous, fmt.Sprintf("%d open pull requests track branch %s; AO will not guess which one to review", len(open), in.Branch))
	case len(open) == 0 && closed:
		return pause(ReasonPullRequestClosed, fmt.Sprintf("The pull request for branch %s was merged or closed before the review finished", in.Branch))
	case len(open) == 0:
		return wait(WaitAwaitingPullRequest, fmt.Sprintf("No pull request is tracked for branch %s yet. Open one for revision %s; the review starts when it appears", in.Branch, ShortCommit(in.Checkpoint)))
	}
	pr := open[0]
	d := ReviewDecision{PR: &pr}

	// 3. The pull request head is the checkpoint. Before the review is linked a
	// lagging head just waits; after, a different head is a different revision.
	if in.Link != nil && (in.Link.PRURL != pr.URL || !SameCommit(in.Link.HeadSHA, pr.HeadSHA)) {
		d2 := pause(ReasonHeadChanged, fmt.Sprintf("The pull request head moved from %s to %s after the review was linked; approvals and CI results for the earlier head do not cover it", ShortCommit(in.Link.HeadSHA), ShortCommit(pr.HeadSHA)))
		d2.PR = &pr
		return d2
	}
	if pr.HeadSHA == "" || !SameCommit(pr.HeadSHA, in.Checkpoint) {
		d2 := wait(WaitAwaitingPRHead, fmt.Sprintf("The pull request head is %s but the review covers %s. Push the checkpoint to the pull request; AO will not review a different revision", ShortCommit(pr.HeadSHA), ShortCommit(in.Checkpoint)))
		d2.PR = &pr
		return d2
	}
	d.Linked = true

	// 4. The review pass for exactly this head.
	run := LatestRunForHead(in.Facts.Runs, pr.URL, in.Checkpoint)
	d.Run = run
	if run == nil {
		if in.AutoEnabled {
			d.Kind, d.Code, d.Detail = ReviewTrigger, WaitAwaitingAutoReview, "Auto review is on; AO is starting the built-in review for this revision"
			return d
		}
		d.Kind, d.Code, d.Detail = ReviewWait, WaitAwaitingManual, "Auto review is off. Trigger the review for this revision from the task's review controls"
		return d
	}
	switch {
	case run.Status == domain.ReviewRunRunning:
		d.Kind, d.Code, d.Detail = ReviewWait, WaitReviewRunning, "The built-in review is running for this revision"
		return d
	case run.Status == domain.ReviewRunFailed || run.Status == domain.ReviewRunCancelled:
		return withPR(pause(ReasonReviewOperational, fmt.Sprintf("The built-in review %s for revision %s. AO does not retry it behind the run's back; fix the reviewer and trigger it again", run.Status, ShortCommit(in.Checkpoint))), d)
	case run.Verdict == domain.VerdictChangesRequested:
		out := withPR(pause(ReasonReviewChangesRequested, fmt.Sprintf("The built-in review requested changes on revision %s. This workflow has no repair route, so resuming would re-evaluate the same revision; address the findings, then cancel this run and start a new one on the new head", ShortCommit(in.Checkpoint))), d)
		out.FinishFailed, out.Outcome = true, "changes_requested"
		return out
	case run.Verdict != domain.VerdictApproved:
		return withPR(pause(ReasonReviewOperational, fmt.Sprintf("The built-in review finished for revision %s without a verdict", ShortCommit(in.Checkpoint))), d)
	}

	// 5. Required CI for exactly this head.
	d.CI = AssessCI(pr, in.Facts.Checks[pr.URL], in.Checkpoint)
	switch {
	case d.CI.Fail:
		return withPR(pause(ReasonCIFailing, d.CI.Detail), d)
	case d.CI.Wait:
		code := WaitAwaitingCIStatus
		if d.CI.State == CIPending {
			code = WaitAwaitingCI
		}
		d.Kind, d.Code, d.Detail = ReviewWait, code, d.CI.Detail
		return d
	}
	d.Kind, d.Code = ReviewComplete, GateReady
	d.Detail = fmt.Sprintf("Approved by the built-in review for %s; CI: %s", ShortCommit(in.Checkpoint), d.CI.State)
	if d.CI.Unproven {
		d.Detail += " (required-check coverage unproven: " + d.CI.Detail + ")"
	}
	return d
}

func withPR(out, base ReviewDecision) ReviewDecision {
	out.PR, out.Run, out.Linked = base.PR, base.Run, base.Linked
	return out
}

// SameCommit compares full or abbreviated commit ids case-insensitively.
func SameCommit(a, b string) bool {
	a, b = strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	if a == "" || b == "" {
		return false
	}
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

// LatestRunForHead returns the newest AO review pass for the pull request at
// exactly this head. A pass for any other head is stale by definition.
func LatestRunForHead(runs []domain.ReviewRun, prURL, head string) *domain.ReviewRun {
	var matching []domain.ReviewRun
	for _, r := range runs {
		if r.PRURL == prURL && SameCommit(r.TargetSHA, head) {
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

// AssessCI decides whether required CI holds for head. AO has no per-check
// "required" flag; the authoritative signal is the provider's merge state: an
// UNSTABLE pull request is mergeable with only non-required checks failing or
// pending. A repository with no checks at all has no CI requirement, and AO does
// not invent one. Anything it cannot classify waits or pauses with a reason.
func AssessCI(pr domain.PullRequest, checks []domain.PullRequestCheck, head string) CIAssessment {
	for _, c := range checks {
		if c.CommitHash != "" && !SameCommit(c.CommitHash, head) {
			return CIAssessment{State: CIUnknown, Wait: true, Detail: fmt.Sprintf("CI results are still for %s, not the reviewed revision %s; waiting for the provider to report on the current head", ShortCommit(c.CommitHash), ShortCommit(head))}
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
			return CIAssessment{State: CIUnknown, Wait: true, Unproven: true, Detail: "All reported checks pass for the current head, but the provider's merge state is BLOCKED: a required check may not have reported for this head (or another merge requirement, such as a required approval, is unmet). AO cannot prove required CI is satisfied and will not treat the partial check set as passing"}
		}
		if unproven {
			return CIAssessment{State: CIPassing, Unproven: true, Detail: "All reported checks pass for the current head; the provider's merge state does not confirm that every required check reported"}
		}
		return CIAssessment{State: CIPassing, Detail: "All reported checks pass for the current head"}
	case domain.CIFailing:
		if nonRequiredOnly {
			return CIAssessment{State: CINonRequiredOnly, Detail: "Failing checks are not required by the provider's merge rules (merge state UNSTABLE)"}
		}
		return CIAssessment{State: CIFailing, Fail: true, Detail: "A CI check is failing for the current head and AO cannot prove it is not required (the provider's merge state is not UNSTABLE)"}
	case domain.CIPending:
		if nonRequiredOnly {
			return CIAssessment{State: CINonRequiredOnly, Detail: "Pending checks are not required by the provider's merge rules (merge state UNSTABLE)"}
		}
		return CIAssessment{State: CIPending, Wait: true, Detail: "Required CI is still running for the current head"}
	}
	// Unknown: either the provider reports no checks for this repository, or AO
	// has not observed CI yet. Only a recorded observation with no checks counts
	// as "no CI to wait for".
	if len(checks) == 0 && !pr.CIObservedAt.IsZero() {
		detail := "The provider reports no checks for the current head, so there is no CI requirement to wait for"
		if blocked {
			detail += "; its merge state is BLOCKED, which may mean a required check has not reported, but AO does not invent a CI requirement"
		}
		return CIAssessment{State: CINoChecks, Unproven: blocked, Detail: detail}
	}
	return CIAssessment{State: CIUnknown, Wait: true, Detail: "CI status for the current head is not known yet; AO will not treat that as passing"}
}

// ReviewLink is the review a caller already bound to a pull request head. Once
// linked, a different pull request or head is a different revision.
type ReviewLink struct {
	PRURL   string
	HeadSHA string
}

// ShortCommit abbreviates a commit id for messages.
func ShortCommit(c string) string {
	if len(c) > 10 {
		return c[:10]
	}
	if c == "" {
		return "(none)"
	}
	return c
}

// BranchLabel names a branch, or a detached HEAD, for messages.
func BranchLabel(b string) string {
	if b == "" {
		return "a detached HEAD"
	}
	return b
}
