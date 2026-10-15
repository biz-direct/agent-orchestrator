package workgate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commit(t *testing.T, dir, msg string) string {
	t.Helper()
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-q", "-m", msg)
	return run(t, dir, "rev-parse", "HEAD")
}

func newRepo(t *testing.T) (dir, base string) {
	t.Helper()
	dir = t.TempDir()
	run(t, dir, "init", "-q", "-b", "task")
	write(t, dir, "a.txt", "a")
	return dir, commit(t, dir, "base")
}

func TestVerifyCheckpointAcceptsACleanDescendantOnTheExpectedBranch(t *testing.T) {
	dir, base := newRepo(t)
	write(t, dir, "b.txt", "b")
	head := commit(t, dir, "next")
	state, m := VerifyCheckpoint(context.Background(), ExecGit{}, dir, Checkpoint{Branch: "task", Head: head[:12], Input: base, Clean: CleanAll})
	if m != nil || state.Head != head {
		t.Fatalf("want ok at %s, got %+v %+v", head, state, m)
	}
	// An unchanged checkpoint (HEAD == input) needs no ancestry check.
	if _, m := VerifyCheckpoint(context.Background(), ExecGit{}, dir, Checkpoint{Branch: "task", Head: head, Input: head, Clean: CleanAll}); m != nil {
		t.Fatalf("unchanged checkpoint: %+v", m)
	}
}

func TestVerifyCheckpointRefusesDirtyStaleWrongBranchAndRewrittenHistory(t *testing.T) {
	dir, base := newRepo(t)
	write(t, dir, "b.txt", "b")
	head := commit(t, dir, "next")
	ctx := context.Background()

	write(t, dir, "untracked.txt", "x")
	state, m := VerifyCheckpoint(ctx, ExecGit{}, dir, Checkpoint{Branch: "task", Head: head, Clean: CleanAll})
	if m == nil || m.Kind != MismatchDirty || state.DirtyTotal != 1 || len(state.Dirty) != 1 {
		t.Fatalf("untracked file must be dirty under CleanAll: %+v %+v", state, m)
	}
	if _, m := VerifyCheckpoint(ctx, ExecGit{}, dir, Checkpoint{Branch: "task", Head: head, Clean: CleanTracked}); m != nil {
		t.Fatalf("untracked output is tolerated under CleanTracked: %+v", m)
	}
	write(t, dir, "a.txt", "edited")
	if _, m := VerifyCheckpoint(ctx, ExecGit{}, dir, Checkpoint{Branch: "task", Head: head, Clean: CleanTracked}); m == nil || m.Kind != MismatchDirty {
		t.Fatalf("tracked edits are never tolerated: %+v", m)
	}
	if _, m := VerifyCheckpoint(ctx, ExecGit{}, dir, Checkpoint{Branch: "task", Head: head, Clean: CleanNone}); m != nil {
		t.Fatalf("CleanNone ignores uncommitted paths: %+v", m)
	}
	run(t, dir, "checkout", "-q", "--", "a.txt")
	run(t, dir, "clean", "-fdq")

	// Stale: the worktree moved past the reported commit.
	if _, m := VerifyCheckpoint(ctx, ExecGit{}, dir, Checkpoint{Branch: "task", Head: base, Clean: CleanAll}); m == nil || m.Kind != MismatchHead {
		t.Fatalf("stale head: %+v", m)
	}
	// Wrong branch, and a detached HEAD never matches.
	run(t, dir, "checkout", "-q", "-b", "other")
	if _, m := VerifyCheckpoint(ctx, ExecGit{}, dir, Checkpoint{Branch: "task", Head: head, Clean: CleanAll}); m == nil || m.Kind != MismatchBranch {
		t.Fatalf("wrong branch: %+v", m)
	}
	run(t, dir, "checkout", "-q", "--detach")
	if _, m := VerifyCheckpoint(ctx, ExecGit{}, dir, Checkpoint{Branch: "task", Head: head, Clean: CleanAll}); m == nil || m.Kind != MismatchBranch {
		t.Fatalf("detached head: %+v", m)
	}
	run(t, dir, "checkout", "-q", "task")

	// Rewritten history: HEAD no longer descends from the input commit.
	run(t, dir, "checkout", "-q", "--orphan", "rewrite")
	write(t, dir, "c.txt", "c")
	rewritten := commit(t, dir, "rewrite")
	run(t, dir, "branch", "-q", "-M", "task2")
	if _, m := VerifyCheckpoint(ctx, ExecGit{}, dir, Checkpoint{Branch: "task2", Head: rewritten, Input: head, Clean: CleanAll}); m == nil || m.Kind != MismatchNonLinear {
		t.Fatalf("non-linear history: %+v", m)
	}
	// An unknown input commit cannot be verified either way.
	if _, m := VerifyCheckpoint(ctx, ExecGit{}, dir, Checkpoint{Branch: "task2", Head: rewritten, Input: strings.Repeat("0", 40), Clean: CleanAll}); m == nil || m.Kind != MismatchHistoryUnreadable || m.Err == nil {
		t.Fatalf("unverifiable history: %+v", m)
	}
}

func TestVerifyCheckpointReportsAnUninspectableWorkspace(t *testing.T) {
	_, m := VerifyCheckpoint(context.Background(), ExecGit{}, t.TempDir(), Checkpoint{Branch: "task"})
	if m == nil || m.Kind != MismatchUnreadable || m.Err == nil {
		t.Fatalf("not a repository: %+v", m)
	}
}

func TestCheckStateOrdersHeadAndDirtyReports(t *testing.T) {
	state := GitState{Branch: "task", Head: "bbbb", DirtyTotal: 1}
	want := Checkpoint{Branch: "task", Head: "aaaa", Clean: CleanAll}
	if m := CheckState(state, want); m == nil || m.Kind != MismatchDirty {
		t.Fatalf("uncommitted paths come first by default: %+v", m)
	}
	want.HeadBeforeClean = true
	if m := CheckState(state, want); m == nil || m.Kind != MismatchHead {
		t.Fatalf("a wrong HEAD comes first when asked: %+v", m)
	}
}

func TestDiffEntriesSplitsRenamesAndExposesSymlinksAndSubmodules(t *testing.T) {
	dir, base := newRepo(t)
	run(t, dir, "mv", "a.txt", "moved.txt")
	if err := os.Symlink("../outside", filepath.Join(dir, "link")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	head := commit(t, dir, "rename and link")
	entries, err := ExecGit{}.DiffEntries(context.Background(), dir, base, head)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]DiffEntry{}
	for _, e := range entries {
		got[e.Path] = e
	}
	if got["a.txt"].Status != "D" || got["moved.txt"].Status != "A" {
		t.Fatalf("a rename must be a delete plus an add: %+v", entries)
	}
	if l := got["link"]; l.DstMode != modeSymlink {
		t.Fatalf("symlink mode lost: %+v", l)
	}
	target, err := ExecGit{}.ReadBlob(context.Background(), dir, got["link"].DstSHA)
	if err != nil || strings.TrimSpace(target) != "../outside" {
		t.Fatalf("symlink target = %q, %v", target, err)
	}
}

func TestSplitStatusFoldsRenameSources(t *testing.T) {
	got := splitStatus("R  new.txt\x00old.txt\x00?? u.txt\x00 M m.txt\x00")
	want := []string{"R  new.txt <- old.txt", "?? u.txt", " M m.txt"}
	if len(got) != len(want) {
		t.Fatalf("got %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}
