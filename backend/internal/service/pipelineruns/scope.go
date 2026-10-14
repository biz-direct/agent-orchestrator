package pipelineruns

import "github.com/aoagents/agent-orchestrator/backend/internal/workgate"

// ScopeViolation is one path a specialist changed outside its allowed scope.
type ScopeViolation = workgate.ScopeViolation

// validateScope checks a stage's entire diff against the snapshotted allowed
// paths using the shared scope check.
var validateScope = workgate.ValidateScope
