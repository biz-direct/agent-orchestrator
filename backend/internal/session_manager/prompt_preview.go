package sessionmanager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// ErrUnknownPromptRole is returned for a role other than orchestrator or worker.
var ErrUnknownPromptRole = errors.New("unknown prompt role")

// ErrUnknownPromptProject is returned when the project to preview is not registered.
var ErrUnknownPromptProject = errors.New("unknown project")

// Prompt roles accepted by the preview methods.
const (
	PromptRoleOrchestrator = string(sessionPromptRoleOrchestrator)
	PromptRoleWorker       = string(sessionPromptRoleWorker)
)

// PromptSample are the stand-in project values the built-in view renders with.
type PromptSample struct {
	ProjectID     string `json:"projectId"`
	ProjectName   string `json:"projectName"`
	Repo          string `json:"repo"`
	DefaultBranch string `json:"defaultBranch"`
	Path          string `json:"path"`
	// OrchestratorSessionID is the stand-in used where a worker prompt names its orchestrator.
	OrchestratorSessionID string `json:"orchestratorSessionId"`
}

// BuiltinPrompts is AO's fixed prompt text for each role, rendered with sample
// project values and none of the user's rules.
type BuiltinPrompts struct {
	Sample PromptSample `json:"sample"`
	// Orchestrator is a project orchestrator's prompt.
	Orchestrator []PromptSection `json:"orchestrator"`
	// Worker is a project worker's prompt, with an active orchestrator.
	Worker []PromptSection `json:"worker"`
	// StandaloneWorker is the prompt of a worker not attached to any project.
	StandaloneWorker []PromptSection `json:"standaloneWorker"`
}

// BuiltinPrompts renders the built-in prompts from the same assembly path a
// spawn uses, so what the user reads cannot drift from what agents get.
func (m *Manager) BuiltinPrompts() BuiltinPrompts {
	sample := PromptSample{
		ProjectID:             "sample-project",
		ProjectName:           "Sample Project",
		Repo:                  "https://github.com/example/sample-project",
		DefaultBranch:         "main",
		Path:                  "/path/to/sample-project",
		OrchestratorSessionID: "sample-project-orchestrator-1",
	}
	project := promptProject{ID: sample.ProjectID, Name: sample.ProjectName, Repo: sample.Repo, DefaultBranch: sample.DefaultBranch, Path: sample.Path}
	return BuiltinPrompts{
		Sample:           sample,
		Orchestrator:     builtinOnly(buildSystemPromptSections(systemPromptConfig{Role: sessionPromptRoleOrchestrator, Project: project})),
		Worker:           builtinOnly(buildSystemPromptSections(systemPromptConfig{Role: sessionPromptRoleWorker, Project: project, OrchestratorSessionID: sample.OrchestratorSessionID})),
		StandaloneWorker: builtinOnly(buildSystemPromptSections(systemPromptConfig{Role: sessionPromptRoleWorker, Standalone: true})),
	}
}

func builtinOnly(sections []PromptSection) []PromptSection {
	out := make([]PromptSection, 0, len(sections))
	for _, section := range sections {
		if section.Layer == PromptLayerBuiltin {
			out = append(out, section)
		}
	}
	return out
}

func promptKindForRole(role string) (domain.SessionKind, error) {
	switch role {
	case PromptRoleOrchestrator:
		return domain.KindOrchestrator, nil
	case PromptRoleWorker:
		return domain.KindWorker, nil
	default:
		return "", ErrUnknownPromptRole
	}
}

// EffectivePrompt assembles the prompt a new session of role would get in the
// project right now: built-in, global rules, project rules, rules file, and the
// per-session runtime blocks. A broken rules file fails the call, exactly as it
// would fail a spawn. Session-specific values use the stand-in id.
func (m *Manager) EffectivePrompt(ctx context.Context, projectID domain.ProjectID, role string) ([]PromptSection, error) {
	kind, err := promptKindForRole(role)
	if err != nil {
		return nil, err
	}
	if _, ok, err := m.store.GetProject(ctx, string(projectID)); err != nil {
		return nil, err
	} else if !ok {
		return nil, ErrUnknownPromptProject
	}
	return m.buildSystemPromptSections(ctx, kind, projectID, "<session-id>", "", false)
}

// Prompt freshness values reported per running session.
const (
	// PromptFreshnessCurrent: the session's prompt equals what a new one would get.
	PromptFreshnessCurrent = "current"
	// PromptFreshnessStale: the session runs an older prompt; it picks up the
	// new rules on its next restore.
	PromptFreshnessStale = "stale"
	// PromptFreshnessUnknown: the session's prompt could not be compared.
	PromptFreshnessUnknown = "unknown"
)

// PromptSession is a running session and whether its prompt matches the rules
// a new session of the same kind would get.
type PromptSession struct {
	SessionID string `json:"sessionId"`
	ProjectID string `json:"projectId"`
	Role      string `json:"role" enum:"orchestrator,worker"`
	Freshness string `json:"freshness" enum:"current,stale,unknown"`
}

// PromptSessions lists the live sessions of role (in projectID, or in every
// project when it is empty) with whether each still uses an older prompt. It
// compares the prompt file written at spawn/restore with a fresh assembly.
func (m *Manager) PromptSessions(ctx context.Context, projectID domain.ProjectID, role string) ([]PromptSession, error) {
	kind, err := promptKindForRole(role)
	if err != nil {
		return nil, err
	}
	recs, err := m.store.ListAllSessions(ctx)
	if err != nil {
		return nil, err
	}
	out := []PromptSession{}
	for _, rec := range recs {
		if rec.Kind != kind || rec.IsTerminated || (projectID != "" && rec.ProjectID != projectID) {
			continue
		}
		out = append(out, PromptSession{
			SessionID: string(rec.ID),
			ProjectID: string(rec.ProjectID),
			Role:      role,
			Freshness: m.promptFreshness(ctx, rec),
		})
	}
	return out, nil
}

func (m *Manager) promptFreshness(ctx context.Context, rec domain.SessionRecord) string {
	dir := m.systemPromptDir(rec.ID)
	if dir == "" {
		return PromptFreshnessUnknown
	}
	applied, err := os.ReadFile(filepath.Join(dir, "system.md"))
	if err != nil {
		return PromptFreshnessUnknown
	}
	current, err := m.buildSystemPrompt(ctx, rec.Kind, rec.ProjectID, rec.ID, rec.RequestedOutput, domain.NormalizeSessionMode(rec.Mode) == domain.SessionModeChat)
	if err != nil {
		return PromptFreshnessUnknown
	}
	if strings.TrimSpace(string(applied)) == strings.TrimSpace(current) {
		return PromptFreshnessCurrent
	}
	return PromptFreshnessStale
}
