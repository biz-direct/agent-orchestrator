package domain

import "strings"

// A crew role session is a hidden attached Chat session (see Store
// CreateAttachedSession) that runs one repository-defined role inside a worker
// task's worktree. Its role is recorded durably in the session's attachment
// marker, "crew-role:<owner>:<role>". The marker is unique per owner and role
// while the role session lives, so a task cannot hold two live sessions for
// one role, and it is cleared when the session ends so the role can be re-added.

const crewRoleMarkerPrefix = "crew-role:"

// CrewRoleMarker returns the attachment marker for a role session of owner.
func CrewRoleMarker(owner SessionID, role string) string {
	return crewRoleMarkerPrefix + string(owner) + ":" + role
}

// CrewRoleFromMarker extracts the role id from an attachment marker. ok is
// false for an empty marker or one that belongs to something else (a pipeline
// stage attempt).
func CrewRoleFromMarker(marker string) (role string, ok bool) {
	rest, found := strings.CutPrefix(marker, crewRoleMarkerPrefix)
	if !found {
		return "", false
	}
	_, role, found = strings.Cut(rest, ":")
	if !found || role == "" {
		return "", false
	}
	return role, true
}
