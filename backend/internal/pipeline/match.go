package pipeline

import "github.com/aoagents/agent-orchestrator/backend/internal/workgate"

// MatchPath reports whether a repo-relative, slash-separated path matches a
// validated allowedPaths pattern. The matcher is shared with the scope check
// and lives in workgate.
func MatchPath(pattern, name string) bool { return workgate.MatchPath(pattern, name) }

// MatchesAny reports whether name matches at least one pattern. An empty list
// matches nothing: a profile that lists no allowedPaths may change nothing.
func MatchesAny(patterns []string, name string) bool { return workgate.MatchesAny(patterns, name) }
