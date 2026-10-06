package pipelineruns

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

// maxReportedDirtyPaths bounds how many dirty paths a rejection explains.
const maxReportedDirtyPaths = 50

// GitState is what the daemon independently observes in a worker's workspace.
// Stage results are checked against it; an agent's claim is never trusted.
type GitState struct {
	Head   string
	Branch string // empty when HEAD is detached
	// Dirty lists tracked changes and untracked, non-ignored files (bounded).
	Dirty      []string
	DirtyTotal int
}

// Git inspects a workspace. Implementations must be read-only: pipeline code
// never resets, cleans, or amends a worker's worktree.
type Git interface {
	Inspect(ctx context.Context, workspace string) (GitState, error)
	// IsAncestor reports whether ancestor is reachable from descendant.
	IsAncestor(ctx context.Context, workspace, ancestor, descendant string) (bool, error)
	// DiffEntries lists every path changed between two commits, with renames
	// split into delete + add so both ends are scope-checked.
	DiffEntries(ctx context.Context, workspace, base, head string) ([]DiffEntry, error)
	// ReadBlob returns the content of a blob (used for symlink targets).
	ReadBlob(ctx context.Context, workspace, sha string) (string, error)
}

// ExecGit shells out to the git binary.
type ExecGit struct{}

// Inspect implements Git.
func (ExecGit) Inspect(ctx context.Context, workspace string) (GitState, error) {
	head, err := gitOutput(ctx, workspace, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return GitState{}, fmt.Errorf("resolve HEAD: %w", err)
	}
	state := GitState{Head: strings.TrimSpace(head)}
	if branch, err := gitOutput(ctx, workspace, "symbolic-ref", "--short", "-q", "HEAD"); err == nil {
		state.Branch = strings.TrimSpace(branch)
	}
	status, err := gitOutput(ctx, workspace, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return GitState{}, fmt.Errorf("git status: %w", err)
	}
	for _, entry := range splitStatus(status) {
		state.DirtyTotal++
		if len(state.Dirty) < maxReportedDirtyPaths {
			state.Dirty = append(state.Dirty, entry)
		}
	}
	return state, nil
}

// IsAncestor implements Git.
func (ExecGit) IsAncestor(ctx context.Context, workspace, ancestor, descendant string) (bool, error) {
	cmd := aoprocess.CommandContext(ctx, "git", "-C", workspace, "merge-base", "--is-ancestor", ancestor, descendant)
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	if exit := exitCode(err); exit == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git merge-base: %w", err)
}

func gitOutput(ctx context.Context, workspace string, args ...string) (string, error) {
	full := append([]string{"-C", workspace}, args...)
	var stderr bytes.Buffer
	cmd := aoprocess.CommandContext(ctx, "git", full...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// splitStatus turns NUL-separated porcelain v1 output into "XY path" entries,
// folding rename/copy source paths into the entry that owns them.
func splitStatus(raw string) []string {
	parts := strings.Split(raw, "\x00")
	var out []string
	for i := 0; i < len(parts); i++ {
		p := parts[i]
		if len(p) < 4 {
			continue
		}
		entry := p
		if p[0] == 'R' || p[0] == 'C' || p[1] == 'R' || p[1] == 'C' {
			if i+1 < len(parts) {
				entry = p + " <- " + parts[i+1]
				i++
			}
		}
		out = append(out, entry)
	}
	return out
}

type exitCoder interface{ ExitCode() int }

func exitCode(err error) int {
	var ec exitCoder
	if errors.As(err, &ec) {
		return ec.ExitCode()
	}
	return -1
}

// DiffEntries implements Git.
func (ExecGit) DiffEntries(ctx context.Context, workspace, base, head string) ([]DiffEntry, error) {
	out, err := gitOutput(ctx, workspace, "diff", "--raw", "-z", "-r", "--no-renames", "--no-ext-diff", "--no-textconv", base, head)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(out, "\x00")
	var entries []DiffEntry
	for i := 0; i+1 < len(parts); i += 2 {
		meta := strings.Fields(strings.TrimPrefix(parts[i], ":"))
		if len(meta) < 5 {
			return nil, fmt.Errorf("unexpected git diff output %q", parts[i])
		}
		entries = append(entries, DiffEntry{SrcMode: meta[0], DstMode: meta[1], DstSHA: meta[3], Status: meta[4][:1], Path: parts[i+1]})
	}
	return entries, nil
}

// ReadBlob implements Git.
func (ExecGit) ReadBlob(ctx context.Context, workspace, sha string) (string, error) {
	return gitOutput(ctx, workspace, "cat-file", "blob", sha)
}
