package workgate

import (
	"errors"
	"strings"
	"testing"
)

func blobs(m map[string]string) func(string) (string, error) {
	return func(sha string) (string, error) {
		if v, ok := m[sha]; ok {
			return v, nil
		}
		return "", errors.New("missing blob")
	}
}

func TestValidateScope(t *testing.T) {
	allowed := []string{"**/*_test.go", "test/**"}
	cases := []struct {
		name    string
		entries []DiffEntry
		blobs   map[string]string
		allowed []string
		want    map[string]string // path -> reason substring
	}{
		{name: "inside scope", entries: []DiffEntry{{Status: "M", DstMode: "100644", Path: "pkg/a_test.go"}}},
		{name: "outside scope modify", entries: []DiffEntry{{Status: "M", DstMode: "100644", Path: "pkg/a.go"}}, want: map[string]string{"pkg/a.go": "modified path is outside"}},
		{name: "outside scope delete", entries: []DiffEntry{{Status: "D", SrcMode: "100644", Path: "pkg/a.go"}}, want: map[string]string{"pkg/a.go": "deleted path is"}},
		{
			// DiffEntries splits a rename, so moving a production file into the
			// allowed area still violates scope at the source end.
			name:    "rename out of scope into scope",
			entries: []DiffEntry{{Status: "D", SrcMode: "100644", Path: "pkg/a.go"}, {Status: "A", DstMode: "100644", Path: "test/a.go"}},
			want:    map[string]string{"pkg/a.go": "deleted path is"},
		},
		{name: "type change", entries: []DiffEntry{{Status: "T", DstMode: "120000", DstSHA: "s", Path: "pkg/x.go"}}, want: map[string]string{"pkg/x.go": "type-changed path is"}},
		{name: "submodule always refused", entries: []DiffEntry{{Status: "A", DstMode: modeSubmodule, Path: "test/sub"}}, want: map[string]string{"test/sub": "submodule"}},
		{name: "submodule removal refused", entries: []DiffEntry{{Status: "D", SrcMode: modeSubmodule, Path: "test/sub"}}, want: map[string]string{"test/sub": "submodule"}},
		{name: "symlink inside scope", entries: []DiffEntry{{Status: "A", DstMode: modeSymlink, DstSHA: "s1", Path: "test/link"}}, blobs: map[string]string{"s1": "other.txt\n"}},
		{name: "symlink absolute", entries: []DiffEntry{{Status: "A", DstMode: modeSymlink, DstSHA: "s1", Path: "test/link"}}, blobs: map[string]string{"s1": "/etc/passwd"}, want: map[string]string{"test/link": "absolute"}},
		{name: "symlink escapes repository", entries: []DiffEntry{{Status: "A", DstMode: modeSymlink, DstSHA: "s1", Path: "test/link"}}, blobs: map[string]string{"s1": "../../x"}, want: map[string]string{"test/link": "escapes the repository"}},
		{name: "symlink into protected code", entries: []DiffEntry{{Status: "A", DstMode: modeSymlink, DstSHA: "s1", Path: "test/link"}}, blobs: map[string]string{"s1": "../pkg/a.go"}, want: map[string]string{"test/link": "outside the profile's allowed paths"}},
		{name: "symlink backslash", entries: []DiffEntry{{Status: "A", DstMode: modeSymlink, DstSHA: "s1", Path: "test/link"}}, blobs: map[string]string{"s1": `..\x`}, want: map[string]string{"test/link": "absolute or unreadable"}},
		{name: "symlink target unreadable", entries: []DiffEntry{{Status: "A", DstMode: modeSymlink, DstSHA: "gone", Path: "test/link"}}, want: map[string]string{"test/link": "could not be read"}},
		{name: "symlink deletion is not resolved", entries: []DiffEntry{{Status: "D", SrcMode: modeSymlink, Path: "test/link"}}},
		{name: "no allowed paths allows nothing", entries: []DiffEntry{{Status: "A", DstMode: "100644", Path: "a_test.go"}}, allowed: []string{}, want: map[string]string{"a_test.go": "allows no changes"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := tc.allowed
			if a == nil {
				a = allowed
			}
			violations, total := ValidateScope(tc.entries, a, blobs(tc.blobs))
			if total != len(tc.want) || len(violations) != len(tc.want) {
				t.Fatalf("got %d (%+v), want %v", total, violations, tc.want)
			}
			for _, v := range violations {
				if sub, ok := tc.want[v.Path]; !ok || !strings.Contains(v.Reason, sub) {
					t.Fatalf("violation %+v does not match %v", v, tc.want)
				}
			}
		})
	}
}

func TestValidateScopeBoundsReportedViolationsButCountsAll(t *testing.T) {
	var entries []DiffEntry
	for i := 0; i < maxReportedViolations+10; i++ {
		entries = append(entries, DiffEntry{Status: "M", DstMode: "100644", Path: "p/" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + ".go"})
	}
	violations, total := ValidateScope(entries, []string{"nothing/**"}, blobs(nil))
	if total != len(entries) || len(violations) != maxReportedViolations {
		t.Fatalf("total=%d reported=%d", total, len(violations))
	}
}
