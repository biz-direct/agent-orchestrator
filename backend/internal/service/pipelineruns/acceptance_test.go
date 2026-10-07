package pipelineruns_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/pipelineruns"
)

// These two stories are the release acceptance for the default
// Build → Test → Review workflow: they drive the real service and SQLite store
// end to end, faking only the executors and the pull request / review facts.

func TestDefaultPipelineHappyPathWithAutoReview(t *testing.T) {
	p := newFullPipeline(t, true, "build-test-review")
	ctx := context.Background()
	run := p.start()

	// Build: the regular worker's own commit is the first checkpoint.
	_, buildHead := p.buildAndSubmit(run)
	if v := p.get(); v.Checkpoint == nil || v.Checkpoint.OutputCommit != buildHead || v.Checkpoint.StageID != "build" {
		t.Fatalf("build checkpoint: %+v", v.Checkpoint)
	}

	// Test: an attached specialist in the same worktree; its result is bound to its commit.
	p.drive()
	res := p.testerSubmits("pass", "succeeded", passReport())
	if !res.Accepted || res.Run.CurrentStageID != "review" {
		t.Fatalf("tester accepted: %s", brief(res.Run))
	}
	testHead := res.Run.Checkpoint.OutputCommit
	if testHead == buildHead {
		t.Fatal("the Tester's commit is its own revision")
	}
	var testEvidence int
	for _, ev := range res.Run.Evidence {
		if ev.StageID == "test" {
			testEvidence++
			if ev.Revision != testHead || !ev.Current {
				t.Fatalf("Test evidence covers its own commit: %+v", ev)
			}
		}
	}
	if testEvidence != 1 {
		t.Fatalf("evidence: %+v", res.Run.Evidence)
	}

	// Review: Auto review starts the built-in reviewer for exactly this head.
	p.drive()
	p.setPR(testHead)
	p.drive()
	if p.reviews.triggerCount() != 1 {
		t.Fatalf("auto review: %d", p.reviews.triggerCount())
	}
	g := gateOf(t, p.get())
	if g.State != "waiting" || g.Code != "review_running" && g.Code != "awaiting_auto_review" {
		t.Fatalf("waiting for the reviewer: %+v", g)
	}
	p.setRun(reviewRun("rr1", testHead, domain.ReviewRunComplete, domain.VerdictApproved, time.Now()))
	p.drive()
	done := p.get()
	if done.State != "completed" || done.RepairsUsed != 0 {
		t.Fatalf("approved review and passing CI complete the run: %s", brief(done))
	}
	if len(done.Reviews) != 1 || done.Reviews[0].HeadSHA != testHead || !done.Reviews[0].Current {
		t.Fatalf("review evidence: %+v", done.Reviews)
	}
	// One task, one worktree, one branch: only the worker exists as a task, and it
	// owns the task again.
	all, _ := p.store.ListAllSessions(ctx)
	if len(all) != 1 || all[0].ID != p.sessionID || !p.admitted(p.sessionID) {
		t.Fatalf("sessions=%d worker admitted=%v", len(all), p.admitted(p.sessionID))
	}
}

func TestDefaultPipelineTestRepairThenReviewRepairThatSkipsTheTesterWithAManualReviewWaitAndARestart(t *testing.T) {
	p := newFullPipeline(t, false, "build-test-review") // Auto review off: manual waits
	run := p.start()
	p.buildAndSubmit(run)
	p.drive()

	// 1. Test finds a production defect: back to the ORIGINAL worker, then Test again.
	res := p.testerSubmits("d1", "production_defect", defectReport("Add overflows"))
	if res.Run.RepairsUsed != 1 || res.Run.CurrentStageID != "build" {
		t.Fatalf("test repair: %s", brief(res.Run))
	}
	p.drive()
	p.builderSubmits("t1")
	p.drive()
	if v := p.get(); v.CurrentStageID != "test" || v.Attempts[len(v.Attempts)-1].AttemptNo != 2 {
		t.Fatalf("Test runs again: %s", brief(v))
	}
	pass := p.testerSubmits("pass", "succeeded", passReport())
	if pass.Run.CurrentStageID != "review" {
		t.Fatalf("%s", brief(pass.Run))
	}
	testedHead := pass.Run.Checkpoint.OutputCommit

	// A restart between stages recovers without replaying anything.
	p.restart()
	p.drive()
	v := p.get()
	if v.Attempts[len(v.Attempts)-1].StageID != "review" || v.Attempts[len(v.Attempts)-1].State != "active" {
		t.Fatalf("review is active after restart: %s", brief(v))
	}

	// 2. Auto review is off: the run waits for a person, keeping manual controls.
	p.setPR(testedHead)
	p.drive()
	if g := gateOf(t, p.get()); g.Code != "awaiting_manual_review" || !g.Manual {
		t.Fatalf("manual wait: %+v", g)
	}
	if p.reviews.triggerCount() != 0 {
		t.Fatal("the pipeline never starts the reviewer when Auto review is off")
	}

	// 3. The manual review requests changes: Review repair skips the Tester.
	testerResumesBefore := len(p.exec.resumed)
	p.setRun(changesRequested("rr1", testedHead, "Handle the empty list"))
	p.drive()
	p.drive() // resume the original worker
	if v := p.get(); v.CurrentStageID != "build" || v.RepairsUsed != 2 || v.Repairs[1].Kind != "review_feedback" {
		t.Fatalf("review repair shares the budget: %s", brief(v))
	}
	_, repairedHead := p.builderSubmits("r1")
	p.drive()
	v = p.get()
	if v.CurrentStageID != "review" {
		t.Fatalf("Build returns straight to Review: %s", brief(v))
	}
	for _, a := range v.Attempts[len(v.Attempts)-1:] {
		if a.StageID == "test" {
			t.Fatal("the Tester must not run again for a review-driven repair")
		}
	}
	// Only the original worker was resumed for the repair.
	if got := len(p.exec.resumed) - testerResumesBefore; got != 1 {
		t.Fatalf("exactly one resume (the worker): %d", got)
	}
	// Earlier Test evidence is retained but labelled as an earlier revision.
	for _, ev := range v.Evidence {
		if ev.StageID == "test" && ev.Current {
			t.Fatalf("Test evidence must not claim to cover the repaired head: %+v", ev)
		}
	}

	// 4. Review waits for a manual trigger again, needs fresh facts for the new head,
	// and an operational failure pauses instead of retrying behind the user's back.
	p.setPR(repairedHead)
	p.drive()
	if g := gateOf(t, p.get()); g.Code != "awaiting_manual_review" || g.Checkpoint != repairedHead {
		t.Fatalf("manual wait on retry: %+v", g)
	}
	p.setRun(reviewRun("rr2", repairedHead, domain.ReviewRunFailed, domain.VerdictNone, time.Now()))
	p.drive()
	paused := p.get()
	if paused.State != "paused" || paused.PauseReason != "review_operational" || paused.RepairsUsed != 2 {
		t.Fatalf("operational pause spends nothing: %s", brief(paused))
	}

	// 5. A person fixes the reviewer and resumes; a fresh approval for the head completes.
	p.mustControl(paused, "resume", "user")
	p.drive()
	p.setRun(reviewRun("rr3", repairedHead, domain.ReviewRunComplete, domain.VerdictApproved, time.Now().Add(time.Minute)))
	p.drive()
	end := p.get()
	if end.State != "completed" || end.RepairsUsed != 2 {
		t.Fatalf("completion: %s", brief(end))
	}
	if len(end.Reviews) != 3 || !end.Reviews[len(end.Reviews)-1].Current || end.Reviews[0].Current {
		t.Fatalf("review evidence is per revision: %+v", end.Reviews)
	}
	if !strings.HasPrefix(end.Reviews[len(end.Reviews)-1].HeadSHA, repairedHead[:7]) {
		t.Fatalf("the final approval is for the repaired head: %+v", end.Reviews)
	}
}

func TestDefaultPipelineThreeAttemptExhaustionNeedsAHumanThenFinishes(t *testing.T) {
	p := newFullPipeline(t, false, "tight-budget") // budget of two
	run := p.start()
	p.buildAndSubmit(run)
	p.drive()
	p.testerSubmits("d1", "production_defect", defectReport("first"))
	p.drive()
	p.builderSubmits("b1")
	p.drive()
	p.testerSubmits("d2", "production_defect", defectReport("second"))
	p.drive()
	p.builderSubmits("b2")
	p.drive()
	p.testerSubmits("d3", "production_defect", defectReport("third"))
	v := p.get()
	if v.State != "paused" || v.PauseReason != "repair_budget_exhausted" || v.RepairsUsed != 2 || !v.Control.NeedsRepairAuthorization {
		t.Fatalf("exhaustion pauses before another return: %s", brief(v))
	}
	if _, err := p.control(v, "resume", "user"); err == nil || code(t, err) != "PIPELINE_REPAIR_AUTHORIZATION_REQUIRED" {
		t.Fatalf("resume never extends the budget: %v", err)
	}
	p.mustControl(v, "authorize_repairs", "user", func(in *pipelineruns.ControlInput) { in.AdditionalRepairs = 1; in.RequestKey = "ok" })
	p.mustControl(v, "resume", "user")
	p.drive()
	p.builderSubmits("b3")
	p.drive()
	p.testerSubmits("pass", "succeeded", passReport())
	if got := p.get(); got.CurrentStageID != "review" || got.RepairsUsed != 3 || got.RepairBudget != 3 {
		t.Fatalf("the authorized repair worked and the pipeline reached Review: %s", brief(got))
	}
}
