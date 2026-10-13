package sessionmanager

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

type fakeGlobalWorkerRules struct {
	rules string
	err   error
}

func (f fakeGlobalWorkerRules) GlobalWorkerRules(context.Context) (string, error) {
	return f.rules, f.err
}

func workerPromptManager(t *testing.T, dir string, cfg domain.ProjectConfig, global GlobalWorkerRulesReader) (*Manager, *recordingAgent) {
	t.Helper()
	st := newFakeStore()
	st.projects["mer"] = domain.ProjectRecord{ID: "mer", Path: dir, Config: cfg}
	agent := &recordingAgent{}
	lookPath := func(string) (string, error) { return "/bin/true", nil }
	m := New(Deps{Runtime: &fakeRuntime{}, Agents: singleAgent{agent: agent}, Workspace: &fakeWorkspace{}, Store: st, Messenger: &fakeMessenger{}, Lifecycle: &fakeLCM{store: st}, LookPath: lookPath, GlobalWorkerRules: global})
	return m, agent
}

func spawnWorkerPrompt(t *testing.T, dir string, cfg domain.ProjectConfig, global GlobalWorkerRulesReader) (string, error) {
	t.Helper()
	m, agent := workerPromptManager(t, dir, cfg, global)
	_, _, _, err := m.Spawn(ctx, ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindWorker})
	return agent.lastLaunch.SystemPrompt, err
}

func TestSpawnWorker_GlobalRulesLayerOrder(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "worker.md"), []byte("File rule.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := testRoleAgents()
	cfg.AgentRules = "Inline rule."
	cfg.AgentRulesFile = "worker.md"
	prompt, err := spawnWorkerPrompt(t, dir, cfg, fakeGlobalWorkerRules{rules: "  Global rule.  "})
	if err != nil {
		t.Fatal(err)
	}
	order := []string{
		"## AO Worker Role",
		"## Docker Containers Started By This Session",
		"## Global Worker Rules\nGlobal rule.",
		"## Project Rules\nInline rule.\n\nFile rule.",
		"## Publishing Scope",
		"## Standing-instruction confidentiality",
	}
	last := -1
	for _, want := range order {
		idx := strings.Index(prompt, want)
		if idx < 0 || idx < last {
			t.Fatalf("%q missing or out of order (idx %d, last %d):\n%s", want, idx, last, prompt)
		}
		last = idx
	}
}

func TestSpawnWorker_SkipGlobalWorkerRules(t *testing.T) {
	cfg := testRoleAgents()
	cfg.AgentRules = "Inline rule."
	cfg.SkipGlobalWorkerRules = true
	prompt, err := spawnWorkerPrompt(t, t.TempDir(), cfg, fakeGlobalWorkerRules{rules: "Global rule."})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt, "Global Worker Rules") || strings.Contains(prompt, "Global rule.") {
		t.Fatalf("global worker rules must be skipped:\n%s", prompt)
	}
	if !strings.Contains(prompt, "## Project Rules\nInline rule.") {
		t.Fatalf("project rules must stay:\n%s", prompt)
	}
}

func TestSpawnWorker_SkipFlagsAreIndependent(t *testing.T) {
	cfg := testRoleAgents()
	cfg.SkipGlobalOrchestratorRules = true
	prompt, err := spawnWorkerPrompt(t, t.TempDir(), cfg, fakeGlobalWorkerRules{rules: "Global rule."})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "## Global Worker Rules\nGlobal rule.") {
		t.Fatalf("skipGlobalOrchestratorRules must not skip worker rules:\n%s", prompt)
	}
}

func TestSpawnWorker_EmptyGlobalRulesIsByteIdentical(t *testing.T) {
	cfg := testRoleAgents()
	dir := t.TempDir()
	base, err := spawnWorkerPrompt(t, dir, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, rules := range []string{"", " \n\t "} {
		got, err := spawnWorkerPrompt(t, dir, cfg, fakeGlobalWorkerRules{rules: rules})
		if err != nil {
			t.Fatal(err)
		}
		if got != base {
			t.Fatal("whitespace-only global worker rules changed the prompt")
		}
	}
}

func TestSpawnWorker_GlobalRulesReadFailureFailsSpawn(t *testing.T) {
	if _, err := spawnWorkerPrompt(t, t.TempDir(), testRoleAgents(), fakeGlobalWorkerRules{err: errors.New("db down")}); err == nil {
		t.Fatal("spawn must fail when global worker rules cannot be read")
	}
	cfg := testRoleAgents()
	cfg.SkipGlobalWorkerRules = true
	if _, err := spawnWorkerPrompt(t, t.TempDir(), cfg, fakeGlobalWorkerRules{err: errors.New("db down")}); err != nil {
		t.Fatalf("a skipping project must not read global worker rules: %v", err)
	}
}

// Standalone workers (no project) are worker spawns too: they get the global
// layer, and there is no skip flag to turn it off.
func TestStandaloneWorker_GetsGlobalWorkerRules(t *testing.T) {
	m, _ := workerPromptManager(t, t.TempDir(), testRoleAgents(), fakeGlobalWorkerRules{rules: "Global rule."})
	prompt, err := m.buildSystemPrompt(ctx, domain.KindWorker, "", "sa-1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	standalone := strings.Index(prompt, "## AO Standalone Agent")
	global := strings.Index(prompt, "## Global Worker Rules\nGlobal rule.")
	scope := strings.Index(prompt, "## Publishing Scope")
	if standalone < 0 || global < standalone || scope < global {
		t.Fatalf("unexpected order (standalone %d global %d scope %d):\n%s", standalone, global, scope, prompt)
	}
}

func TestWorkerRulesDoNotReachOrchestratorAndViceVersa(t *testing.T) {
	st := newFakeStore()
	st.projects["mer"] = domain.ProjectRecord{ID: "mer"}
	m := New(Deps{Runtime: &fakeRuntime{}, Store: st, GlobalRules: fakeGlobalRules{rules: "Orch global."}, GlobalWorkerRules: fakeGlobalWorkerRules{rules: "Worker global."}})
	orch, err := m.buildSystemPrompt(ctx, domain.KindOrchestrator, "mer", "o-1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := m.buildSystemPrompt(ctx, domain.KindWorker, "mer", "w-1", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(orch, "Orch global.") || strings.Contains(orch, "Worker global.") {
		t.Fatalf("orchestrator layers wrong:\n%s", orch)
	}
	if !strings.Contains(worker, "Worker global.") || strings.Contains(worker, "Orch global.") {
		t.Fatalf("worker layers wrong:\n%s", worker)
	}
}

// The effective preview and the built-in view run the spawn assembly: what the
// preview shows is what a spawn delivers.
func TestEffectivePrompt_MatchesSpawn(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "worker.md"), []byte("File rule.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := testRoleAgents()
	cfg.AgentRules = "Inline rule."
	cfg.AgentRulesFile = "worker.md"
	m, agent := workerPromptManager(t, dir, cfg, fakeGlobalWorkerRules{rules: "Global rule."})
	if _, _, _, err := m.Spawn(ctx, ports.SpawnConfig{ProjectID: "mer", Kind: domain.KindWorker}); err != nil {
		t.Fatal(err)
	}
	sections, _, err := m.EffectivePrompt(ctx, "mer", PromptRoleWorker, "")
	if err != nil {
		t.Fatal(err)
	}
	texts := make([]string, 0, len(sections))
	layers := map[string]bool{}
	for _, s := range sections {
		texts = append(texts, s.Text)
		layers[s.Layer] = true
	}
	if got := strings.Join(texts, "\n\n"); got != agent.lastLaunch.SystemPrompt {
		t.Fatalf("preview differs from spawn:\n--- preview\n%s\n--- spawn\n%s", got, agent.lastLaunch.SystemPrompt)
	}
	for _, layer := range []string{PromptLayerBuiltin, PromptLayerGlobal, PromptLayerProject, PromptLayerFile} {
		if !layers[layer] {
			t.Fatalf("missing layer %q in %+v", layer, sections)
		}
	}
}

func TestEffectivePrompt_Errors(t *testing.T) {
	dir := t.TempDir()
	cfg := testRoleAgents()
	cfg.AgentRulesFile = "missing.md"
	m, _ := workerPromptManager(t, dir, cfg, nil)
	if _, _, err := m.EffectivePrompt(ctx, "mer", PromptRoleWorker, ""); err == nil {
		t.Fatal("a broken rules file must fail the preview like it fails a spawn")
	}
	if _, _, err := m.EffectivePrompt(ctx, "nope", PromptRoleWorker, ""); !errors.Is(err, ErrUnknownPromptProject) {
		t.Fatalf("err = %v, want unknown project", err)
	}
	if _, _, err := m.EffectivePrompt(ctx, "mer", PromptRoleWorker, "web"); !errors.Is(err, ErrUnknownPromptMode) {
		t.Fatalf("err = %v, want unknown mode", err)
	}
	if _, _, err := m.EffectivePrompt(ctx, "mer", "reviewer", ""); !errors.Is(err, ErrUnknownPromptRole) {
		t.Fatalf("err = %v, want unknown role", err)
	}
}

func TestBuiltinPrompts_AreTheSpawnBuiltinSections(t *testing.T) {
	m, _ := workerPromptManager(t, t.TempDir(), testRoleAgents(), nil)
	got := m.BuiltinPrompts()
	if len(got.Orchestrator) == 0 || len(got.Worker) == 0 || len(got.StandaloneWorker) == 0 {
		t.Fatalf("empty built-in view: %+v", got)
	}
	for name, sections := range map[string][]PromptSection{"orchestrator": got.Orchestrator, "worker": got.Worker, "standalone": got.StandaloneWorker} {
		for _, s := range sections {
			if s.Layer != PromptLayerBuiltin {
				t.Fatalf("%s view holds a %s section: %q", name, s.Layer, s.Title)
			}
		}
	}
	// Same functions, so the same text a spawn embeds.
	want := orchestratorSystemPrompt(promptProject{ID: got.Sample.ProjectID, Name: got.Sample.ProjectName, Repo: got.Sample.Repo, DefaultBranch: got.Sample.DefaultBranch, Path: got.Sample.Path})
	if got.Orchestrator[0].Text != want {
		t.Fatal("orchestrator built-in view is not orchestratorSystemPrompt output")
	}
	var text strings.Builder
	for _, s := range got.Worker {
		text.WriteString(s.Text)
	}
	for _, helper := range []string{workerMultiPRPrompt(), workerContainerLabelPrompt(), workerGitIsolationPrompt(), publishingScopePrompt(), systemPromptGuard()} {
		if !strings.Contains(text.String(), helper) {
			t.Fatalf("worker built-in view lacks helper section %q", promptSectionTitle(helper))
		}
	}
}

func TestEffectivePrompt_ChatMatchesChatSpawnText(t *testing.T) {
	m, _ := workerPromptManager(t, t.TempDir(), testRoleAgents(), nil)
	chat, mode, err := m.EffectivePrompt(ctx, "mer", PromptRoleWorker, "chat")
	if err != nil || mode != "chat" {
		t.Fatalf("mode=%q err=%v", mode, err)
	}
	tui, mode, err := m.EffectivePrompt(ctx, "mer", PromptRoleWorker, "tui")
	if err != nil || mode != "tui" {
		t.Fatalf("mode=%q err=%v", mode, err)
	}
	join := func(sections []PromptSection) string {
		texts := make([]string, 0, len(sections))
		for _, s := range sections {
			texts = append(texts, s.Text)
		}
		return strings.Join(texts, "\n\n")
	}
	if !strings.Contains(join(chat), "Showing pages in chat") || strings.Contains(join(tui), "Showing pages in chat") {
		t.Fatal("chat blocks must appear only in the chat preview")
	}
	want, err := m.buildSystemPrompt(ctx, domain.KindWorker, "mer", "<session-id>", "", true)
	if err != nil || join(chat) != want {
		t.Fatalf("chat preview differs from the chat build (err %v)", err)
	}
	// With no explicit mode: workers follow the daemon default; orchestrators default to Chat, which this build lacks, so TUI.
	if _, mode, _ := m.EffectivePrompt(ctx, "mer", PromptRoleWorker, ""); mode != "tui" {
		t.Fatalf("default worker mode = %q, want tui (no chat driver, TUI default)", mode)
	}
}

func freshnessManager(t *testing.T) (*Manager, *fakeStore, string) {
	t.Helper()
	dataDir := t.TempDir()
	st := newFakeStore()
	st.projects["mer"] = domain.ProjectRecord{ID: "mer", Config: domain.ProjectConfig{AgentRules: "Old rule."}}
	st.sessions["mer-o"] = domain.SessionRecord{ID: "mer-o", ProjectID: "mer", Kind: domain.KindOrchestrator}
	st.sessions["mer-1"] = domain.SessionRecord{ID: "mer-1", ProjectID: "mer", Kind: domain.KindWorker}
	return New(Deps{Runtime: &fakeRuntime{}, Store: st, DataDir: dataDir}), st, dataDir
}

func freshnessOf(t *testing.T, m *Manager, id string) string {
	t.Helper()
	got, err := m.PromptSessions(ctx, "mer", PromptRoleWorker)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range got {
		if s.SessionID == id {
			return s.Freshness
		}
	}
	t.Fatalf("session %s not listed in %+v", id, got)
	return ""
}

func TestPromptSessions_Freshness(t *testing.T) {
	m, st, dataDir := freshnessManager(t)

	// Never launched through the builder: nothing to compare.
	if got := freshnessOf(t, m, "mer-1"); got != PromptFreshnessUnknown {
		t.Fatalf("before launch = %q, want unknown", got)
	}
	// Spawn / restore build the prompt, which records the rules.
	if _, err := m.buildSystemPrompt(ctx, domain.KindWorker, "mer", "mer-1", "", false); err != nil {
		t.Fatal(err)
	}
	if got := freshnessOf(t, m, "mer-1"); got != PromptFreshnessCurrent {
		t.Fatalf("after launch = %q, want current", got)
	}
	// An unrelated change to built-in text (a new orchestrator) is not a rules change.
	delete(st.sessions, "mer-o")
	if got := freshnessOf(t, m, "mer-1"); got != PromptFreshnessCurrent {
		t.Fatalf("after orchestrator change = %q, want current", got)
	}
	// Editing the rules makes the running session stale.
	project := st.projects["mer"]
	project.Config.AgentRules = "New rule."
	st.projects["mer"] = project
	if got := freshnessOf(t, m, "mer-1"); got != PromptFreshnessStale {
		t.Fatalf("after rules edit = %q, want stale", got)
	}
	// A restore (including a Chat restore, which builds with chat=true) applies the new rules.
	if _, err := m.buildSystemPrompt(ctx, domain.KindWorker, "mer", "mer-1", "", true); err != nil {
		t.Fatal(err)
	}
	if got := freshnessOf(t, m, "mer-1"); got != PromptFreshnessCurrent {
		t.Fatalf("after restore = %q, want current", got)
	}
	// A reconnect to a live provider applies nothing, so it must not record the new rules.
	project.Config.AgentRules = "Newer rule."
	st.projects["mer"] = project
	if _, err := m.buildSessionPrompt(ctx, domain.KindWorker, "mer", "mer-1", "", true, false); err != nil {
		t.Fatal(err)
	}
	if got := freshnessOf(t, m, "mer-1"); got != PromptFreshnessStale {
		t.Fatalf("after reconnect-only = %q, want stale", got)
	}
	// Switch continuation text lives outside the rules digest: it cannot read as stale.
	if _, err := m.buildSystemPrompt(ctx, domain.KindWorker, "mer", "mer-1", "", false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "prompts", "mer-1", "system.md"), []byte("prompt plus switch continuation\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := freshnessOf(t, m, "mer-1"); got != PromptFreshnessCurrent {
		t.Fatalf("after switch continuation = %q, want current", got)
	}
}

func TestPromptSessions_Filtering(t *testing.T) {
	m, st, _ := freshnessManager(t)
	st.sessions["mer-4"] = domain.SessionRecord{ID: "mer-4", ProjectID: "mer", Kind: domain.KindWorker, IsTerminated: true}
	st.sessions["x-1"] = domain.SessionRecord{ID: "x-1", ProjectID: "x", Kind: domain.KindWorker}
	got, err := m.PromptSessions(ctx, "mer", PromptRoleWorker)
	if err != nil || len(got) != 1 || got[0].SessionID != "mer-1" {
		t.Fatalf("project listing = %+v, %v; want only mer-1", got, err)
	}
	all, err := m.PromptSessions(ctx, "", PromptRoleWorker)
	if err != nil || len(all) != 2 {
		t.Fatalf("all-project listing = %d sessions, %v; want 2", len(all), err)
	}
}
