package pipelineruns

import "github.com/aoagents/agent-orchestrator/backend/internal/workgate"

// The git inspection used to verify checkpoints is pipeline-independent and
// lives in workgate; these aliases keep the pipeline's own API unchanged.
type (
	// GitState is what the daemon independently observes in a worker's workspace.
	GitState = workgate.GitState
	// Git inspects a workspace read-only.
	Git = workgate.Git
	// ExecGit shells out to the git binary.
	ExecGit = workgate.ExecGit
	// DiffEntry is one changed path between two commits.
	DiffEntry = workgate.DiffEntry
)
