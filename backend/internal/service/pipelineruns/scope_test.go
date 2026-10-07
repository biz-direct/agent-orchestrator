package pipelineruns_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/pipelineruns"
)

const scopedTesterProfile = `version: 1
id: tester
description: Writes and improves tests
instructions: You are the Tester. Only change tests.
harness: claude-code
allowedPaths:
  - "**/*_test.go"
  - "test/**"
`

// specialistStage is a staged fixture whose Build stage is complete and whose
// Tester is the active executor, in a repo with production code to protect.
type specialistStage struct {
	*staged
	spec    domain.SessionID
	view    pipelineruns.RunView
	attempt pipelineruns.AttemptView
}

func newSpecialistStage(t *testing.T) *specialistStage {
	t.Helper()
	f := newFixture(t, map[string]string{
		".ao/pipelines/workflows/build-test.yaml": buildTestWorkflow,
		".ao/pipelines/profiles/tester.yaml":      scopedTesterProfile,
		"src/main.go":                             "package src\n",
		"src/main_test.go":                        "package src\n",
	})
	exec := &fakeExecutor{store: f.store, repo: f.repo}
	f.svc = pipelineruns.New(pipelineruns.Deps{Store: f.store, Messenger: f.messenger, Executor: exec})
	s := &staged{fixture: f, exec: exec, gate: pipelineruns.NewStoreGate(f.store, nil)}
	run := s.startBuildTest()
	s.buildAndSubmit(run)
	if err := s.svc.DriveHandoffs(context.Background()); err != nil {
		t.Fatal(err)
	}
	view := s.get()
	att := view.Attempts[1]
	return &specialistStage{staged: s, spec: domain.SessionID(att.ConversationSessionID), view: view, attempt: att}
}

func passReport() *pipelineruns.StageReport {
	return &pipelineruns.StageReport{
		Findings: []pipelineruns.ReportFinding{{Criterion: "feature has tests", Status: "met", Evidence: "TestFeature passes"}},
		Commands: []pipelineruns.ReportCommand{{Command: "go test ./...", ExitCode: 0, Summary: "ok"}},
	}
}

func (s *specialistStage) commit(msg string, edits func()) string {
	s.t.Helper()
	edits()
	git(s.t, s.repo, "add", "-A")
	git(s.t, s.repo, "commit", "-q", "-m", msg)
	return git(s.t, s.repo, "rev-parse", "HEAD")
}

func (s *specialistStage) submitSpec(outcome, head, key string, report *pipelineruns.StageReport) (pipelineruns.SubmitResult, error) {
	return s.svc.Submit(context.Background(), pipelineruns.SubmitInput{
		SessionID: s.spec, RunID: s.view.ID, AttemptID: s.attempt.ID, ControllerGeneration: s.attempt.ControllerGeneration,
		IdempotencyKey: key, Outcome: outcome, ExpectedInputCommit: s.attempt.InputCommit, OutputCommit: head,
		Summary: "tester summary", Report: report,
	})
}

func TestSpecialistMayChangeTestsInsideItsScopeAndEvidenceIsRevisionBound(t *testing.T) {
	s := newSpecialistStage(t)
	head := s.commit("tests", func() {
		writeFile(t, s.repo, "src/feature_test.go", "package src\n")
		writeFile(t, s.repo, "test/e2e/flow.txt", "flow\n")
		if err := os.Remove(filepath.Join(s.repo, "src", "main_test.go")); err != nil { // deleting a test is in scope
			t.Fatal(err)
		}
	})
	res, err := s.submitSpec("succeeded", head, "k1", passReport())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Accepted || res.Run.State != "completed" {
		t.Fatalf("test-only changes must be accepted: %+v", res.Run)
	}
	if len(res.Run.Evidence) != 1 {
		t.Fatalf("evidence: %+v", res.Run.Evidence)
	}
	ev := res.Run.Evidence[0]
	if ev.Revision != head || ev.StageID != "test" || ev.Profile != "tester" || ev.Outcome != "succeeded" || ev.Findings != 1 || ev.Commands != 1 {
		t.Fatalf("test evidence must name the exact revision it covers: %+v", ev)
	}
	if res.Attempt.Report == nil || len(res.Attempt.Report.Findings) != 1 {
		t.Fatalf("the structured report is retained: %+v", res.Attempt)
	}
	if got := res.Run.Stages[1].AllowedPaths; len(got) != 2 {
		t.Fatalf("the stage shows the scope it was held to: %v", got)
	}
}

func TestSpecialistProductionEditsAreRejectedPreservedAndFixableByRevert(t *testing.T) {
	s := newSpecialistStage(t)
	head := s.commit("sneaky fix", func() {
		writeFile(t, s.repo, "src/feature_test.go", "package src\n")
		writeFile(t, s.repo, "src/main.go", "package src\n// changed\n")
	})
	_, err := s.submitSpec("succeeded", head, "k1", passReport())
	if code(t, err) != "PIPELINE_SCOPE_VIOLATION" {
		t.Fatalf("production edit: %v", err)
	}
	if !strings.Contains(err.Error(), "hand-off constraint, not a sandbox") {
		t.Fatalf("the rejection must say validation is not a sandbox: %v", err)
	}
	view := s.get()
	if view.State != "running" || view.LastRejection == nil || view.LastRejection.Code != "PIPELINE_SCOPE_VIOLATION" {
		t.Fatalf("run: %+v", view)
	}
	paths := strings.Join(view.LastRejection.Paths, "\n")
	if !strings.Contains(paths, "src/main.go") || strings.Contains(paths, "feature_test.go") {
		t.Fatalf("only offending paths are listed: %v", view.LastRejection.Paths)
	}
	if body, _ := os.ReadFile(filepath.Join(s.repo, "src", "main.go")); !strings.Contains(string(body), "changed") {
		t.Fatal("the offending commit and files are preserved, never reset or amended")
	}
	if git(t, s.repo, "rev-parse", "HEAD") != head {
		t.Fatal("HEAD must be untouched")
	}

	// The net diff is what counts: reverting the production edit makes it in scope.
	fixed := s.commit("revert prod edit", func() { writeFile(t, s.repo, "src/main.go", "package src\n") })
	res, err := s.submitSpec("succeeded", fixed, "k2", passReport())
	if err != nil || !res.Accepted {
		t.Fatalf("after reverting the out-of-scope change: %+v err=%v", res, err)
	}
	// Rejection evidence survives acceptance.
	found := false
	for _, e := range res.Run.Events {
		if e.Kind == "submission_rejected" && strings.Contains(e.Message, "outside") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the rejected attempt must stay in the retained history: %+v", res.Run.Events)
	}
}

func TestScopeCoversDeletionsRenamesAndEveryCommitOfTheStage(t *testing.T) {
	cases := map[string]struct {
		edits func(t *testing.T, s *specialistStage)
		want  string // substring expected in the violation list ("" = accepted)
	}{
		"deleting production code": {
			edits: func(t *testing.T, s *specialistStage) {
				if err := os.Remove(filepath.Join(s.repo, "src", "main.go")); err != nil {
					t.Fatal(err)
				}
			},
			want: "src/main.go: deleted path is outside",
		},
		"renaming a test into production code": {
			edits: func(t *testing.T, s *specialistStage) { git(t, s.repo, "mv", "src/main_test.go", "src/renamed.go") },
			want:  "src/renamed.go",
		},
		"renaming production code into the test tree": {
			edits: func(t *testing.T, s *specialistStage) {
				if err := os.MkdirAll(filepath.Join(s.repo, "test"), 0o755); err != nil {
					t.Fatal(err)
				}
				git(t, s.repo, "mv", "src/main.go", "test/main.go")
			},
			want: "src/main.go: deleted path is outside",
		},
		"renaming a test within scope": {
			edits: func(t *testing.T, s *specialistStage) { git(t, s.repo, "mv", "src/main_test.go", "src/other_test.go") },
		},
		"a new file at the repository root": {
			edits: func(t *testing.T, s *specialistStage) { writeFile(t, s.repo, "Makefile", "all:\n") },
			want:  "Makefile",
		},
		"changing a hidden dotfile": {
			edits: func(t *testing.T, s *specialistStage) { writeFile(t, s.repo, ".github/workflows/ci.yml", "x\n") },
			want:  ".github/workflows/ci.yml",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := newSpecialistStage(t)
			head := s.commit("stage work", func() { tc.edits(t, s) })
			res, err := s.submitSpec("succeeded", head, "k", passReport())
			if tc.want == "" {
				if err != nil || !res.Accepted {
					t.Fatalf("in-scope change rejected: %v", err)
				}
				return
			}
			if code(t, err) != "PIPELINE_SCOPE_VIOLATION" {
				t.Fatalf("want a scope violation, got %v", err)
			}
			if got := strings.Join(s.get().LastRejection.Paths, "\n"); !strings.Contains(got, tc.want) {
				t.Fatalf("violations %q should contain %q", got, tc.want)
			}
		})
	}
}

func TestScopeJudgesTheWholeStageDiffNotJustTheLastCommit(t *testing.T) {
	s := newSpecialistStage(t)
	s.commit("prod edit early", func() { writeFile(t, s.repo, "src/main.go", "package src\n// early\n") })
	head := s.commit("tests later", func() { writeFile(t, s.repo, "src/late_test.go", "package src\n") })
	if _, err := s.submitSpec("succeeded", head, "k", passReport()); code(t, err) != "PIPELINE_SCOPE_VIOLATION" {
		t.Fatalf("an out-of-scope commit earlier in the stage still counts: %v", err)
	}
}

func TestScopeSymlinkEdgeCases(t *testing.T) {
	cases := map[string]struct {
		target string
		link   string
		want   string
	}{
		"symlink into production code": {link: "test/link", target: "../src/main.go", want: "points outside the profile's allowed paths"},
		"symlink escaping the repo":    {link: "test/escape", target: "../../outside", want: "escapes the repository"},
		"absolute symlink":             {link: "test/abs", target: "/etc/passwd", want: "absolute"},
		"symlink inside the scope":     {link: "test/ok", target: "fixtures/data.txt"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := newSpecialistStage(t)
			head := s.commit("link", func() {
				if err := os.MkdirAll(filepath.Join(s.repo, "test", "fixtures"), 0o755); err != nil {
					t.Fatal(err)
				}
				writeFile(t, s.repo, "test/fixtures/data.txt", "x\n")
				if err := os.Symlink(tc.target, filepath.Join(s.repo, filepath.FromSlash(tc.link))); err != nil {
					t.Skip("symlinks unavailable:", err)
				}
			})
			res, err := s.submitSpec("succeeded", head, "k", passReport())
			if tc.want == "" {
				if err != nil || !res.Accepted {
					t.Fatalf("in-scope symlink rejected: %v", err)
				}
				return
			}
			if code(t, err) != "PIPELINE_SCOPE_VIOLATION" {
				t.Fatalf("want a scope violation, got %v", err)
			}
			if got := strings.Join(s.get().LastRejection.Paths, "\n"); !strings.Contains(got, tc.want) {
				t.Fatalf("violations %q should contain %q", got, tc.want)
			}
		})
	}
}

func TestNoChangeValidationPassesWithAReport(t *testing.T) {
	s := newSpecialistStage(t)
	res, err := s.submitSpec("succeeded", s.attempt.InputCommit, "k", passReport())
	if err != nil || !res.Accepted || res.Run.Checkpoint == nil || !res.Run.Checkpoint.NoChange {
		t.Fatalf("a specialist that only validated changes nothing: %+v err=%v", res, err)
	}
}

func TestSpecialistDirtyCheckpointIsPreserved(t *testing.T) {
	s := newSpecialistStage(t)
	writeFile(t, s.repo, "src/uncommitted_test.go", "package src\n")
	_, err := s.submitSpec("succeeded", s.attempt.InputCommit, "k", passReport())
	if code(t, err) != "PIPELINE_DIRTY_WORKSPACE" {
		t.Fatalf("uncommitted test work cannot be handed off: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(s.repo, "src", "uncommitted_test.go")); statErr != nil {
		t.Fatal("dirty work is preserved")
	}
}

func TestProductionDefectIsReportedForBuildNotFixedByTheSpecialist(t *testing.T) {
	s := newSpecialistStage(t)
	head := s.commit("failing test", func() { writeFile(t, s.repo, "src/bug_test.go", "package src\n") })
	report := passReport()
	report.Defects = []pipelineruns.ReportDefect{{Description: "main.Add overflows on large input", Paths: []string{"src/main.go"}}}
	report.Findings[0].Status = "unmet"
	res, err := s.submitSpec("production_defect", head, "k", report)
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted || res.Run.State != "running" || res.Run.CurrentStageID != "build" || res.Run.RepairsUsed != 1 {
		t.Fatalf("a production defect is routed to Build with its repair counted once: %s", brief(res.Run))
	}
	if res.Run.Attempts[2].Feedback == nil || !strings.Contains(res.Run.Attempts[2].Feedback.Defects[0].Description, "main.Add overflows") {
		t.Fatalf("the defect travels to Build as feedback: %+v", res.Run.Attempts[2].Feedback)
	}
	att := res.Run.Attempts[1]
	if att.State != "failed" || att.Outcome != "production_defect" || att.Report == nil || len(att.Report.Defects) != 1 || att.OutputCommit != head {
		t.Fatalf("the report and revision are retained: %+v", att)
	}
	if len(res.Run.Evidence) != 1 || res.Run.Evidence[0].Defects != 1 || res.Run.Evidence[0].Revision != head {
		t.Fatalf("evidence: %+v", res.Run.Evidence)
	}
	// Scope still applies to a defect report: it cannot smuggle in the fix.
	s2 := newSpecialistStage(t)
	fixHead := s2.commit("fixed it myself", func() { writeFile(t, s2.repo, "src/main.go", "package src\n// fixed\n") })
	if _, err := s2.submitSpec("production_defect", fixHead, "k", report); code(t, err) != "PIPELINE_SCOPE_VIOLATION" {
		t.Fatalf("a specialist cannot fix production code under its own authority: %v", err)
	}
}

func TestStageReportContractIsEnforced(t *testing.T) {
	s := newSpecialistStage(t)
	head := s.attempt.InputCommit
	unmet := passReport()
	unmet.Findings[0].Status = "unmet"
	noFindings := &pipelineruns.StageReport{}
	badStatus := passReport()
	badStatus.Findings[0].Status = "mostly"
	defectsOnPass := passReport()
	defectsOnPass.Defects = []pipelineruns.ReportDefect{{Description: "x"}}
	for name, tc := range map[string]struct {
		outcome string
		report  *pipelineruns.StageReport
	}{
		"no report at all":        {"succeeded", nil},
		"empty report":            {"succeeded", noFindings},
		"pass with unmet finding": {"succeeded", unmet},
		"unknown finding status":  {"succeeded", badStatus},
		"pass listing defects":    {"succeeded", defectsOnPass},
		"defect without defects":  {"production_defect", passReport()},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.submitSpec(tc.outcome, head, "k-"+name, tc.report); code(t, err) != "INVALID_PIPELINE_RESULT" {
				t.Fatalf("%s: %v", name, err)
			}
		})
	}
	if view := s.get(); view.State != "running" || view.Stages[1].State != "active" {
		t.Fatalf("invalid reports must not change the run: %+v", view)
	}
}

func TestOnlySpecialistsMayReportProductionDefects(t *testing.T) {
	f := newFixture(t, map[string]string{".ao/pipelines/workflows/build-only.yaml": buildOnlyWorkflow})
	run, _ := f.start("user", nil)
	head := git(t, f.repo, "rev-parse", "HEAD")
	if _, err := f.submit(run, "production_defect", head, "k"); code(t, err) != "INVALID_PIPELINE_RESULT" {
		t.Fatalf("the regular worker cannot report a defect in its own code: %v", err)
	}
}

func TestWorkerDoneReportsAndStaleResultsCannotSatisfyTheGate(t *testing.T) {
	s := newSpecialistStage(t)
	ctx := context.Background()
	// A done report from the worker is a coordination claim, not a stage result.
	now := time.Now().UTC()
	if _, err := s.store.CreateReport(ctx, domain.ReportRecord{
		ID: "rep-1", SessionID: s.sessionID, ProjectID: "proj", State: domain.ReportDone, Note: "all done",
		CreatedAt: now, AvailableAt: now, SettlementDeadline: now.Add(time.Minute), RepeatCount: 1, DeliveryState: domain.ReportPending,
	}); err != nil {
		t.Fatal(err)
	}
	if view := s.get(); view.State != "running" || view.Stages[1].State != "active" || view.Checkpoint == nil || view.Checkpoint.StageID != "build" {
		t.Fatalf("an unsolicited done report must not advance anything: %+v", view)
	}
	// A stale report (wrong input revision) cannot satisfy the gate.
	in := pipelineruns.SubmitInput{
		SessionID: s.spec, RunID: s.view.ID, AttemptID: s.attempt.ID, ControllerGeneration: s.attempt.ControllerGeneration,
		IdempotencyKey: "stale", Outcome: "succeeded", ExpectedInputCommit: strings.Repeat("a", 40), OutputCommit: s.attempt.InputCommit, Report: passReport(),
	}
	if _, err := s.svc.Submit(ctx, in); code(t, err) != "PIPELINE_STALE_REVISION" {
		t.Fatalf("stale input revision: %v", err)
	}
	// After a valid result, a different conflicting result for the attempt is refused.
	if res, err := s.submitSpec("succeeded", s.attempt.InputCommit, "first", passReport()); err != nil || !res.Accepted {
		t.Fatalf("first result: %v", err)
	}
	if _, err := s.submitSpec("production_defect", s.attempt.InputCommit, "second", passReport()); code(t, err) != "PIPELINE_ATTEMPT_STALE" {
		t.Fatalf("a conflicting result after acceptance: %v", err)
	}
}
