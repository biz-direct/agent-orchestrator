package pipeline

import "testing"

func TestMatchPath(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"**/*_test.go", "a_test.go", true},
		{"**/*_test.go", "pkg/deep/b_test.go", true},
		{"**/*_test.go", "pkg/b.go", false},
		{"**/*_test.go", "pkg/b_test.go.orig", false},
		{"test/**", "test/a.go", true},
		{"test/**", "test/x/y/z.txt", true},
		{"test/**", "test", false},
		{"test/**", "tests/a.go", false},
		{"test/**", "src/test/a.go", false},
		{"src/*.go", "src/a.go", true},
		{"src/*.go", "src/sub/a.go", false},
		{"a/**/b.go", "a/b.go", true},
		{"a/**/b.go", "a/x/y/b.go", true},
		{"a/**/b.go", "a/x/y/c.go", false},
		{"**", "anything/at/all", true},
		{"**/**/x", "p/q/x", true},
		{"docs/[ab]*.md", "docs/a1.md", true},
		{"docs/[ab]*.md", "docs/c1.md", false},
		{"exact.txt", "exact.txt", true},
		{"exact.txt", "dir/exact.txt", false},
	}
	for _, c := range cases {
		if got := MatchPath(c.pattern, c.name); got != c.want {
			t.Errorf("MatchPath(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

func TestMatchesAnyEmptyListAllowsNothing(t *testing.T) {
	if MatchesAny(nil, "a_test.go") {
		t.Fatal("a profile with no allowed paths may change nothing")
	}
	if !MatchesAny([]string{"x/**", "**/*_test.go"}, "p/a_test.go") {
		t.Fatal("any matching pattern admits the path")
	}
}
