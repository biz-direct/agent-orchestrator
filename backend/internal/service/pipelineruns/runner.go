package pipelineruns

import "github.com/aoagents/agent-orchestrator/backend/internal/workgate"

// The validation command runner is pipeline-independent and lives in workgate;
// these aliases keep the pipeline's own API unchanged.
type (
	// CommandSpec is one independently executed validation command.
	CommandSpec = workgate.CommandSpec
	// CommandOutcome is what happened, before classification.
	CommandOutcome = workgate.CommandOutcome
	// CommandRunner executes validation commands.
	CommandRunner = workgate.CommandRunner
	// ExecRunner runs commands in their own process group with a timeout.
	ExecRunner = workgate.ExecRunner
)

// commandEnv is the environment validation commands receive: an allowlisted
// subset of the daemon's own plus AO_PIPELINE_VALIDATION.
func commandEnv() []string {
	return workgate.CommandEnv("AO_PIPELINE_VALIDATION=1")
}
