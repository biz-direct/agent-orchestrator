package pipelineruns

import (
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/workgate"
)

// Review stage pause reasons. Each is actionable: the run keeps the evidence it
// was paused on and nothing here touches the worktree.
const (
	// PauseHeadChanged: the revision under review is no longer the pull
	// request's or the workspace's head, so earlier approvals and CI results do
	// not speak for it.
	PauseHeadChanged domain.PipelinePauseReason = workgate.ReasonHeadChanged
	// PauseReviewChangesRequested: AO's reviewer requested changes on the
	// current head. Review repair routing is a later slice; until then a person
	// decides what happens next.
	PauseReviewChangesRequested domain.PipelinePauseReason = workgate.ReasonReviewChangesRequested
	// PauseReviewOperational: the reviewer failed, was cancelled, or finished
	// without a verdict. It is never retried behind the run's back.
	PauseReviewOperational domain.PipelinePauseReason = workgate.ReasonReviewOperational
	// PauseCIFailing: a CI check failed and AO cannot prove it is not required.
	PauseCIFailing domain.PipelinePauseReason = workgate.ReasonCIFailing
	// PausePullRequestClosed: the task's pull request was merged or closed.
	PausePullRequestClosed domain.PipelinePauseReason = workgate.ReasonPullRequestClosed
	// PausePullRequestAmbiguous: several open pull requests claim the branch.
	PausePullRequestAmbiguous domain.PipelinePauseReason = workgate.ReasonPullRequestAmbiguous
	// PauseReviewUnverifiable: AO could not inspect the facts it needs.
	PauseReviewUnverifiable domain.PipelinePauseReason = workgate.ReasonReviewUnverifiable
)

// Review gate codes shown while the run waits. Waiting needs no decision: the
// gate re-evaluates on its own as soon as the missing fact appears.
const (
	WaitAwaitingPullRequest = workgate.WaitAwaitingPullRequest
	WaitAwaitingPRHead      = workgate.WaitAwaitingPRHead
	WaitAwaitingManual      = workgate.WaitAwaitingManual
	WaitAwaitingAutoReview  = workgate.WaitAwaitingAutoReview
	WaitReviewRunning       = workgate.WaitReviewRunning
	WaitAwaitingCI          = workgate.WaitAwaitingCI
	WaitAwaitingCIStatus    = workgate.WaitAwaitingCIStatus
	// GateReady means every completion condition holds for the current head.
	GateReady = workgate.GateReady
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
	CIPassing         = workgate.CIPassing
	CINoChecks        = workgate.CINoChecks
	CINonRequiredOnly = workgate.CINonRequiredOnly
	CIPending         = workgate.CIPending
	CIFailing         = workgate.CIFailing
	CIUnknown         = workgate.CIUnknown
)

// ReviewFacts are the read-only facts the Review gate decides on, and
// ReviewGateway is the pipeline's view of AO's built-in review subsystem. Both
// are pipeline-independent and live in workgate.
type (
	ReviewFacts   = workgate.ReviewFacts
	ReviewGateway = workgate.ReviewGateway
)

type gateKind = workgate.ReviewKind

const (
	gateWait     = workgate.ReviewWait
	gateTrigger  = workgate.ReviewTrigger
	gatePause    = workgate.ReviewPause
	gateComplete = workgate.ReviewComplete
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

type ciAssessment = workgate.CIAssessment

// evaluateReview decides what a Review attempt should do now using the shared
// current-commit approval and required-CI gate.
func evaluateReview(in gateInput) gateDecision {
	wi := workgate.ReviewInput{
		Checkpoint: in.Checkpoint, Branch: in.Branch, Git: in.Git, GitErr: in.GitErr,
		AutoEnabled: in.AutoEnabled, Facts: in.Facts,
	}
	if in.Link != nil {
		wi.Link = &workgate.ReviewLink{PRURL: in.Link.PRURL, HeadSHA: in.Link.HeadSHA}
	}
	d := workgate.EvaluateReview(wi)
	return gateDecision{
		Kind: d.Kind, Code: d.Code, Detail: d.Detail, Pause: domain.PipelinePauseReason(d.Pause),
		FinishFailed: d.FinishFailed, Outcome: d.Outcome, PR: d.PR, Run: d.Run, CI: d.CI, Linked: d.Linked,
	}
}

func assessCI(pr domain.PullRequest, checks []domain.PullRequestCheck, head string) ciAssessment {
	return workgate.AssessCI(pr, checks, head)
}
