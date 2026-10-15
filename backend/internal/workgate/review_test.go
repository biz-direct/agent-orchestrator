package workgate

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

const (
	gateHead  = "abcdef0123456789abcdef0123456789abcdef01"
	gateOther = "fedcba9876543210fedcba9876543210fedcba98"
	gatePR    = "https://github.com/o/r/pull/9"
)

func gateBase() ReviewInput {
	return ReviewInput{
		Checkpoint: gateHead, Branch: "feat/x", Git: GitState{Branch: "feat/x", Head: gateHead},
		Facts: ReviewFacts{
			PRs:    []domain.PullRequest{{URL: gatePR, Number: 9, SourceBranch: "feat/x", HeadSHA: gateHead, CI: domain.CIPassing, CIObservedAt: time.Now()}},
			Runs:   []domain.ReviewRun{{ID: "r1", PRURL: gatePR, TargetSHA: gateHead, Status: domain.ReviewRunComplete, Verdict: domain.VerdictApproved, CreatedAt: time.Now()}},
			Checks: map[string][]domain.PullRequestCheck{},
		},
	}
}

func TestEvaluateReviewDecisions(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*ReviewInput)
		kind     ReviewKind
		code     string
		pause    string
		failedAt bool
	}{
		{name: "approved and passing completes", kind: ReviewComplete, code: GateReady},
		{name: "passing but provider blocked waits for CI status", mutate: func(in *ReviewInput) { in.Facts.PRs[0].ProviderMergeStateStatus = "BLOCKED" }, kind: ReviewWait, code: WaitAwaitingCIStatus},
		{name: "passing and clean completes", mutate: func(in *ReviewInput) { in.Facts.PRs[0].ProviderMergeStateStatus = "CLEAN" }, kind: ReviewComplete, code: GateReady},
		{name: "workspace unreadable", mutate: func(in *ReviewInput) { in.GitErr = errors.New("boom") }, kind: ReviewPause, pause: ReasonReviewUnverifiable},
		{name: "branch switched", mutate: func(in *ReviewInput) { in.Git.Branch = "other" }, kind: ReviewPause, pause: ReasonUnexpectedChanges},
		{name: "untracked files do not matter", mutate: func(in *ReviewInput) { in.Git.DirtyTotal = 3 }, kind: ReviewComplete, code: GateReady},
		{name: "tracked edits pause", mutate: func(in *ReviewInput) { in.Git.TrackedTotal = 1 }, kind: ReviewPause, pause: ReasonUnexpectedChanges},
		{name: "head moved", mutate: func(in *ReviewInput) { in.Git.Head = gateOther }, kind: ReviewPause, pause: ReasonHeadChanged},
		{name: "no pull request", mutate: func(in *ReviewInput) { in.Facts.PRs = nil }, kind: ReviewWait, code: WaitAwaitingPullRequest},
		{name: "pull request for another branch", mutate: func(in *ReviewInput) { in.Facts.PRs[0].SourceBranch = "other" }, kind: ReviewWait, code: WaitAwaitingPullRequest},
		{name: "closed pull request", mutate: func(in *ReviewInput) { in.Facts.PRs[0].Closed = true }, kind: ReviewPause, pause: ReasonPullRequestClosed},
		{name: "lagging pull request head", mutate: func(in *ReviewInput) { in.Facts.PRs[0].HeadSHA = gateOther }, kind: ReviewWait, code: WaitAwaitingPRHead},
		{name: "empty pull request head", mutate: func(in *ReviewInput) { in.Facts.PRs[0].HeadSHA = "" }, kind: ReviewWait, code: WaitAwaitingPRHead},
		{
			name: "linked head moved",
			mutate: func(in *ReviewInput) {
				in.Link = &ReviewLink{PRURL: gatePR, HeadSHA: gateOther}
				in.Facts.PRs[0].HeadSHA = gateHead
			},
			kind: ReviewPause, pause: ReasonHeadChanged,
		},
		{
			name:   "linked to a different pull request",
			mutate: func(in *ReviewInput) { in.Link = &ReviewLink{PRURL: gatePR + "0", HeadSHA: gateHead} },
			kind:   ReviewPause, pause: ReasonHeadChanged,
		},
		{name: "abbreviated checkpoint matches", mutate: func(in *ReviewInput) { in.Checkpoint, in.Git.Head = gateHead[:12], gateHead[:12] }, kind: ReviewComplete, code: GateReady},
		{name: "no review yet, auto on", mutate: func(in *ReviewInput) { in.Facts.Runs = nil; in.AutoEnabled = true }, kind: ReviewTrigger, code: WaitAwaitingAutoReview},
		{name: "no review yet, auto off", mutate: func(in *ReviewInput) { in.Facts.Runs = nil }, kind: ReviewWait, code: WaitAwaitingManual},
		{
			name:   "review for another head ignored",
			mutate: func(in *ReviewInput) { in.Facts.Runs[0].TargetSHA = gateOther },
			kind:   ReviewWait, code: WaitAwaitingManual,
		},
		{name: "review running", mutate: func(in *ReviewInput) { in.Facts.Runs[0].Status, in.Facts.Runs[0].Verdict = domain.ReviewRunRunning, "" }, kind: ReviewWait, code: WaitReviewRunning},
		{name: "review delivered counts as complete", mutate: func(in *ReviewInput) { in.Facts.Runs[0].Status = domain.ReviewRunDelivered }, kind: ReviewComplete, code: GateReady},
		{name: "reviewer failed", mutate: func(in *ReviewInput) { in.Facts.Runs[0].Status = domain.ReviewRunFailed }, kind: ReviewPause, pause: ReasonReviewOperational},
		{name: "no verdict", mutate: func(in *ReviewInput) { in.Facts.Runs[0].Verdict = "" }, kind: ReviewPause, pause: ReasonReviewOperational},
		{name: "changes requested", mutate: func(in *ReviewInput) { in.Facts.Runs[0].Verdict = domain.VerdictChangesRequested }, kind: ReviewPause, pause: ReasonReviewChangesRequested, failedAt: true},
		{
			name: "the newest pass for the head wins",
			mutate: func(in *ReviewInput) {
				in.Facts.Runs = append(in.Facts.Runs, domain.ReviewRun{ID: "r2", PRURL: gatePR, TargetSHA: gateHead, Status: domain.ReviewRunComplete, Verdict: domain.VerdictChangesRequested, CreatedAt: time.Now().Add(time.Minute)})
			},
			kind: ReviewPause, pause: ReasonReviewChangesRequested, failedAt: true,
		},
		{name: "ambiguous pull requests", mutate: func(in *ReviewInput) {
			second := in.Facts.PRs[0]
			second.URL += "1"
			in.Facts.PRs = append(in.Facts.PRs, second)
		}, kind: ReviewPause, pause: ReasonPullRequestAmbiguous},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := gateBase()
			if tc.mutate != nil {
				tc.mutate(&in)
			}
			got := EvaluateReview(in)
			if got.Kind != tc.kind || (tc.code != "" && got.Code != tc.code) || (tc.pause != "" && got.Pause != tc.pause) || got.FinishFailed != tc.failedAt {
				t.Fatalf("got kind=%v code=%q pause=%q finishFailed=%v detail=%q; want kind=%v code=%q pause=%q finishFailed=%v", got.Kind, got.Code, got.Pause, got.FinishFailed, got.Detail, tc.kind, tc.code, tc.pause, tc.failedAt)
			}
			if got.Kind != ReviewComplete && got.Detail == "" {
				t.Fatal("every wait and pause must carry an explicit reason")
			}
		})
	}
}

func TestAssessCI(t *testing.T) {
	now := time.Now()
	base := domain.PullRequest{CIObservedAt: now}
	stale := []domain.PullRequestCheck{{Name: "ci", CommitHash: gateOther}}
	current := []domain.PullRequestCheck{{Name: "ci", CommitHash: gateHead}}
	cases := []struct {
		name   string
		ci     domain.CIState
		state  string
		merge  string
		checks []domain.PullRequestCheck
		observ bool
		want   string
		wait   bool
		fail   bool
		// unproven is whether required-check coverage is unconfirmed.
		unproven bool
	}{
		{name: "passing", ci: domain.CIPassing, observ: true, checks: current, want: CIPassing, unproven: true},
		{name: "passing clean is confirmed", ci: domain.CIPassing, merge: "CLEAN", observ: true, checks: current, want: CIPassing},
		{name: "passing blocked cannot be proven", ci: domain.CIPassing, merge: "BLOCKED", observ: true, checks: current, want: CIUnknown, wait: true, unproven: true},
		{name: "no checks and blocked is noted but not invented", ci: domain.CIUnknown, merge: "BLOCKED", observ: true, want: CINoChecks, unproven: true},
		{name: "no checks and clean has no requirement", ci: domain.CIUnknown, merge: "CLEAN", observ: true, want: CINoChecks},
		{name: "failing blocked", ci: domain.CIFailing, merge: "BLOCKED", observ: true, want: CIFailing, fail: true},
		{name: "failing no merge state", ci: domain.CIFailing, observ: true, want: CIFailing, fail: true},
		{name: "failing unstable is non required", ci: domain.CIFailing, merge: "unstable", observ: true, want: CINonRequiredOnly},
		{name: "pending clean waits", ci: domain.CIPending, merge: "CLEAN", observ: true, want: CIPending, wait: true},
		{name: "pending unstable is non required", ci: domain.CIPending, merge: "UNSTABLE", observ: true, want: CINonRequiredOnly},
		{name: "unknown never observed waits", ci: domain.CIUnknown, want: CIUnknown, wait: true},
		{name: "unknown observed without checks has no requirement", ci: domain.CIUnknown, observ: true, want: CINoChecks},
		{name: "unknown with checks waits", ci: domain.CIUnknown, observ: true, checks: current, want: CIUnknown, wait: true},
		{name: "empty ci state is unknown", ci: "", want: CIUnknown, wait: true},
		{name: "stale check commit waits", ci: domain.CIPassing, observ: true, checks: stale, want: CIUnknown, wait: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pr := base
			pr.CI, pr.ProviderMergeStateStatus = tc.ci, tc.merge
			if !tc.observ {
				pr.CIObservedAt = time.Time{}
			}
			got := AssessCI(pr, tc.checks, gateHead)
			if got.State != tc.want || got.Wait != tc.wait || got.Fail != tc.fail || got.Unproven != tc.unproven || got.Detail == "" {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestReadyDetailStatesWhenRequiredCheckCoverageIsUnproven(t *testing.T) {
	in := gateBase()
	got := EvaluateReview(in)
	if got.Kind != ReviewComplete || !strings.Contains(got.Detail, "required-check coverage unproven") {
		t.Fatalf("a pass without a confirming merge state must say so: %+v", got)
	}
	in.Facts.PRs[0].ProviderMergeStateStatus = "CLEAN"
	if got := EvaluateReview(in); got.Kind != ReviewComplete || strings.Contains(got.Detail, "unproven") {
		t.Fatalf("a confirmed pass carries no caveat: %+v", got)
	}
}
