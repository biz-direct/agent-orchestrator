package pipeline

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const validTester = `version: 1
id: tester
description: Writes and improves tests
instructions: |
  You are the Tester. Only change tests.
harness: claude-code
model: opus
allowedPaths:
  - "**/*_test.go"
  - "test/**"
validation:
  - id: unit
    command: go test ./...
    timeoutSeconds: 300
  - id: lint
    command: golangci-lint run
    required: false
`

const validWorkflow = `version: 1
id: build-test-review
description: Build, then Test, then Review
repairBudget: 3
stages:
  - id: build
    kind: build
  - id: test
    kind: specialist
    profile: tester
    repairTo: build
  - id: review
    kind: review
    repairTo: build
`

func diagText(ds []Diagnostic) string {
	var parts []string
	for _, d := range ds {
		parts = append(parts, d.Field+": "+d.Message)
	}
	return strings.Join(parts, "\n")
}

func TestDiscoverEmptyRepository(t *testing.T) {
	cat := Discover(t.TempDir())
	if len(cat.Profiles) != 0 || len(cat.Workflows) != 0 || len(cat.Diagnostics) != 0 {
		t.Fatalf("empty repo should yield an empty catalog: %+v", cat)
	}
}

func TestDiscoverValidProfileAndWorkflow(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ProfilesDir+"/tester.yaml", validTester)
	writeFile(t, root, WorkflowsDir+"/build-test-review.yaml", validWorkflow)

	cat := Discover(root)
	if len(cat.Profiles) != 1 || !cat.Profiles[0].Valid {
		t.Fatalf("profile should be valid: %+v", cat.Profiles)
	}
	p := cat.Profiles[0].Profile
	if p.ID != "tester" || p.Harness != "claude-code" || p.Model != "opus" {
		t.Fatalf("unexpected profile: %+v", p)
	}
	if p.InstructionsSource != "inline" || len(p.InstructionsSHA256) != 64 || !strings.Contains(p.InstructionsText, "Only change tests") {
		t.Fatalf("instructions provenance missing: %+v", p)
	}
	if len(p.AllowedPaths) != 2 || len(p.Validation) != 2 {
		t.Fatalf("paths/validation not parsed: %+v", p)
	}
	if !p.Validation[0].Required || p.Validation[0].TimeoutSeconds != 300 {
		t.Fatalf("unit command: %+v", p.Validation[0])
	}
	if p.Validation[1].Required || p.Validation[1].TimeoutSeconds != DefaultValidationTimeoutSeconds {
		t.Fatalf("lint command: %+v", p.Validation[1])
	}

	if len(cat.Workflows) != 1 || !cat.Workflows[0].Valid {
		t.Fatalf("workflow should be valid: %s", diagText(cat.Workflows[0].Diagnostics))
	}
	w := cat.Workflows[0].Workflow
	if w.RepairBudget != 3 || len(w.Stages) != 3 || w.Stages[1].Profile != "tester" || w.Stages[2].RepairTo != "build" {
		t.Fatalf("unexpected workflow: %+v", w)
	}
	if _, ok := cat.FindWorkflow("build-test-review"); !ok {
		t.Fatal("FindWorkflow failed")
	}
}

func TestDiscoverInstructionsFile(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "docs/tester.md", "Be a careful tester.\n")
	writeFile(t, root, ProfilesDir+"/tester.yaml", strings.Replace(validTester, "instructions: |\n  You are the Tester. Only change tests.\n", "instructionsFile: docs/tester.md\n", 1))
	cat := Discover(root)
	if !cat.Profiles[0].Valid {
		t.Fatalf("should be valid: %s", diagText(cat.Profiles[0].Diagnostics))
	}
	p := cat.Profiles[0].Profile
	if p.InstructionsSource != "docs/tester.md" || p.InstructionsText != "Be a careful tester.\n" {
		t.Fatalf("file instructions not resolved: %+v", p)
	}
}

func TestDiscoverProfileInvalid(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"unsupported version", strings.Replace(validTester, "version: 1", "version: 2", 1), "unsupported version 2"},
		{"missing version", strings.Replace(validTester, "version: 1\n", "", 1), "version is required"},
		{"bad id", strings.Replace(validTester, "id: tester", "id: Tester_One", 1), "must be 1-64 lowercase"},
		{"missing description", strings.Replace(validTester, "description: Writes and improves tests\n", "", 1), "description is required"},
		{"unknown harness", strings.Replace(validTester, "harness: claude-code", "harness: nope", 1), `unknown harness "nope"`},
		{"traversal glob", strings.Replace(validTester, `"test/**"`, `"../outside/**"`, 1), "traversal"},
		{"absolute glob", strings.Replace(validTester, `"test/**"`, `"/etc/**"`, 1), "not absolute"},
		{"git glob", strings.Replace(validTester, `"test/**"`, `".git/**"`, 1), ".git"},
		{"negated glob", strings.Replace(validTester, `"test/**"`, `"!test/**"`, 1), "negated"},
		{"embedded doublestar", strings.Replace(validTester, `"test/**"`, `"te**st/a"`, 1), "whole path segment"},
		{"unknown field", validTester + "extra: true\n", `unknown field "extra"`},
		{"duplicate key", validTester + "id: other\n", "already defined"},
		{"bad timeout", strings.Replace(validTester, "timeoutSeconds: 300", "timeoutSeconds: 99999", 1), "timeoutSeconds"},
		{"empty command", strings.Replace(validTester, "command: go test ./...", `command: "  "`, 1), "command is required"},
		{"dup command id", strings.Replace(validTester, "id: lint", "id: unit", 1), "duplicate validation command id"},
		{"both instructions", validTester + "instructionsFile: README.md\n", "not both"},
		{"alias", "version: 1\nid: tester\ndescription: &d x\ninstructions: *d\n", "anchors and aliases"},
		{"multi doc", validTester + "---\nversion: 1\n", "multiple YAML documents"},
		{"missing file ref", strings.Replace(validTester, "instructions: |\n  You are the Tester. Only change tests.\n", "instructionsFile: docs/missing.md\n", 1), "does not exist"},
		{"file traversal ref", strings.Replace(validTester, "instructions: |\n  You are the Tester. Only change tests.\n", "instructionsFile: ../secret.md\n", 1), "traversal"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, root, ProfilesDir+"/tester.yaml", tc.body)
			cat := Discover(root)
			if len(cat.Profiles) != 1 {
				t.Fatalf("want one profile entry, got %d", len(cat.Profiles))
			}
			e := cat.Profiles[0]
			if e.Valid || e.Profile != nil {
				t.Fatal("invalid profile must not be valid")
			}
			if !strings.Contains(diagText(e.Diagnostics), tc.want) {
				t.Fatalf("diagnostics %q should contain %q", diagText(e.Diagnostics), tc.want)
			}
			if e.Diagnostics[0].File != ProfilesDir+"/tester.yaml" {
				t.Fatalf("diagnostic must name the file: %+v", e.Diagnostics[0])
			}
		})
	}
}

func TestDiscoverWorkflowInvalid(t *testing.T) {
	base := func(stages string) string {
		return "version: 1\nid: wf\ndescription: d\nstages:\n" + stages
	}
	tests := []struct {
		name string
		body string
		want string
	}{
		{"unknown profile", base("  - {id: build, kind: build}\n  - {id: test, kind: specialist, profile: ghost}\n"), `unknown profile "ghost"`},
		{"invalid profile ref", base("  - {id: build, kind: build}\n  - {id: test, kind: specialist, profile: broken}\n"), `profile "broken" is invalid`},
		{"unsupported kind", base("  - {id: build, kind: build}\n  - {id: x, kind: deploy}\n"), "unsupported stage kind"},
		{"terminal kind", base("  - {id: build, kind: build}\n  - {id: x, kind: terminal}\n"), "Terminal worker stages are not supported"},
		{"parallel", base("  - {id: build, kind: build}\n  - id: test\n    kind: specialist\n    profile: tester\n    parallel: true\n"), "graph feature"},
		{"depends on", base("  - {id: build, kind: build}\n  - id: test\n    kind: specialist\n    profile: tester\n    dependsOn: [build]\n"), "graph feature"},
		{"duplicate stage", base("  - {id: build, kind: build}\n  - {id: build, kind: specialist, profile: tester}\n"), "duplicate stage id"},
		{"build not first", base("  - {id: test, kind: specialist, profile: tester}\n  - {id: build, kind: build}\n"), "must be first"},
		{"no build", base("  - {id: test, kind: specialist, profile: tester}\n"), "exactly one build"},
		{"two builds", base("  - {id: build, kind: build}\n  - {id: b2, kind: build}\n"), "must be first"},
		{"review not last", base("  - {id: build, kind: build}\n  - {id: review, kind: review}\n  - {id: test, kind: specialist, profile: tester}\n"), "must be last"},
		{"build with profile", base("  - {id: build, kind: build, profile: tester}\n"), "cannot reference a profile"},
		{"specialist without profile", base("  - {id: build, kind: build}\n  - {id: test, kind: specialist}\n"), "requires a profile"},
		{"repair unknown", base("  - {id: build, kind: build}\n  - {id: test, kind: specialist, profile: tester, repairTo: ghost}\n"), "unknown stage"},
		{"repair forward", base("  - {id: build, kind: build}\n  - {id: test, kind: specialist, profile: tester, repairTo: review}\n  - {id: review, kind: review}\n"), "must be an earlier stage"},
		{"repair non-build", base("  - {id: build, kind: build}\n  - {id: test, kind: specialist, profile: tester}\n  - {id: review, kind: review, repairTo: test}\n"), "must be the build stage"},
		{"budget too high", "version: 1\nid: wf\ndescription: d\nrepairBudget: 99\nstages:\n  - {id: build, kind: build}\n", "repairBudget"},
		{"no stages", "version: 1\nid: wf\ndescription: d\n", "at least one stage"},
		{"bad version", "version: 7\nid: wf\ndescription: d\nstages:\n  - {id: build, kind: build}\n", "unsupported version 7"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, root, ProfilesDir+"/tester.yaml", validTester)
			writeFile(t, root, ProfilesDir+"/broken.yaml", "version: 9\nid: broken\n")
			writeFile(t, root, WorkflowsDir+"/wf.yaml", tc.body)
			cat := Discover(root)
			e, ok := cat.FindWorkflow("wf")
			if !ok {
				t.Fatalf("workflow entry missing: %+v", cat.Workflows)
			}
			if e.Valid {
				t.Fatal("expected invalid workflow")
			}
			if !strings.Contains(diagText(e.Diagnostics), tc.want) {
				t.Fatalf("diagnostics %q should contain %q", diagText(e.Diagnostics), tc.want)
			}
		})
	}
}

func TestDiscoverDuplicateIDsAcrossFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ProfilesDir+"/a.yaml", validTester)
	writeFile(t, root, ProfilesDir+"/b.yml", validTester)
	cat := Discover(root)
	if len(cat.Profiles) != 2 {
		t.Fatalf("want 2 entries, got %d", len(cat.Profiles))
	}
	for _, e := range cat.Profiles {
		if e.Valid || !strings.Contains(diagText(e.Diagnostics), "duplicate profile id") {
			t.Fatalf("both duplicates must be flagged: %+v", e)
		}
	}
}

func TestDiscoverRejectsSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, outside, "evil.yaml", validTester)
	if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(ProfilesDir)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "evil.yaml"), filepath.Join(root, filepath.FromSlash(ProfilesDir), "evil.yaml")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	cat := Discover(root)
	if len(cat.Profiles) != 1 || cat.Profiles[0].Valid {
		t.Fatalf("symlinked definition must be rejected: %+v", cat.Profiles)
	}
	if !strings.Contains(diagText(cat.Profiles[0].Diagnostics), "outside the project folder") &&
		!strings.Contains(diagText(cat.Profiles[0].Diagnostics), "symlink") {
		t.Fatalf("unexpected diagnostics: %s", diagText(cat.Profiles[0].Diagnostics))
	}
}

func TestDiscoverRejectsSymlinkedInstructions(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, outside, "secret.md", "secret")
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(root, "link.md")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	writeFile(t, root, ProfilesDir+"/tester.yaml", strings.Replace(validTester, "instructions: |\n  You are the Tester. Only change tests.\n", "instructionsFile: link.md\n", 1))
	cat := Discover(root)
	if cat.Profiles[0].Valid {
		t.Fatal("symlinked instruction file must be rejected")
	}
}

func TestDiscoverOversizedDefinition(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ProfilesDir+"/big.yaml", "version: 1\nid: big\ndescription: "+strings.Repeat("x", MaxDefinitionBytes))
	cat := Discover(root)
	if cat.Profiles[0].Valid || !strings.Contains(diagText(cat.Profiles[0].Diagnostics), "too large") {
		t.Fatalf("oversized file must be rejected: %+v", cat.Profiles[0])
	}
}

func TestDiscoverMissingRoot(t *testing.T) {
	cat := Discover(filepath.Join(t.TempDir(), "nope"))
	if len(cat.Diagnostics) == 0 {
		t.Fatal("missing project folder should yield a catalog diagnostic")
	}
}

func TestDiscoverIgnoresNonYAMLFiles(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, ProfilesDir+"/README.md", "notes")
	cat := Discover(root)
	if len(cat.Profiles) != 0 {
		t.Fatalf("non-YAML files are not definitions: %+v", cat.Profiles)
	}
}

func TestUnsupportedExecutionReasonBlocksEveryWorkflow(t *testing.T) {
	if UnsupportedExecutionReason(Workflow{ID: "x"}) == "" {
		t.Fatal("discovery slice must not mark workflows executable")
	}
}
