package pipelineruns_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/pipelineruns"
)

// fullPipeline drives Build → Test → Review with the real service, store, and
// git, and fakes only the executors and the review facts.
type fullPipeline struct {
	*reviewing
	workflow string
}

func newFullPipeline(t *testing.T, auto bool, workflow string) *fullPipeline {
	t.Helper()
	return &fullPipeline{reviewing: newReviewing(t, auto), workflow: workflow}
}

func (p *fullPipeline) start() pipelineruns.RunView {
	p.t.Helper()
	run, err := p.svc.Start(context.Background(), pipelineruns.StartInput{SessionID: p.sessionID, WorkflowID: p.workflow, RequestedBy: "user"})
	if err != nil {
		p.t.Fatal(err)
	}
	return run
}

func (p *fullPipeline) commit(msg, file string) string {
	p.t.Helper()
	writeFile(p.t, p.repo, file, msg+"\n")
	git(p.t, p.repo, "add", "-A")
	git(p.t, p.repo, "commit", "-q", "-m", msg)
	return git(p.t, p.repo, "rev-parse", "HEAD")
}

func (p *fullPipeline) active(stage string) pipelineruns.AttemptView {
	p.t.Helper()
	v := p.get()
	for _, a := range v.Attempts {
		if a.StageID == stage && a.State == "active" {
			return a
		}
	}
	p.t.Fatalf("no active %s attempt: %s", stage, brief(v))
	return pipelineruns.AttemptView{}
}

// testerSubmits makes the active Tester report at a fresh test commit.
func (p *fullPipeline) testerSubmits(key, outcome string, report *pipelineruns.StageReport) pipelineruns.SubmitResult {
	p.t.Helper()
	att := p.active("test")
	head := p.commit("tests "+key, "t_"+key+"_test.go")
	res, err := p.svc.Submit(context.Background(), pipelineruns.SubmitInput{
		SessionID: domain.SessionID(att.ConversationSessionID), RunID: p.get().ID, AttemptID: att.ID, ControllerGeneration: att.ControllerGeneration,
		IdempotencyKey: key, Outcome: outcome, ExpectedInputCommit: att.InputCommit, OutputCommit: head, Summary: "tester " + key, Report: report,
	})
	if err != nil {
		p.t.Fatal(err)
	}
	return res
}

// builderSubmits makes the resumed original worker commit and submit.
func (p *fullPipeline) builderSubmits(key string) (pipelineruns.SubmitResult, string) {
	p.t.Helper()
	att := p.active("build")
	head := p.commit("fix "+key, "fix_"+key+".go")
	res, err := p.svc.Submit(context.Background(), pipelineruns.SubmitInput{
		SessionID: p.sessionID, RunID: p.get().ID, AttemptID: att.ID, ControllerGeneration: att.ControllerGeneration,
		IdempotencyKey: "build-" + key, Outcome: "succeeded", ExpectedInputCommit: att.InputCommit, OutputCommit: head, Summary: "fixed " + key,
	})
	if err != nil {
		p.t.Fatal(err)
	}
	return res, head
}

// toReviewStage runs Build and a passing Test, leaving Review active.
func (p *fullPipeline) toReviewStage() string {
	p.t.Helper()
	run := p.start()
	p.buildAndSubmit(run)
	p.drive() // Tester starts
	p.testerSubmits("pass1", "succeeded", passReport())
	p.drive() // Review becomes active
	v := p.get()
	if v.CurrentStageID != "review" || v.Attempts[len(v.Attempts)-1].State != "active" {
		p.t.Fatalf("review must be active: %s", brief(v))
	}
	return v.Attempts[len(v.Attempts)-1].InputCommit
}

func changesRequested(id, head, body string) domain.ReviewRun {
	r := reviewRun(id, head, domain.ReviewRunComplete, domain.VerdictChangesRequested, time.Now())
	r.Body, r.GithubReviewID = body, "9001"
	return r
}

func TestReviewFeedbackReturnsToTheOriginalBuilderThenStraightBackToReview(t *testing.T) {
	p := newFullPipeline(t, true, "build-test-review")
	head := p.toReviewStage()
	p.setPR(head)
	p.drive()
	if p.reviews.triggerCount() != 1 {
		t.Fatalf("auto review starts at the Review stage: %d", p.reviews.triggerCount())
	}
	sent := len(p.messenger.sent)
	starts, resumed := len(p.exec.starts), len(p.exec.resumed)

	p.setRun(changesRequested("rr1", head, "Handle nil input in Parse"))
	p.drive()
	v := p.get()
	if v.State != "running" || v.CurrentStageID != "build" || v.RepairsUsed != 1 || v.RepairsRemaining != 2 {
		t.Fatalf("current-head feedback is routed to Build and counted once: %s", brief(v))
	}
	review := v.Attempts[len(v.Attempts)-2]
	build := v.Attempts[len(v.Attempts)-1]
	if review.StageID != "review" || review.State != "failed" || review.Outcome != "changes_requested" {
		t.Fatalf("review attempt: %+v", review)
	}
	if build.StageID != "build" || build.State != "handoff" || build.RepairSourceAttemptID != review.ID || build.Feedback == nil ||
		build.Feedback.Kind != "review_feedback" || build.Feedback.ReviewRunID != "rr1" || build.Feedback.Revision != head || !strings.Contains(build.Feedback.ReviewBody, "Handle nil input") {
		t.Fatalf("build repair attempt carries the revision-bound review feedback: %+v", build)
	}
	if len(v.Repairs) != 1 || v.Repairs[0].Kind != "review_feedback" || v.Repairs[0].SourceStageID != "review" || v.Repairs[0].ReturnStageID != "review" {
		t.Fatalf("repairs: %+v", v.Repairs)
	}
	if p.admitted(p.sessionID) {
		t.Fatal("nobody executes until the handoff is proven")
	}

	// The original worker conversation is resumed, once, with the feedback.
	p.drive()
	if len(p.exec.resumed) != resumed+1 || len(p.exec.starts) != starts {
		t.Fatalf("exactly the original worker is resumed: resumed=%d starts=%d", len(p.exec.resumed)-resumed, len(p.exec.starts)-starts)
	}
	call := p.exec.resumed[len(p.exec.resumed)-1]
	for _, want := range []string{"repair 1 of 3", "Handle nil input in Parse", "GitHub review: 9001", head, "push the commit", "earlier test results do not cover it"} {
		if !strings.Contains(call.Prompt, want) {
			t.Fatalf("repair prompt missing %q:\n%s", want, call.Prompt)
		}
	}
	if call.ID != p.sessionID || !p.admitted(p.sessionID) {
		t.Fatalf("the original worker is the active Builder: %+v", call)
	}
	if len(p.messenger.sent) != sent {
		t.Fatalf("feedback travels through the pipeline, not a direct nudge: %v", p.messenger.sent[sent:])
	}

	// Build repairs and the run goes directly back to Review, not Test.
	res, newHead := p.builderSubmits("r1")
	if !res.Accepted || res.Run.CurrentStageID != "review" || res.Run.Attempts[len(res.Run.Attempts)-1].AttemptNo != 2 {
		t.Fatalf("after the repair the run returns to Review: %s", brief(res.Run))
	}
	p.drive()
	if len(p.exec.starts) != starts || len(p.exec.resumed) != resumed+1 {
		t.Fatalf("a review repair must not re-run the Tester: starts=%d resumed=%d", len(p.exec.starts)-starts, len(p.exec.resumed)-resumed)
	}
	v = p.get()
	if v.Checkpoint == nil || v.Checkpoint.OutputCommit != newHead {
		t.Fatalf("checkpoint: %+v", v.Checkpoint)
	}
	for _, ev := range v.Evidence {
		if ev.StageID == "test" && ev.Current {
			t.Fatalf("the Test result must be labelled as covering an earlier revision: %+v", ev)
		}
	}

	// The PR has not been pushed yet: the old head's feedback never counts for
	// the new revision.
	if g := gateOf(t, v); g.Code != pipelineruns.WaitAwaitingPRHead || g.Checkpoint != newHead {
		t.Fatalf("review waits for the repaired head: %+v", g)
	}
	// Once pushed, auto review runs again for the new head and needs fresh evidence.
	p.setPR(newHead)
	p.drive()
	if p.reviews.triggerCount() != 2 {
		t.Fatalf("auto review triggers again on each return: %d", p.reviews.triggerCount())
	}
	p.setRun(reviewRun("rr2", newHead, domain.ReviewRunComplete, domain.VerdictApproved, time.Now()))
	p.drive()
	done := p.get()
	if done.State != "completed" || done.RepairsUsed != 1 {
		t.Fatalf("fresh approval for the repaired head completes: %s", brief(done))
	}
	if len(done.Reviews) != 2 || done.Reviews[0].Current || done.Reviews[0].Outcome != "changes_requested" || !done.Reviews[1].Current || done.Reviews[1].ReviewRunID != "rr2" {
		t.Fatalf("review evidence per revision: %+v", done.Reviews)
	}
}

func TestReviewRetryWaitsForAManualTriggerWhenAutoReviewIsOff(t *testing.T) {
	p := newFullPipeline(t, false, "build-test-review")
	head := p.toReviewStage()
	p.setPR(head)
	p.setRun(changesRequested("rr1", head, "Fix it"))
	p.drive() // routes to Build
	p.drive() // resumes the worker
	_, newHead := p.builderSubmits("r1")
	p.drive()
	p.setPR(newHead)
	p.drive()
	g := gateOf(t, p.get())
	if g.State != "waiting" || g.Code != pipelineruns.WaitAwaitingManual || !g.Manual {
		t.Fatalf("the retry waits for a manual trigger: %+v", g)
	}
	if p.reviews.triggerCount() != 0 {
		t.Fatal("the pipeline must not start the reviewer when Auto review is off")
	}
}

func TestReviewAndTestRepairsShareOneBudget(t *testing.T) {
	p := newFullPipeline(t, true, "tight-budget")
	run := p.start()
	p.buildAndSubmit(run)
	p.drive()

	// Repair 1 comes from Test.
	res := p.testerSubmits("d1", "production_defect", defectReport("Add overflows"))
	if res.Run.RepairsUsed != 1 || res.Run.RepairsRemaining != 1 {
		t.Fatalf("test defect: %s", brief(res.Run))
	}
	p.drive()
	p.builderSubmits("t1")
	p.drive() // Tester resumes
	p.testerSubmits("pass", "succeeded", passReport())
	p.drive() // Review active
	head := p.get().Attempts[len(p.get().Attempts)-1].InputCommit
	p.setPR(head)

	// Repair 2 comes from Review and spends the last attempt.
	p.setRun(changesRequested("rr1", head, "first review"))
	p.drive()
	if v := p.get(); v.RepairsUsed != 2 || v.RepairsRemaining != 0 || v.State != "running" || v.CurrentStageID != "build" {
		t.Fatalf("review repair after a test repair: %s", brief(v))
	}
	p.drive()
	_, head2 := p.builderSubmits("r1")
	p.drive()
	p.setPR(head2)
	p.setRun(changesRequested("rr2", head2, "second review"))
	p.drive()
	v := p.get()
	if v.State != "paused" || v.PauseReason != "repair_budget_exhausted" || v.RepairsUsed != 2 || len(v.Repairs) != 2 {
		t.Fatalf("mixed Test/Review failures share the budget and pause before a third return: %s", brief(v))
	}
	for _, a := range v.Attempts {
		if a.StageID == "build" && a.State == "handoff" {
			t.Fatal("no extra Build attempt may exist once the budget is spent")
		}
	}
	before := len(p.exec.resumed)
	p.drive()
	if len(p.exec.resumed) != before || p.get().RepairsUsed != 2 {
		t.Fatal("exhaustion is never silently extended")
	}
}

func TestReviewFeedbackIsCountedOnceUnderDuplicateAndConcurrentDelivery(t *testing.T) {
	p := newFullPipeline(t, false, "build-test-review")
	head := p.toReviewStage()
	p.setPR(head)
	p.setRun(changesRequested("rr1", head, "Fix it"))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = p.svc.DriveHandoffs(context.Background())
		}()
	}
	wg.Wait()
	for i := 0; i < 3; i++ {
		p.drive()
	}
	v := p.get()
	if v.RepairsUsed != 1 || len(v.Repairs) != 1 {
		t.Fatalf("one repair however the feedback arrives: %s %+v", brief(v), v.Repairs)
	}
	if len(p.exec.resumed) != 1 {
		t.Fatalf("the worker receives the feedback once: %d resumes", len(p.exec.resumed))
	}

	// A restarted service over the same store neither re-counts nor re-delivers.
	again := pipelineruns.New(pipelineruns.Deps{Store: p.store, Messenger: p.messenger, Executor: p.exec, Reviews: p.reviews})
	if err := again.ReconcileAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := again.DriveHandoffs(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v := p.get(); v.RepairsUsed != 1 || len(p.exec.resumed) != 1 {
		t.Fatalf("restart must not spend the budget again: %s resumes=%d", brief(v), len(p.exec.resumed))
	}
}

func TestStaleReviewFeedbackForAnOlderHeadIsIgnored(t *testing.T) {
	p := newFullPipeline(t, false, "build-test-review")
	head := p.toReviewStage()
	p.setPR(head)
	p.setRun(changesRequested("old", "1111111111111111111111111111111111111111", "about an older head"))
	p.drive()
	v := p.get()
	if v.State != "running" || v.RepairsUsed != 0 || v.CurrentStageID != "review" {
		t.Fatalf("feedback for another head must not spend repairs: %s", brief(v))
	}
}

func TestReviewFeedbackRepairNeedsALiveOriginalConversation(t *testing.T) {
	p := newFullPipeline(t, false, "build-test-review")
	head := p.toReviewStage()
	p.setPR(head)
	p.setRun(changesRequested("rr1", head, "Fix it"))
	p.exec.resumeErr = fmt.Errorf("%w: controller is gone", ports.ErrPipelineResumeUnsafe)
	p.drive() // routes to Build
	p.drive() // attempts to resume
	v := p.get()
	if v.State != "paused" || v.PauseReason != "recovery_decision_required" {
		t.Fatalf("a dead original conversation asks for a decision instead of starting a fresh one: %s", brief(v))
	}
	if len(p.exec.starts) != 1 {
		t.Fatalf("no fresh conversation may be started: %d", len(p.exec.starts))
	}
}
