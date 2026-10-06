package pipelineruns

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/pipeline"
)

func discoverMultiStage(t *testing.T) (pipeline.Catalog, pipeline.WorkflowEntry) {
	t.Helper()
	root := t.TempDir()
	write := func(rel, body string) {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".ao/pipelines/profiles/tester.yaml", "version: 1\nid: tester\ndescription: T\ninstructions: be careful\nharness: codex\nmodel: gpt-x\nallowedPaths: [\"**/*_test.go\"]\nvalidation:\n  - {id: unit, command: go test ./...}\n")
	write(".ao/pipelines/workflows/bt.yaml", "version: 1\nid: bt\ndescription: d\nstages:\n  - {id: build, kind: build}\n  - {id: test, kind: specialist, profile: tester, repairTo: build}\n")
	cat := pipeline.Discover(root)
	entry, ok := cat.FindWorkflow("bt")
	if !ok || !entry.Valid {
		t.Fatalf("fixture workflow invalid: %+v", entry.Diagnostics)
	}
	return cat, entry
}

func TestSnapshotResolvesSettingsAndPrefersExplicitUserOverrides(t *testing.T) {
	cat, entry := discoverMultiStage(t)
	worker := domain.SessionRecord{Harness: domain.HarnessClaudeCode, Metadata: domain.SessionMetadata{Model: "opus"}}
	now := time.Unix(1_700_000_000, 0)

	snap, _, sum, err := buildSnapshot(snapshotInput{Workflow: entry, Catalog: cat, Worker: worker, RequestedBy: domain.PipelineRequestedByOrchestrator, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	build, test := snap.Stages[0], snap.Stages[1]
	if build.Harness != "claude-code" || build.Model != "opus" || build.SettingsSource != SourceWorker {
		t.Fatalf("build keeps the worker's own settings: %+v", build)
	}
	if test.Harness != "codex" || test.Model != "gpt-x" || test.SettingsSource != SourceProfile {
		t.Fatalf("specialist uses profile defaults when nobody overrides: %+v", test)
	}
	if len(snap.Profiles) != 1 || snap.Profiles[0].Instructions != "be careful" || snap.Profiles[0].AllowedPaths[0] != "**/*_test.go" || snap.Profiles[0].Validation[0].Command != "go test ./..." || len(snap.Profiles[0].DefinitionSHA256) != 64 {
		t.Fatalf("profile contents and provenance must be frozen: %+v", snap.Profiles)
	}

	over, _, sum2, err := buildSnapshot(snapshotInput{
		Workflow: entry, Catalog: cat, Worker: worker, RequestedBy: domain.PipelineRequestedByUser, Now: now,
		Overrides: map[string]StageOverride{"test": {Model: "gpt-y"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := over.Stages[1]; got.Harness != "codex" || got.Model != "gpt-y" || got.SettingsSource != SourceUser {
		t.Fatalf("explicit user choice wins field by field: %+v", got)
	}
	if over.Profiles[0].Instructions != "be careful" || len(over.Profiles[0].AllowedPaths) != 1 {
		t.Fatal("overrides never weaken mandatory instructions or path constraints")
	}
	if sum == sum2 {
		t.Fatal("different resolved settings must hash differently")
	}

	if _, _, _, err := buildSnapshot(snapshotInput{Workflow: entry, Catalog: cat, Worker: worker, Overrides: map[string]StageOverride{"ghost": {Model: "x"}}, Now: now}); err == nil || !strings.Contains(err.Error(), "unknown stage") {
		t.Fatalf("unknown stage override: %v", err)
	}
	if _, _, _, err := buildSnapshot(snapshotInput{Workflow: entry, Catalog: cat, Worker: worker, Overrides: map[string]StageOverride{"build": {Model: "x"}}, Now: now}); err == nil {
		t.Fatal("the regular worker cannot be overridden")
	}
}

func TestSnapshotIsStableForTheSameInput(t *testing.T) {
	cat, entry := discoverMultiStage(t)
	in := snapshotInput{Workflow: entry, Catalog: cat, Worker: domain.SessionRecord{Harness: domain.HarnessClaudeCode}, RequestedBy: domain.PipelineRequestedByUser, Now: time.Unix(1_700_000_000, 0)}
	_, a, sumA, _ := buildSnapshot(in)
	_, b, sumB, _ := buildSnapshot(in)
	if a != b || sumA != sumB {
		t.Fatal("snapshots must be deterministic so retries and restarts see the same frozen input")
	}
}
