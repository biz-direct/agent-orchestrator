package rulesfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func reasonOf(t *testing.T, err error) Reason {
	t.Helper()
	var re *Error
	if !errors.As(err, &re) {
		t.Fatalf("err = %v, want *Error", err)
	}
	return re.Reason
}

func TestRead(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	write := func(p, content string) {
		t.Helper()
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("docs/rules.md", "hello")
	write("big.md", strings.Repeat("a", 11))
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustLink := func(target, link string) {
		t.Helper()
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	mustLink(filepath.Join(outside, "secret"), "escape.md")
	mustLink(outside, "escapedir")
	mustLink("docs/rules.md", "inside.md")
	mustLink(filepath.Join(root, "missing-target"), "dangling.md")

	got, err := Read(FieldOrchestratorRulesFile, root, "docs/rules.md", 10)
	if err != nil || string(got) != "hello" {
		t.Fatalf("Read = %q, %v", got, err)
	}
	if got, err := Read(FieldOrchestratorRulesFile, root, "inside.md", 10); err != nil || string(got) != "hello" {
		t.Fatalf("in-repo symlink = %q, %v", got, err)
	}

	cases := []struct {
		name, rel string
		limit     int
		want      Reason
	}{
		{"absolute", "/etc/passwd", 10, ReasonOutsideRepo},
		{"dotdot", "../x", 10, ReasonOutsideRepo},
		{"nested dotdot", "docs/../../x", 10, ReasonOutsideRepo},
		{"symlink file out of repo", "escape.md", 10, ReasonOutsideRepo},
		{"symlink dir out of repo", "escapedir/secret", 10, ReasonOutsideRepo},
		{"missing", "nope.md", 10, ReasonNotFound},
		{"dangling symlink", "dangling.md", 10, ReasonNotFound},
		{"directory", "docs", 10, ReasonNotRegular},
		{"too large", "big.md", 10, ReasonTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Read(FieldOrchestratorRulesFile, root, tc.rel, tc.limit)
			if got := reasonOf(t, err); got != tc.want {
				t.Fatalf("reason = %q, want %q", got, tc.want)
			}
		})
	}

	if _, err := Read(FieldAgentRulesFile, root, "big.md", 0); err != nil {
		t.Fatalf("limit 0 must not cap: %v", err)
	}
	if _, err := Read(FieldOrchestratorRulesFile, root, "big.md", 11); err != nil {
		t.Fatalf("exactly the limit must be allowed: %v", err)
	}
}
