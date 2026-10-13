package workgate

import (
	"context"
	"strings"
)

// CleanMode says how clean the worktree must be.
type CleanMode int

const (
	// CleanNone does not look at uncommitted paths.
	CleanNone CleanMode = iota
	// CleanTracked tolerates untracked build output but not tracked edits.
	CleanTracked
	// CleanAll requires no tracked edits and no untracked, non-ignored files.
	CleanAll
)

// Checkpoint is what a worktree must look like to count as a committed
// checkpoint: on the expected branch, with HEAD at the expected commit, clean,
// and (optionally) descended from an input commit.
type Checkpoint struct {
	// Branch is the expected branch; a detached HEAD never matches.
	Branch string
	// Head is the expected HEAD. An abbreviated id matches by prefix.
	Head string
	// Input, when set, must be HEAD or one of its ancestors.
	Input string
	Clean CleanMode
	// HeadBeforeClean reports a wrong HEAD ahead of uncommitted paths when both
	// hold. The default reports uncommitted paths first.
	HeadBeforeClean bool
}

// MismatchKind names why a worktree is not the expected checkpoint.
type MismatchKind string

const (
	// MismatchUnreadable means git could not be queried (see Mismatch.Err).
	MismatchUnreadable MismatchKind = "unreadable"
	// MismatchHistoryUnreadable means ancestry could not be verified (see Mismatch.Err).
	MismatchHistoryUnreadable MismatchKind = "history_unreadable"
	// MismatchBranch means the workspace is on another branch or a detached HEAD.
	MismatchBranch MismatchKind = "branch"
	// MismatchDirty means uncommitted paths exist where none are allowed.
	MismatchDirty MismatchKind = "dirty"
	// MismatchHead means HEAD is not the expected commit.
	MismatchHead MismatchKind = "head"
	// MismatchNonLinear means HEAD does not descend from the input commit.
	MismatchNonLinear MismatchKind = "non_linear"
)

// Mismatch is the first way a worktree differs from the expected checkpoint.
type Mismatch struct {
	Kind  MismatchKind
	State GitState
	// Err is set for the unreadable kinds.
	Err error
}

// CheckState compares an observed state with the expected checkpoint. It does
// not check ancestry, which needs git; use VerifyCheckpoint for that.
func CheckState(state GitState, want Checkpoint) *Mismatch {
	if state.Branch != want.Branch {
		return &Mismatch{Kind: MismatchBranch, State: state}
	}
	headWrong := !strings.HasPrefix(state.Head, want.Head)
	dirty := (want.Clean == CleanAll && state.DirtyTotal > 0) || (want.Clean == CleanTracked && state.TrackedTotal > 0)
	switch {
	case want.HeadBeforeClean && headWrong:
		return &Mismatch{Kind: MismatchHead, State: state}
	case dirty:
		return &Mismatch{Kind: MismatchDirty, State: state}
	case headWrong:
		return &Mismatch{Kind: MismatchHead, State: state}
	}
	return nil
}

// VerifyCheckpoint inspects workspace read-only and reports how it differs from
// the expected checkpoint, or nil when it matches. The observed state is always
// returned so callers can record the HEAD they judged. Nothing here resets,
// cleans, or amends the worktree.
func VerifyCheckpoint(ctx context.Context, g Git, workspace string, want Checkpoint) (GitState, *Mismatch) {
	state, err := g.Inspect(ctx, workspace)
	if err != nil {
		return GitState{}, &Mismatch{Kind: MismatchUnreadable, Err: err}
	}
	if m := CheckState(state, want); m != nil {
		return state, m
	}
	if want.Input != "" && state.Head != want.Input {
		descends, err := g.IsAncestor(ctx, workspace, want.Input, state.Head)
		if err != nil {
			return state, &Mismatch{Kind: MismatchHistoryUnreadable, State: state, Err: err}
		}
		if !descends {
			return state, &Mismatch{Kind: MismatchNonLinear, State: state}
		}
	}
	return state, nil
}
