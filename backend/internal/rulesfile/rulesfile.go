// Package rulesfile reads repo-relative standing-instruction files
// (agentRulesFile, orchestratorRulesFile) safely.
//
// The read resolves symlinks and refuses anything whose real path leaves the
// project's real root, so a link committed to a repo cannot pull in files from
// elsewhere on the host.
package rulesfile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Reason says why a rules file could not be used.
type Reason string

const (
	ReasonOutsideRepo Reason = "outside_repo"
	ReasonNotFound    Reason = "not_found"
	ReasonNotRegular  Reason = "not_regular_file"
	ReasonUnreadable  Reason = "unreadable"
	ReasonTooLarge    Reason = "too_large"
)

// Config field names carried by Error.Field.
const (
	FieldOrchestratorRulesFile = "orchestratorRulesFile"
	FieldAgentRulesFile        = "agentRulesFile"
)

// Error is a typed rules-file failure. Callers map it to an API error that
// names the file and the reason instead of a generic internal error.
type Error struct {
	Field  string
	Path   string
	Reason Reason
	Err    error
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("%s %q: %s", e.Field, e.Path, e.Reason)
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// Code is the stable API error code for this failure.
func (e *Error) Code() string {
	if e.Field == FieldOrchestratorRulesFile {
		return "ORCHESTRATOR_RULES_FILE_INVALID"
	}
	return "AGENT_RULES_FILE_INVALID"
}

// Details are the machine-readable fields clients show next to the message.
func (e *Error) Details() map[string]any {
	return map[string]any{"field": e.Field, "path": e.Path, "reason": string(e.Reason)}
}

// Read returns the contents of the repo-relative file rel under projectRoot.
// It resolves the real path, requires it to sit inside the real project root,
// requires a regular file, and reads at most limit+1 bytes. A limit <= 0 means
// no size cap. The size cap applies to the raw file bytes.
func Read(field, projectRoot, rel string, limit int) ([]byte, error) {
	fail := func(reason Reason, err error) ([]byte, error) {
		return nil, &Error{Field: field, Path: rel, Reason: reason, Err: err}
	}
	trimmed := strings.TrimSpace(rel)
	if strings.TrimSpace(projectRoot) == "" {
		return fail(ReasonUnreadable, errors.New("project path is required"))
	}
	if filepath.IsAbs(trimmed) || strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, `\`) {
		return fail(ReasonOutsideRepo, nil)
	}
	clean := filepath.Clean(trimmed)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fail(ReasonOutsideRepo, nil)
	}
	for _, seg := range strings.Split(filepath.ToSlash(clean), "/") {
		if seg == ".." {
			return fail(ReasonOutsideRepo, nil)
		}
	}

	realRoot, err := filepath.EvalSymlinks(projectRoot)
	if err != nil {
		return fail(ReasonUnreadable, fmt.Errorf("resolve project root: %w", err))
	}
	realPath, err := filepath.EvalSymlinks(filepath.Join(realRoot, clean))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fail(ReasonNotFound, nil)
		}
		return fail(ReasonUnreadable, err)
	}
	within, err := filepath.Rel(realRoot, realPath)
	if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) || filepath.IsAbs(within) {
		return fail(ReasonOutsideRepo, nil)
	}

	f, err := os.Open(realPath) //nolint:gosec // realPath is verified to sit inside the project's real root
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fail(ReasonNotFound, nil)
		}
		return fail(ReasonUnreadable, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return fail(ReasonUnreadable, err)
	}
	if !info.Mode().IsRegular() {
		return fail(ReasonNotRegular, nil)
	}

	var reader io.Reader = f
	if limit > 0 {
		reader = io.LimitReader(f, int64(limit)+1)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return fail(ReasonUnreadable, err)
	}
	if limit > 0 && len(data) > limit {
		return fail(ReasonTooLarge, nil)
	}
	return data, nil
}
