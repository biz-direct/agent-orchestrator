package pipeline

import (
	"os"
	"path/filepath"
	"testing"
)

// The shipped examples are copied by users; they must always validate and run.
func TestShippedExamplesAreValidAndExecutable(t *testing.T) {
	src := filepath.Join("..", "..", "..", "examples", "pipelines")
	repo := t.TempDir()
	copyFile := func(from, to string) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(src, from))
		if err != nil {
			t.Fatal(err)
		}
		dest := filepath.Join(repo, to)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dest, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	copyFile("profiles/tester.yaml", ".ao/pipelines/profiles/tester.yaml")
	copyFile("workflows/build-test-review.yaml", ".ao/pipelines/workflows/build-test-review.yaml")
	copyFile("workflows/build-review.yaml", ".ao/pipelines/workflows/build-review.yaml")
	copyFile("instructions/tester.md", "docs/ai/tester.md")

	cat := Discover(repo)
	if len(cat.Diagnostics) != 0 {
		t.Fatalf("catalog diagnostics: %+v", cat.Diagnostics)
	}
	for _, p := range cat.Profiles {
		if !p.Valid || len(p.Diagnostics) != 0 {
			t.Fatalf("profile %s: %+v", p.ID, p.Diagnostics)
		}
	}
	if len(cat.Workflows) != 2 {
		t.Fatalf("workflows: %+v", cat.Workflows)
	}
	for _, w := range cat.Workflows {
		if !w.Valid || w.Workflow == nil || len(w.Diagnostics) != 0 {
			t.Fatalf("workflow %s: %+v", w.ID, w.Diagnostics)
		}
		if reason := UnsupportedExecutionReason(*w.Workflow); reason != "" {
			t.Fatalf("workflow %s is not executable: %s", w.ID, reason)
		}
	}
	full, _ := cat.FindWorkflow("build-test-review")
	if full.Workflow == nil || len(full.Workflow.Stages) != 3 || full.Workflow.Stages[2].Kind != StageReview || full.Workflow.RepairBudget != 3 {
		t.Fatalf("the default workflow shape: %+v", full.Workflow)
	}
	tester, ok := cat.FindProfile("tester")
	if !ok || tester.Profile == nil || tester.Profile.InstructionsSource != "docs/ai/tester.md" || len(tester.Profile.Validation) != 2 || tester.Profile.Validation[1].Required {
		t.Fatalf("tester profile: %+v", tester.Profile)
	}
}
