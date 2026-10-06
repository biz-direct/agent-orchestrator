package pipelineruns_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/pipelineruns"
)

const reviewURL = "https://github.com/o/r/pull/1"

const buildTestReviewWorkflow = `version: 1
id: build-test-review
description: Build, Test, then Review
stages:
  - {id: build, kind: build}
  - {id: test, kind: specialist, profile: tester, repairTo: build}
  - {id: review, kind: review, repairTo: build}
`

const tightBudgetWorkflow = `version: 1
id: tight-budget
description: Build, Test, Review with two repairs
repairBudget: 2
stages:
  - {id: build, kind: build}
  - {id: test, kind: specialist, profile: tester, repairTo: build}
  - {id: review, kind: review, repairTo: build}
`

const reviewNoRepairWorkflow = `version: 1
id: review-norepair
description: Build then Review without a repair route
stages:
  - {id: build, kind: build}
  - {id: review, kind: review}
`

// fakeReviews stands in for the review subsystem and the SCM facts.
type fakeReviews struct {
	mu       sync.Mutex
	facts    pipelineruns.ReviewFacts
	factsErr error
	skip     string
	trigErr  error
	triggers int
	bypass   bool
}

func (f *fakeReviews) Facts(context.Context, domain.SessionID) (pipelineruns.ReviewFacts, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.facts, f.factsErr
}

func (f *fakeReviews) TriggerAuto(ctx context.Context, _ domain.SessionID) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.triggers++
	f.bypass = ports.PipelineBypass(ctx)
	return f.skip, f.trigErr
}

func (f *fakeReviews) set(fn func(*pipelineruns.ReviewFacts)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.facts)
}

func (f *fakeReviews) triggerCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.triggers
}

func openPR(head string) domain.PullRequest {
	return domain.PullRequest{
		URL: reviewURL, Number: 1, SourceBranch: "task-branch", HeadSHA: head,
		CI: domain.CIPassing, CIObservedAt: time.Now(),
	}
}

func reviewRun(id, head string, status domain.ReviewRunStatus, verdict domain.ReviewVerdict, at time.Time) domain.ReviewRun {
	return domain.ReviewRun{ID: id, SessionID: "s", PRURL: reviewURL, TargetSHA: head, Status: status, Verdict: verdict, CreatedAt: at}
}

type reviewing struct {
	*staged
	reviews *fakeReviews
}

func newReviewing(t *testing.T, auto bool) *reviewing {
	t.Helper()
	f := newFixture(t, map[string]string{
		".ao/pipelines/workflows/build-review.yaml":      reviewWorkflow,
		".ao/pipelines/workflows/build-test-review.yaml": buildTestReviewWorkflow,
		".ao/pipelines/workflows/review-norepair.yaml":   reviewNoRepairWorkflow,
		".ao/pipelines/workflows/tight-budget.yaml":      tightBudgetWorkflow,
		".ao/pipelines/profiles/tester.yaml":             testerProfileYAML,
	})
	exec := &fakeExecutor{store: f.store, repo: f.repo}
	rev := &fakeReviews{}
	f.svc = pipelineruns.New(pipelineruns.Deps{Store: f.store, Messenger: f.messenger, Executor: exec, Reviews: rev})
	if auto {
		if _, err := f.store.SetSessionAutoReview(context.Background(), f.sessionID, true, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	return &reviewing{staged: &staged{fixture: f, exec: exec, gate: pipelineruns.NewStoreGate(f.store, nil)}, reviews: rev}
}

// toReview runs build-review through Build and the handoff, leaving the Review
// attempt active, and returns the reviewed head.
func (r *reviewing) toReview() (pipelineruns.RunView, string) {
	return r.toReviewVia("build-review")
}

func (r *reviewing) toReviewVia(workflow string) (pipelineruns.RunView, string) {
	r.t.Helper()
	run, err := r.svc.Start(context.Background(), pipelineruns.StartInput{SessionID: r.sessionID, WorkflowID: workflow, RequestedBy: "user"})
	if err != nil {
		r.t.Fatal(err)
	}
	_, head := r.buildAndSubmit(run)
	r.drive()
	view := r.get()
	if view.CurrentStageID != "review" || view.Attempts[len(view.Attempts)-1].State != "active" {
		r.t.Fatalf("review attempt must be active after the handoff: %+v", view.Attempts)
	}
	return view, head
}

func (r *reviewing) drive() {
	r.t.Helper()
	if err := r.svc.DriveHandoffs(context.Background()); err != nil {
		r.t.Fatal(err)
	}
}

func (r *reviewing) setPR(head string) {
	r.reviews.set(func(f *pipelineruns.ReviewFacts) {
		pr := openPR(head)
		f.PRs = []domain.PullRequest{pr}
		f.Checks = map[string][]domain.PullRequestCheck{}
	})
}

func (r *reviewing) setRun(run domain.ReviewRun) {
	r.reviews.set(func(f *pipelineruns.ReviewFacts) { f.Runs = append(f.Runs[:0:0], run) })
}

func gateOf(t *testing.T, v pipelineruns.RunView) pipelineruns.ReviewGateView {
	t.Helper()
	if v.ReviewGate == nil {
		t.Fatalf("expected a review gate: %+v", v)
	}
	return *v.ReviewGate
}

func TestReviewStageIsReachedAfterBuildWithoutAnExecutor(t *testing.T) {
	r := newReviewing(t, false)
	run, head := r.toReview()
	review := run.Attempts[len(run.Attempts)-1]
	if review.StageID != "review" || review.ExecutorSessionID != "" || review.ConversationSessionID != "" || review.InputCommit != head {
		t.Fatalf("review attempt: %+v", review)
	}
	// The writer was fenced before the review became active, and no specialist
	// conversation was started for it.
	if got := strings.Join(r.exec.events, ","); got != "relinquish:"+string(r.sessionID) {
		t.Fatalf("handoff to review fences the worker only: %s", got)
	}
	if run.Stages[1].SettingsSource != pipelineruns.SourceReviewer || run.Stages[1].Harness != "" {
		t.Fatalf("review uses the configured reviewer, not a stage harness: %+v", run.Stages[1])
	}
	if r.admitted(r.sessionID) {
		t.Fatal("the worker must stay stopped while AO's review evaluates the checkpoint")
	}
}

func TestReviewManualWaitThenApprovedAndPassingCIcompletes(t *testing.T) {
	r := newReviewing(t, false)
	_, head := r.toReview()
	ctx := context.Background()

	// No pull request yet: the run waits visibly instead of passing or pausing.
	r.drive()
	if g := gateOf(t, r.get()); g.State != "waiting" || g.Code != pipelineruns.WaitAwaitingPullRequest {
		t.Fatalf("no PR: %+v", g)
	}

	// PR lagging behind the checkpoint: AO does not review a different revision.
	r.setPR("0000000000000000000000000000000000000000")
	r.drive()
	if g := gateOf(t, r.get()); g.Code != pipelineruns.WaitAwaitingPRHead || g.State != "waiting" {
		t.Fatalf("lagging head: %+v", g)
	}

	// Auto review is off: the run shows that it waits for a person and never
	// triggers the reviewer itself.
	r.setPR(head)
	r.drive()
	g := gateOf(t, r.get())
	if g.State != "waiting" || g.Code != pipelineruns.WaitAwaitingManual || !g.Manual || g.AutoReview || g.HeadSHA != head || g.Checkpoint != head {
		t.Fatalf("manual wait: %+v", g)
	}
	if r.reviews.triggerCount() != 0 {
		t.Fatal("with Auto review off the pipeline must not start the reviewer")
	}
	if ok, _ := r.svc.ReviewTriggerAllowed(ctx, r.sessionID); !ok {
		t.Fatal("the review controls stay available at the Review stage")
	}

	// A manual run is picked up and followed; an approval alone with failing CI
	// does not complete the run.
	now := time.Now()
	r.setRun(reviewRun("rr1", head, domain.ReviewRunRunning, domain.VerdictNone, now))
	r.drive()
	if g := gateOf(t, r.get()); g.Code != pipelineruns.WaitReviewRunning || g.ReviewRunID != "rr1" {
		t.Fatalf("running: %+v", g)
	}
	link, ok, err := r.store.GetPipelineReviewLink(ctx, r.get().Attempts[1].ID)
	if err != nil || !ok || link.HeadSHA != head || link.PRURL != reviewURL || link.ReviewRunID != "rr1" {
		t.Fatalf("review stage must be linked to the exact head and run: %+v ok=%v err=%v", link, ok, err)
	}

	r.setRun(reviewRun("rr1", head, domain.ReviewRunComplete, domain.VerdictApproved, now))
	r.reviews.set(func(f *pipelineruns.ReviewFacts) {
		f.PRs[0].CI = domain.CIPending
	})
	r.drive()
	if v := r.get(); v.State != "running" || gateOf(t, v).Code != pipelineruns.WaitAwaitingCI {
		t.Fatalf("approved but required CI pending must wait: %+v", v.ReviewGate)
	}

	r.reviews.set(func(f *pipelineruns.ReviewFacts) { f.PRs[0].CI = domain.CIPassing })
	r.drive()
	done := r.get()
	if done.State != "completed" || done.Checkpoint == nil || done.Checkpoint.OutputCommit != head || done.Checkpoint.StageID != "review" {
		t.Fatalf("approved + passing CI on the current head completes: %+v", done)
	}
	if len(done.Reviews) != 1 || done.Reviews[0].ReviewRunID != "rr1" || done.Reviews[0].HeadSHA != head || !done.Reviews[0].Current || done.Reviews[0].Outcome != "succeeded" {
		t.Fatalf("review evidence: %+v", done.Reviews)
	}
	if done.ReviewGate != nil {
		t.Fatalf("a finished run has no live gate: %+v", done.ReviewGate)
	}
	if !r.admitted(r.sessionID) {
		t.Fatal("a completed run releases the worker")
	}
}

func TestReviewAutoTriggersTheBuiltInReviewOnBehalfOfTheRun(t *testing.T) {
	r := newReviewing(t, true)
	_, head := r.toReview()
	r.setPR(head)

	r.drive()
	if r.reviews.triggerCount() != 1 || !r.reviews.bypass {
		t.Fatalf("auto review must start once, marked as the pipeline's own trigger: n=%d bypass=%v", r.reviews.triggerCount(), r.reviews.bypass)
	}
	events := r.get().Events
	found := false
	for _, e := range events {
		found = found || e.Kind == "review_triggered"
	}
	if !found {
		t.Fatalf("the trigger must be recorded: %+v", events)
	}

	// Once the reviewer is running no further triggers are made.
	r.setRun(reviewRun("rr1", head, domain.ReviewRunRunning, domain.VerdictNone, time.Now()))
	r.drive()
	r.drive()
	if r.reviews.triggerCount() != 1 {
		t.Fatalf("a running review must not be triggered again: %d", r.reviews.triggerCount())
	}
}

func TestReviewAutoNotEligibleYetWaitsInsteadOfPausing(t *testing.T) {
	r := newReviewing(t, true)
	_, head := r.toReview()
	r.setPR(head)
	r.reviews.skip = "idle_threshold_not_met"
	r.drive()
	v := r.get()
	if v.State != "running" || gateOf(t, v).Code != pipelineruns.WaitAwaitingAutoReview || !strings.Contains(gateOf(t, v).Message, "idle_threshold_not_met") {
		t.Fatalf("an ineligible task waits with the reason: %+v", v.ReviewGate)
	}
	r.reviews.skip = ""
	r.drive()
	if r.reviews.triggerCount() != 2 {
		t.Fatalf("the trigger is retried once the task is eligible: %d", r.reviews.triggerCount())
	}
}

func TestReviewerOperationalFailurePausesWithoutRetry(t *testing.T) {
	t.Run("trigger error", func(t *testing.T) {
		r := newReviewing(t, true)
		_, head := r.toReview()
		r.setPR(head)
		r.reviews.trigErr = context.DeadlineExceeded
		r.drive()
		v := r.get()
		if v.State != "paused" || v.PauseReason != string(pipelineruns.PauseReviewOperational) {
			t.Fatalf("a reviewer that cannot start pauses the run: %+v", v)
		}
		r.drive()
		if r.reviews.triggerCount() != 1 {
			t.Fatal("a paused run never retries the reviewer behind its back")
		}
	})
	t.Run("failed run", func(t *testing.T) {
		r := newReviewing(t, true)
		_, head := r.toReview()
		r.setPR(head)
		r.setRun(reviewRun("rr1", head, domain.ReviewRunFailed, domain.VerdictNone, time.Now()))
		r.drive()
		v := r.get()
		if v.State != "paused" || v.PauseReason != string(pipelineruns.PauseReviewOperational) {
			t.Fatalf("a failed reviewer pauses instead of using the automatic retry limit: %+v", v)
		}
		if r.reviews.triggerCount() != 0 {
			t.Fatal("the pipeline must not retry a failed review automatically")
		}
		if v.RepairsUsed != 0 {
			t.Fatal("operational failures never spend the repair budget")
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		r := newReviewing(t, false)
		_, head := r.toReview()
		r.setPR(head)
		r.setRun(reviewRun("rr1", head, domain.ReviewRunCancelled, domain.VerdictNone, time.Now()))
		r.drive()
		if v := r.get(); v.State != "paused" || v.PauseReason != string(pipelineruns.PauseReviewOperational) {
			t.Fatalf("%+v", v)
		}
	})
}

func TestReviewChangesRequestedWithoutARepairRoutePausesAndDoesNotWakeTheWorker(t *testing.T) {
	r := newReviewing(t, false)
	_, head := r.toReviewVia("review-norepair")
	r.setPR(head)
	r.setRun(reviewRun("rr1", head, domain.ReviewRunComplete, domain.VerdictChangesRequested, time.Now()))
	sent := len(r.messenger.sent)
	r.drive()
	v := r.get()
	if v.State != "paused" || v.PauseReason != string(pipelineruns.PauseReviewChangesRequested) {
		t.Fatalf("changes requested is an actionable pause: %+v", v)
	}
	review := v.Attempts[len(v.Attempts)-1]
	if review.State != "failed" || review.Outcome != "changes_requested" || v.RepairsUsed != 0 {
		t.Fatalf("the verdict is retained as the attempt's result: %+v", review)
	}
	if len(r.messenger.sent) != sent {
		t.Fatalf("the pipeline must not nudge the worker itself: %v", r.messenger.sent[sent:])
	}
	if r.admitted(r.sessionID) {
		t.Fatal("a paused review keeps the worker stopped until a person decides")
	}
	// A paused run no longer accepts a review pass until it is resumed.
	if ok, reason := r.svc.ReviewTriggerAllowed(context.Background(), r.sessionID); ok || !strings.Contains(reason, "paused") {
		t.Fatalf("ok=%v reason=%q", ok, reason)
	}
}

func TestReviewApprovalForAnotherHeadNeverCounts(t *testing.T) {
	r := newReviewing(t, false)
	_, head := r.toReview()
	r.setPR(head)
	r.setRun(reviewRun("old", "1111111111111111111111111111111111111111", domain.ReviewRunComplete, domain.VerdictApproved, time.Now()))
	r.drive()
	v := r.get()
	if v.State != "running" || gateOf(t, v).Code != pipelineruns.WaitAwaitingManual || gateOf(t, v).ReviewRunID != "" {
		t.Fatalf("a stale approval must not satisfy the current head: %+v", v.ReviewGate)
	}
}

func TestReviewHeadChangesAfterLinkingPause(t *testing.T) {
	t.Run("pull request head moves", func(t *testing.T) {
		r := newReviewing(t, false)
		_, head := r.toReview()
		r.setPR(head)
		r.setRun(reviewRun("rr1", head, domain.ReviewRunRunning, domain.VerdictNone, time.Now()))
		r.drive()
		r.setPR("2222222222222222222222222222222222222222")
		r.drive()
		if v := r.get(); v.State != "paused" || v.PauseReason != string(pipelineruns.PauseHeadChanged) {
			t.Fatalf("a moved PR head after linking pauses: %+v", v)
		}
	})
	t.Run("workspace head moves", func(t *testing.T) {
		r := newReviewing(t, false)
		_, head := r.toReview()
		r.setPR(head)
		writeFile(t, r.repo, "sneaky.txt", "x\n")
		git(t, r.repo, "add", "-A")
		git(t, r.repo, "commit", "-q", "-m", "sneaky")
		r.drive()
		if v := r.get(); v.State != "paused" || v.PauseReason != string(pipelineruns.PauseHeadChanged) {
			t.Fatalf("an unexpected commit while reviewing pauses: %+v", v)
		}
	})
	t.Run("tracked edits", func(t *testing.T) {
		r := newReviewing(t, false)
		_, head := r.toReview()
		r.setPR(head)
		writeFile(t, r.repo, "feature.txt", "edited\n")
		r.drive()
		if v := r.get(); v.State != "paused" || v.PauseReason != string(pipelineruns.PauseUnexpectedChanges) {
			t.Fatalf("uncommitted edits to tracked files do not match the reviewed head: %+v", v)
		}
	})
}

func TestReviewUnknownOrUnclassifiedCINeverPasses(t *testing.T) {
	staleCheck := []domain.PullRequestCheck{{Name: "ci", CommitHash: "3333333333333333333333333333333333333333", Status: domain.PRCheckPassed}}
	cases := map[string]struct {
		mutate    func(*pipelineruns.ReviewFacts)
		wantState string
		wantCode  string
		wantPause string
	}{
		"unknown CI not observed yet": {
			mutate:    func(f *pipelineruns.ReviewFacts) { f.PRs[0].CI, f.PRs[0].CIObservedAt = domain.CIUnknown, time.Time{} },
			wantState: "running", wantCode: pipelineruns.WaitAwaitingCIStatus,
		},
		"stale checks for an older commit": {
			mutate:    func(f *pipelineruns.ReviewFacts) { f.Checks[reviewURL] = staleCheck },
			wantState: "running", wantCode: pipelineruns.WaitAwaitingCIStatus,
		},
		"failing check that may be required": {
			mutate: func(f *pipelineruns.ReviewFacts) {
				f.PRs[0].CI, f.PRs[0].ProviderMergeStateStatus = domain.CIFailing, "BLOCKED"
			},
			wantState: "paused", wantPause: string(pipelineruns.PauseCIFailing),
		},
		"failing check with no merge state": {
			mutate:    func(f *pipelineruns.ReviewFacts) { f.PRs[0].CI = domain.CIFailing },
			wantState: "paused", wantPause: string(pipelineruns.PauseCIFailing),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := newReviewing(t, false)
			_, head := r.toReview()
			r.setPR(head)
			r.setRun(reviewRun("rr1", head, domain.ReviewRunComplete, domain.VerdictApproved, time.Now()))
			r.reviews.set(tc.mutate)
			r.drive()
			v := r.get()
			if v.State != tc.wantState {
				t.Fatalf("state = %s, want %s: %+v", v.State, tc.wantState, v.ReviewGate)
			}
			if tc.wantCode != "" && gateOf(t, v).Code != tc.wantCode {
				t.Fatalf("gate: %+v", gateOf(t, v))
			}
			if tc.wantPause != "" && v.PauseReason != tc.wantPause {
				t.Fatalf("pause = %s, want %s", v.PauseReason, tc.wantPause)
			}
		})
	}
}

func TestReviewNoRequiredCIMeansNoInventedRequirement(t *testing.T) {
	r := newReviewing(t, false)
	_, head := r.toReview()
	r.setPR(head)
	r.reviews.set(func(f *pipelineruns.ReviewFacts) { f.PRs[0].CI = domain.CIUnknown })
	r.setRun(reviewRun("rr1", head, domain.ReviewRunComplete, domain.VerdictApproved, time.Now()))
	r.drive()
	if v := r.get(); v.State != "completed" {
		t.Fatalf("a repository with no checks completes on approval alone: %+v", v.ReviewGate)
	}
}

func TestReviewNonRequiredFailingChecksDoNotBlock(t *testing.T) {
	r := newReviewing(t, false)
	_, head := r.toReview()
	r.setPR(head)
	r.reviews.set(func(f *pipelineruns.ReviewFacts) {
		f.PRs[0].CI, f.PRs[0].ProviderMergeStateStatus = domain.CIFailing, "UNSTABLE"
	})
	r.setRun(reviewRun("rr1", head, domain.ReviewRunComplete, domain.VerdictApproved, time.Now()))
	r.drive()
	if v := r.get(); v.State != "completed" {
		t.Fatalf("an UNSTABLE merge state means only non-required checks fail: %+v", v.ReviewGate)
	}
}

func TestReviewPullRequestClosedOrAmbiguousPauses(t *testing.T) {
	r := newReviewing(t, false)
	_, head := r.toReview()
	r.setPR(head)
	r.reviews.set(func(f *pipelineruns.ReviewFacts) { f.PRs[0].Merged = true })
	r.drive()
	if v := r.get(); v.State != "paused" || v.PauseReason != string(pipelineruns.PausePullRequestClosed) {
		t.Fatalf("a merged PR pauses the run: %+v", v)
	}

	r2 := newReviewing(t, false)
	_, head2 := r2.toReview()
	r2.setPR(head2)
	r2.reviews.set(func(f *pipelineruns.ReviewFacts) {
		second := f.PRs[0]
		second.URL = reviewURL + "2"
		f.PRs = append(f.PRs, second)
	})
	r2.drive()
	if v := r2.get(); v.State != "paused" || v.PauseReason != string(pipelineruns.PausePullRequestAmbiguous) {
		t.Fatalf("two open PRs for the branch are never guessed between: %+v", v)
	}
}

func TestReviewStageDoesNotTakeSubmittedResults(t *testing.T) {
	r := newReviewing(t, false)
	run, head := r.toReview()
	att := run.Attempts[len(run.Attempts)-1]
	_, err := r.svc.Submit(context.Background(), pipelineruns.SubmitInput{
		SessionID: r.sessionID, RunID: run.ID, AttemptID: att.ID, IdempotencyKey: "k", Outcome: "succeeded",
		ExpectedInputCommit: att.InputCommit, OutputCommit: head, Summary: "approve me",
	})
	if code(t, err) != "PIPELINE_STAGE_NOT_SUBMITTABLE" {
		t.Fatalf("an agent cannot approve its own review: %v", err)
	}
	if v := r.get(); v.State != "running" {
		t.Fatalf("%+v", v)
	}
}

func TestReviewTriggersAreGatedUntilTheReviewStage(t *testing.T) {
	r := newReviewing(t, false)
	ctx := context.Background()
	if ok, _ := r.svc.ReviewTriggerAllowed(ctx, r.sessionID); !ok {
		t.Fatal("an ordinary worker is never gated")
	}
	run, err := r.svc.Start(ctx, pipelineruns.StartInput{SessionID: r.sessionID, WorkflowID: "build-test-review", RequestedBy: "user"})
	if err != nil {
		t.Fatal(err)
	}
	if ok, reason := r.svc.ReviewTriggerAllowed(ctx, r.sessionID); ok || !strings.Contains(reason, `"build"`) {
		t.Fatalf("in Build a review must wait: ok=%v reason=%q", ok, reason)
	}
	r.buildAndSubmit(run)
	if ok, _ := r.svc.ReviewTriggerAllowed(ctx, r.sessionID); ok {
		t.Fatal("during the handoff to Test no review may start")
	}
	r.drive()
	if ok, reason := r.svc.ReviewTriggerAllowed(ctx, r.sessionID); ok || !strings.Contains(reason, `"test"`) {
		t.Fatalf("in Test a review must wait: ok=%v reason=%q", ok, reason)
	}
	// The same decision is available to the review engine through the guard.
	guard := pipelineruns.NewStoreGuard(r.store, nil)
	if ok, _ := guard.ReviewTriggerAllowed(ctx, r.sessionID); ok {
		t.Fatal("the store-backed guard must agree")
	}
}

func TestReviewSurvivesADaemonRestart(t *testing.T) {
	r := newReviewing(t, false)
	_, head := r.toReview()
	r.setPR(head)
	r.setRun(reviewRun("rr1", head, domain.ReviewRunRunning, domain.VerdictNone, time.Now()))
	r.drive()

	// A new service over the same store continues from durable facts; the
	// startup reconcile must not pause a Review attempt for lacking an executor.
	again := pipelineruns.New(pipelineruns.Deps{Store: r.store, Messenger: r.messenger, Executor: r.exec, Reviews: r.reviews})
	if err := again.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := r.get(); v.State != "running" {
		t.Fatalf("restart must not pause a waiting review: %+v", v)
	}
	r.setRun(reviewRun("rr1", head, domain.ReviewRunComplete, domain.VerdictApproved, time.Now()))
	if err := again.DriveHandoffs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := r.get(); v.State != "completed" {
		t.Fatalf("the restarted service completes from durable state: %+v", v)
	}
}

func TestReviewStageRejectsStageOverrides(t *testing.T) {
	r := newReviewing(t, false)
	_, err := r.svc.Start(context.Background(), pipelineruns.StartInput{
		SessionID: r.sessionID, WorkflowID: "build-review", RequestedBy: "user",
		Overrides: map[string]pipelineruns.StageOverride{"review": {Harness: "claude-code"}},
	})
	if code(t, err) != "INVALID_PIPELINE_OVERRIDE" {
		t.Fatalf("the reviewer is configured in review settings, not per stage: %v", err)
	}
}
