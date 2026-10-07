package pipelineruns

import (
	"fmt"
	"path"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/pipeline"
)

// Git file modes that need special handling in a stage diff.
const (
	modeSymlink   = "120000"
	modeSubmodule = "160000"

	maxReportedViolations = 50
)

// DiffEntry is one changed path between two commits, with renames and copies
// split into a delete plus an add so both ends are checked.
type DiffEntry struct {
	// Status is A, M, D, or T (type change).
	Status  string
	SrcMode string
	DstMode string
	DstSHA  string
	Path    string
}

// ScopeViolation is one path a specialist changed outside its allowed scope.
type ScopeViolation struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// validateScope checks a stage's entire diff against the snapshotted allowed
// paths. It is a hand-off constraint on committed changes, not a sandbox: it
// cannot stop a process from writing elsewhere, only refuse to advance a stage
// whose result touches paths its profile does not permit.
func validateScope(entries []DiffEntry, allowed []string, readBlob func(sha string) (string, error)) (violations []ScopeViolation, total int) {
	add := func(p, reason string) {
		total++
		if len(violations) < maxReportedViolations {
			violations = append(violations, ScopeViolation{Path: p, Reason: reason})
		}
	}
	for _, e := range entries {
		if e.SrcMode == modeSubmodule || e.DstMode == modeSubmodule {
			add(e.Path, "submodule changes are never allowed")
			continue
		}
		if !pipeline.MatchesAny(allowed, e.Path) {
			reason := "outside the profile's allowed paths"
			if len(allowed) == 0 {
				reason = "the profile allows no changes"
			}
			add(e.Path, verbFor(e.Status)+" "+reason)
			continue
		}
		if e.DstMode == modeSymlink && e.Status != "D" {
			target, err := readBlob(e.DstSHA)
			if err != nil {
				add(e.Path, fmt.Sprintf("the symlink target could not be read: %v", err))
				continue
			}
			target = strings.TrimSpace(target)
			switch {
			case target == "" || strings.HasPrefix(target, "/") || strings.Contains(target, "\\"):
				add(e.Path, fmt.Sprintf("symlink to %q is absolute or unreadable; links must stay inside the repository", target))
			default:
				resolved := path.Clean(path.Join(path.Dir(e.Path), target))
				switch {
				case resolved == ".." || strings.HasPrefix(resolved, "../"):
					add(e.Path, fmt.Sprintf("symlink to %q escapes the repository", target))
				case !pipeline.MatchesAny(allowed, resolved):
					add(e.Path, fmt.Sprintf("symlink to %q points outside the profile's allowed paths", target))
				}
			}
		}
	}
	return violations, total
}

func verbFor(status string) string {
	switch status {
	case "A":
		return "added path is"
	case "D":
		return "deleted path is"
	case "T":
		return "type-changed path is"
	default:
		return "modified path is"
	}
}
