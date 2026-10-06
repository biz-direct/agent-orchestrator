package pipelineruns

import (
	"errors"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

const (
	gateHead  = "abcdef0123456789abcdef0123456789abcdef01"
	gateOther = "fedcba9876543210fedcba9876543210fedcba98"
	gatePR    = "https://github.com/o/r/pull/9"
)

func gateBase() gateInput {
	return gateInput{
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
		mutate   func(*gateInput)
		kind     gateKind
		code     string
		pause    domain.PipelinePauseReason
		failedAt bool
	}{
		{name: "approved and passing completes", kind: gateComplete, code: GateReady},
		{name: "workspace unreadable", mutate: func(in *gateInput) { in.GitErr = errors.New("boom") }, kind: gatePause, pause: PauseReviewUnverifiable},
		{name: "branch switched", mutate: func(in *gateInput) { in.Git.Branch = "other" }, kind: gatePause, pause: PauseUnexpectedChanges},
		{name: "untracked files do not matter", mutate: func(in *gateInput) { in.Git.DirtyTotal = 3 }, kind: gateComplete, code: GateReady},
		{name: "tracked edits pause", mutate: func(in *gateInput) { in.Git.TrackedTotal = 1 }, kind: gatePause, pause: PauseUnexpectedChanges},
		{name: "head moved", mutate: func(in *gateInput) { in.Git.Head = gateOther }, kind: gatePause, pause: PauseHeadChanged},
		{name: "no pull request", mutate: func(in *gateInput) { in.Facts.PRs = nil }, kind: gateWait, code: WaitAwaitingPullRequest},
		{name: "pull request for another branch", mutate: func(in *gateInput) { in.Facts.PRs[0].SourceBranch = "other" }, kind: gateWait, code: WaitAwaitingPullRequest},
		{name: "closed pull request", mutate: func(in *gateInput) { in.Facts.PRs[0].Closed = true }, kind: gatePause, pause: PausePullRequestClosed},
		{name: "lagging pull request head", mutate: func(in *gateInput) { in.Facts.PRs[0].HeadSHA = gateOther }, kind: gateWait, code: WaitAwaitingPRHead},
		{name: "empty pull request head", mutate: func(in *gateInput) { in.Facts.PRs[0].HeadSHA = "" }, kind: gateWait, code: WaitAwaitingPRHead},
		{
			name: "linked head moved",
			mutate: func(in *gateInput) {
				in.Link = &domain.PipelineReviewLink{PRURL: gatePR, HeadSHA: gateOther}
				in.Facts.PRs[0].HeadSHA = gateHead
			},
			kind: gatePause, pause: PauseHeadChanged,
		},
		{
			name:   "linked to a different pull request",
			mutate: func(in *gateInput) { in.Link = &domain.PipelineReviewLink{PRURL: gatePR + "0", HeadSHA: gateHead} },
			kind:   gatePause, pause: PauseHeadChanged,
		},
		{name: "abbreviated checkpoint matches", mutate: func(in *gateInput) { in.Checkpoint, in.Git.Head = gateHead[:12], gateHead[:12] }, kind: gateComplete, code: GateReady},
		{name: "no review yet, auto on", mutate: func(in *gateInput) { in.Facts.Runs = nil; in.AutoEnabled = true }, kind: gateTrigger, code: WaitAwaitingAutoReview},
		{name: "no review yet, auto off", mutate: func(in *gateInput) { in.Facts.Runs = nil }, kind: gateWait, code: WaitAwaitingManual},
		{
			name:   "review for another head ignored",
			mutate: func(in *gateInput) { in.Facts.Runs[0].TargetSHA = gateOther },
			kind:   gateWait, code: WaitAwaitingManual,
		},
		{name: "review running", mutate: func(in *gateInput) { in.Facts.Runs[0].Status, in.Facts.Runs[0].Verdict = domain.ReviewRunRunning, "" }, kind: gateWait, code: WaitReviewRunning},
		{name: "review delivered counts as complete", mutate: func(in *gateInput) { in.Facts.Runs[0].Status = domain.ReviewRunDelivered }, kind: gateComplete, code: GateReady},
		{name: "reviewer failed", mutate: func(in *gateInput) { in.Facts.Runs[0].Status = domain.ReviewRunFailed }, kind: gatePause, pause: PauseReviewOperational},
		{name: "no verdict", mutate: func(in *gateInput) { in.Facts.Runs[0].Verdict = "" }, kind: gatePause, pause: PauseReviewOperational},
		{name: "changes requested", mutate: func(in *gateInput) { in.Facts.Runs[0].Verdict = domain.VerdictChangesRequested }, kind: gatePause, pause: PauseReviewChangesRequested, failedAt: true},
		{
			name: "the newest pass for the head wins",
			mutate: func(in *gateInput) {
				in.Facts.Runs = append(in.Facts.Runs, domain.ReviewRun{ID: "r2", PRURL: gatePR, TargetSHA: gateHead, Status: domain.ReviewRunComplete, Verdict: domain.VerdictChangesRequested, CreatedAt: time.Now().Add(time.Minute)})
			},
			kind: gatePause, pause: PauseReviewChangesRequested, failedAt: true,
		},
		{name: "ambiguous pull requests", mutate: func(in *gateInput) {
			second := in.Facts.PRs[0]
			second.URL += "1"
			in.Facts.PRs = append(in.Facts.PRs, second)
		}, kind: gatePause, pause: PausePullRequestAmbiguous},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := gateBase()
			if tc.mutate != nil {
				tc.mutate(&in)
			}
			got := evaluateReview(in)
			if got.Kind != tc.kind || (tc.code != "" && got.Code != tc.code) || (tc.pause != "" && got.Pause != tc.pause) || got.FinishFailed != tc.failedAt {
				t.Fatalf("got kind=%v code=%q pause=%q finishFailed=%v detail=%q; want kind=%v code=%q pause=%q finishFailed=%v", got.Kind, got.Code, got.Pause, got.FinishFailed, got.Detail, tc.kind, tc.code, tc.pause, tc.failedAt)
			}
			if got.Kind != gateComplete && got.Detail == "" {
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
	}{
		{name: "passing", ci: domain.CIPassing, observ: true, checks: current, want: CIPassing},
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
			got := assessCI(pr, tc.checks, gateHead)
			if got.State != tc.want || got.Wait != tc.wait || got.Fail != tc.fail || got.Detail == "" {
				t.Fatalf("got %+v", got)
			}
		})
	}
}
