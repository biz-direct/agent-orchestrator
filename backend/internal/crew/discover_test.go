package crew

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

func role(id string) string {
	return "version: 1\nid: " + id + "\ndescription: The " + id + "\ninstructions: do the " + id + " job\n"
}

const validTester = `version: 1
id: tester
description: Browser and integration tester
instructions: |
  You are the Tester.
harness: claude-code
model: opus
allowedPaths:
  - "e2e/**"
validation:
  - id: integration
    command: npm run e2e
    timeoutSeconds: 300
  - id: lint
    command: npm run lint
    required: false
`

const validWorkflow = `version: 1
id: full
description: Build, test, document, review
roles: [builder, tester, documenter]
order: [builder, tester, documenter, review]
returns:
  tester: [builder]
  documenter: [tester, builder]
  review: [builder]
loopCap: 4
`

func diagText(ds []Diagnostic) string {
	var parts []string
	for _, d := range ds {
		parts = append(parts, d.File+" "+d.Field+": "+d.Message)
	}
	return strings.Join(parts, "\n")
}

func fixture(t *testing.T, workflow string) Catalog {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, RolesDir+"/builder.yaml", role("builder"))
	writeFile(t, root, RolesDir+"/tester.yaml", validTester)
	writeFile(t, root, RolesDir+"/documenter.yaml", role("documenter"))
	writeFile(t, root, WorkflowsDir+"/wf.yaml", workflow)
	return Discover(root)
}

func wfBody(extra string) string {
	return "version: 1\nid: wf\ndescription: d\n" + extra
}

func mustInvalid(t *testing.T, cat Catalog, want string) {
	t.Helper()
	if len(cat.Workflows) != 1 {
		t.Fatalf("want one workflow, got %d", len(cat.Workflows))
	}
	w := cat.Workflows[0]
	if w.Valid || w.Workflow != nil {
		t.Fatalf("workflow should be invalid: %+v", w)
	}
	if got := diagText(w.Diagnostics); !strings.Contains(got, want) {
		t.Fatalf("diagnostics %q should contain %q", got, want)
	}
}

func TestDiscoverEmptyRepository(t *testing.T) {
	cat := Discover(t.TempDir())
	if len(cat.Roles) != 0 || len(cat.Workflows) != 0 || len(cat.Diagnostics) != 0 {
		t.Fatalf("empty repo should yield an empty catalog: %+v", cat)
	}
}

func TestDiscoverValidRolesAndWorkflow(t *testing.T) {
	cat := fixture(t, validWorkflow)
	if len(cat.Roles) != 3 {
		t.Fatalf("roles: %+v", cat.Roles)
	}
	tester, ok := cat.FindRole("tester")
	if !ok || !tester.Valid {
		t.Fatalf("tester should be valid: %s", diagText(tester.Diagnostics))
	}
	r := tester.Role
	if r.Harness != "claude-code" || r.Model != "opus" || r.InstructionsSource != "inline" ||
		len(r.AllowedPaths) != 1 || len(r.Validation) != 2 {
		t.Fatalf("tester fields: %+v", r)
	}
	if r.Validation[0].TimeoutSeconds != 300 || !r.Validation[0].Required || r.Validation[1].Required {
		t.Fatalf("validation defaults: %+v", r.Validation)
	}
	wf, ok := cat.FindWorkflow("full")
	if !ok || !wf.Valid {
		t.Fatalf("workflow should be valid: %s", diagText(wf.Diagnostics))
	}
	w := wf.Workflow
	if w.LoopCap != 4 || strings.Join(w.Order, ",") != "builder,tester,documenter,review" {
		t.Fatalf("workflow: %+v", w)
	}
	if len(w.Returns) != 3 || w.Returns[0].From != "tester" || w.Returns[2].From != "review" {
		t.Fatalf("returns should be ordered by baton position: %+v", w.Returns)
	}
}

func TestWorkflowWithoutReviewAndDefaultLoopCap(t *testing.T) {
	cat := fixture(t, wfBody("roles: [builder, tester]\norder: [builder, tester]\n"))
	w := cat.Workflows[0]
	if !w.Valid {
		t.Fatalf("should be valid: %s", diagText(w.Diagnostics))
	}
	if w.Workflow.LoopCap != DefaultLoopCap || len(w.Workflow.Returns) != 0 {
		t.Fatalf("defaults: %+v", w.Workflow)
	}
}

func TestWorkflowValidationRules(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"unknown role profile", "roles: [builder, ghost]\norder: [builder, ghost]\n", `unknown role "ghost"`},
		{"review not last", "roles: [builder, tester]\norder: [builder, review, tester]\n", "may only be the last step"},
		{"role listed twice", "roles: [builder, tester]\norder: [builder, tester, builder]\n", "more than once"},
		{"declared role missing from order", "roles: [builder, tester]\norder: [builder]\n", `"tester" is declared but missing`},
		{"order role not declared", "roles: [builder]\norder: [builder, tester]\n", "not one of the declared roles"},
		{"review declared as a role", "roles: [builder, review]\norder: [builder]\n", "reserved for the review step"},
		{"duplicate declared role", "roles: [builder, builder]\norder: [builder]\n", "duplicate role"},
		{"forward return", "roles: [builder, tester]\norder: [builder, tester]\nreturns:\n  builder: [tester]\n", "must be an earlier role"},
		{"self return", "roles: [builder, tester]\norder: [builder, tester]\nreturns:\n  tester: [tester]\n", "must be an earlier role"},
		{"return to review", "roles: [builder, tester]\norder: [builder, tester, review]\nreturns:\n  tester: [review]\n", "cannot be returned to the review step"},
		{"return from unknown", "roles: [builder]\norder: [builder]\nreturns:\n  ghost: [builder]\n", "not in the forward order"},
		{"review return without review", "roles: [builder]\norder: [builder]\nreturns:\n  review: [builder]\n", "no review step"},
		{"empty return list", "roles: [builder, tester]\norder: [builder, tester]\nreturns:\n  tester: []\n", "at least one earlier role"},
		{"loop cap zero", "roles: [builder]\norder: [builder]\nloopCap: 0\n", "loopCap must be between"},
		{"loop cap unbounded", "roles: [builder]\norder: [builder]\nloopCap: 999\n", "loopCap must be between"},
		{"no roles", "order: [builder]\n", "needs at least one role"},
		{"no order", "roles: [builder]\n", "order is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mustInvalid(t, fixture(t, wfBody(tc.body)), tc.want)
		})
	}
}

func TestWorkflowRejectsParallelAndGraphFeatures(t *testing.T) {
	for _, key := range []string{"parallel: [tester, documenter]", "dependsOn: [builder]", "stages: []", "when: always", "matrix: {a: 1}"} {
		body := wfBody("roles: [builder]\norder: [builder]\n" + key + "\n")
		mustInvalid(t, fixture(t, body), "parallel or graph feature")
	}
}

func TestVersionRequiredAndChecked(t *testing.T) {
	mustInvalid(t, fixture(t, "id: wf\ndescription: d\nroles: [builder]\norder: [builder]\n"), "version is required")
	mustInvalid(t, fixture(t, "version: 2\nid: wf\ndescription: d\nroles: [builder]\norder: [builder]\n"), "unsupported version 2")
}

func TestRejectsUnknownFieldsAndAmbiguousYAML(t *testing.T) {
	cases := map[string]string{
		"unknown field":   wfBody("roles: [builder]\norder: [builder]\nbogus: 1\n"),
		"duplicate key":   wfBody("roles: [builder]\norder: [builder]\norder: [builder]\n"),
		"anchor":          wfBody("roles: &r [builder]\norder: *r\n"),
		"multi-document":  wfBody("roles: [builder]\norder: [builder]\n") + "---\nversion: 1\n",
		"merge key":       wfBody("roles: [builder]\norder: [builder]\nreturns:\n  <<: {a: [b]}\n"),
		"empty file":      "",
		"not utf8":        "version: 1\nid: \xff\xfe\n",
		"wrong type":      wfBody("roles: builder\norder: [builder]\n"),
		"unterminated":    "version: 1\nid: [wf\n",
		"scalar document": "just text\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			cat := fixture(t, body)
			w := cat.Workflows[0]
			if w.Valid || len(w.Diagnostics) == 0 {
				t.Fatalf("%s should be rejected with diagnostics", name)
			}
			for _, d := range w.Diagnostics {
				if d.File != WorkflowsDir+"/wf.yaml" || d.Message == "" {
					t.Fatalf("diagnostic needs file and message: %+v", d)
				}
			}
		})
	}
	// Unknown field message names the field and the same applies to roles.
	root := t.TempDir()
	writeFile(t, root, RolesDir+"/builder.yaml", role("builder")+"harnes: x\n")
	cat := Discover(root)
	if got := diagText(cat.Roles[0].Diagnostics); !strings.Contains(got, `unknown field "harnes"`) {
		t.Fatalf("got %q", got)
	}
}

func TestRoleValidation(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"no instructions", "version: 1\nid: builder\ndescription: d\n", "instructions"},
		{"both instructions", role("builder") + "instructionsFile: docs/x.md\n", "not both"},
		{"unknown harness", role("builder") + "harness: nope\n", "unknown harness"},
		{"absolute glob", role("builder") + "allowedPaths: [\"/etc/*\"]\n", "repo-relative"},
		{"traversal glob", role("builder") + "allowedPaths: [\"../x\"]\n", "traversal"},
		{"git glob", role("builder") + "allowedPaths: [\".git/**\"]\n", ".git"},
		{"duplicate glob", role("builder") + "allowedPaths: [\"a/**\", \"a/**\"]\n", "duplicate allowed path"},
		{"missing description", "version: 1\nid: builder\ninstructions: x\n", "description is required"},
		{"bad id", strings.Replace(role("builder"), "id: builder", "id: Bad_ID", 1), "must be 1-64 lowercase"},
		{"reserved id", role("review"), "reserved"},
		{"bad timeout", role("builder") + "validation:\n  - {id: u, command: x, timeoutSeconds: 99999}\n", "timeoutSeconds"},
		{"empty command", role("builder") + "validation:\n  - {id: u, command: \"\"}\n", "command is required"},
		{"duplicate command id", role("builder") + "validation:\n  - {id: u, command: x}\n  - {id: u, command: y}\n", "duplicate validation command"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, root, RolesDir+"/builder.yaml", tc.body)
			e := Discover(root).Roles[0]
			if e.Valid || e.Role != nil {
				t.Fatalf("should be invalid")
			}
			if got := diagText(e.Diagnostics); !strings.Contains(got, tc.want) {
				t.Fatalf("diagnostics %q should contain %q", got, tc.want)
			}
		})
	}
}

func TestInstructionsFile(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "docs/builder.md", "Be the builder.\n")
	writeFile(t, root, RolesDir+"/builder.yaml", "version: 1\nid: builder\ndescription: d\ninstructionsFile: docs/builder.md\n")
	e := Discover(root).Roles[0]
	if !e.Valid || e.Role.InstructionsSource != "docs/builder.md" || e.Role.InstructionsText != "Be the builder.\n" || e.Role.InstructionsSHA256 == "" {
		t.Fatalf("instructions file: %+v %s", e.Role, diagText(e.Diagnostics))
	}
}

func TestRejectsPathsOutsideRepoSymlinksAndOversizedFiles(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, outside, "secret.md", "secret")
	writeFile(t, root, "docs/real.md", "ok")
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(root, "docs", "link.md")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if err := os.Symlink("real.md", filepath.Join(root, "docs", "inrepo.md")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "docs/big.md", strings.Repeat("x", MaxInstructionBytes+1))
	for name, file := range map[string]string{
		"symlink out of repo": "docs/link.md",
		"symlink in repo":     "docs/inrepo.md",
		"absolute":            "/etc/passwd",
		"traversal":           "../x.md",
		"oversized":           "docs/big.md",
		"missing":             "docs/nope.md",
	} {
		t.Run(name, func(t *testing.T) {
			writeFile(t, root, RolesDir+"/r.yaml", "version: 1\nid: r\ndescription: d\ninstructionsFile: "+file+"\n")
			e := Discover(root).Roles[0]
			if e.Valid {
				t.Fatalf("%s should be rejected", name)
			}
			if got := diagText(e.Diagnostics); !strings.Contains(got, "instructionsFile") {
				t.Fatalf("diagnostic should name the field: %q", got)
			}
		})
	}

	// A symlinked definition file, a symlinked definitions directory, and an
	// oversized definition are all rejected with file diagnostics.
	defs := t.TempDir()
	writeFile(t, defs, "sneaky.yaml", role("sneaky"))
	root2 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root2, RolesDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(defs, "sneaky.yaml"), filepath.Join(root2, RolesDir, "sneaky.yaml")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root2, RolesDir+"/huge.yaml", role("huge")+"#"+strings.Repeat("x", MaxDefinitionBytes))
	cat := Discover(root2)
	if len(cat.Roles) != 2 {
		t.Fatalf("roles: %+v", cat.Roles)
	}
	for _, e := range cat.Roles {
		if e.Valid || len(e.Diagnostics) == 0 || e.Diagnostics[0].File == "" {
			t.Fatalf("%s should be rejected with a file diagnostic: %+v", e.File, e)
		}
	}
	root3 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root3, DefinitionDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(defs, filepath.Join(root3, RolesDir)); err != nil {
		t.Fatal(err)
	}
	cat = Discover(root3)
	if len(cat.Roles) != 0 || len(cat.Diagnostics) == 0 {
		t.Fatalf("symlinked roles dir must be rejected: %+v", cat)
	}
}

func TestDuplicateIDsAreFlagged(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, RolesDir+"/a.yaml", role("same"))
	writeFile(t, root, RolesDir+"/b.yaml", role("same"))
	for _, e := range Discover(root).Roles {
		if e.Valid || !strings.Contains(diagText(e.Diagnostics), "duplicate role id") {
			t.Fatalf("duplicates should be invalid: %+v", e)
		}
	}
}

func TestWorkflowReferencingInvalidRole(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, RolesDir+"/builder.yaml", "version: 1\nid: builder\n")
	writeFile(t, root, WorkflowsDir+"/wf.yaml", wfBody("roles: [builder]\norder: [builder]\n"))
	w := Discover(root).Workflows[0]
	if w.Valid || !strings.Contains(diagText(w.Diagnostics), `role "builder" is invalid`) {
		t.Fatalf("got %s", diagText(w.Diagnostics))
	}
}

func TestRejectsMergeKeysAndAnchoredKeysInOtherwiseValidRoles(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"merge key": {
			"<<: {version: 1, id: builder, description: Builder, instructions: Build}\n",
			"YAML merge keys are not supported",
		},
		"anchored key": {
			"version: 1\n&key id: builder\ndescription: Builder\ninstructions: Build\n",
			"YAML anchors and aliases are not supported",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, root, RolesDir+"/builder.yaml", tc.body)
			e := Discover(root).Roles[0]
			if e.Valid || !strings.Contains(diagText(e.Diagnostics), tc.want) {
				t.Fatalf("want %q, got valid=%v %s", tc.want, e.Valid, diagText(e.Diagnostics))
			}
		})
	}
}

func TestRejectsInRepositorySymlinkedDirectories(t *testing.T) {
	// Definitions directory is a symlink to another directory in the repo.
	root := t.TempDir()
	writeFile(t, root, "profiles/builder.yaml", role("builder"))
	if err := os.MkdirAll(filepath.Join(root, DefinitionDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "profiles"), filepath.Join(root, RolesDir)); err != nil {
		t.Skip("symlinks unavailable")
	}
	cat := Discover(root)
	if len(cat.Roles) != 0 || !strings.Contains(diagText(cat.Diagnostics), "symlink") {
		t.Fatalf("in-repo symlinked roles dir must be rejected: %+v", cat)
	}

	// Instruction file reached through an in-repository symlinked parent.
	root = t.TempDir()
	writeFile(t, root, "real/builder.md", "Be the builder.\n")
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "docs")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, RolesDir+"/builder.yaml", "version: 1\nid: builder\ndescription: d\ninstructionsFile: docs/builder.md\n")
	e := Discover(root).Roles[0]
	if e.Valid || !strings.Contains(diagText(e.Diagnostics), "symlink") {
		t.Fatalf("instruction path through symlinked parent must be rejected: %s", diagText(e.Diagnostics))
	}
}

func TestSymlinkedRegisteredRootIsStillSupported(t *testing.T) {
	target := t.TempDir()
	writeFile(t, target, RolesDir+"/builder.yaml", role("builder"))
	link := filepath.Join(t.TempDir(), "repo-link")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	cat := Discover(link)
	if len(cat.Roles) != 1 || !cat.Roles[0].Valid {
		t.Fatalf("a symlinked registered root should still work: %+v", cat)
	}
}
