package pipeline

import (
	"path"
	"strings"
)

// MatchPath reports whether a repo-relative, slash-separated path matches a
// validated allowedPaths pattern. Segments use path.Match syntax; a segment that
// is exactly "**" matches any number of directories (including none). A pattern
// that ends in "**" matches everything beneath its prefix but not the prefix
// directory itself, because a path names files, not directories.
func MatchPath(pattern, name string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			// Collapse repeated ** and try every split point.
			for len(pat) > 1 && pat[1] == "**" {
				pat = pat[1:]
			}
			if len(pat) == 1 {
				return len(name) > 0
			}
			for i := 0; i <= len(name); i++ {
				if matchSegments(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		ok, err := path.Match(pat[0], name[0])
		if err != nil || !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

// MatchesAny reports whether name matches at least one pattern. An empty list
// matches nothing: a profile that lists no allowedPaths may change nothing.
func MatchesAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if MatchPath(p, name) {
			return true
		}
	}
	return false
}
