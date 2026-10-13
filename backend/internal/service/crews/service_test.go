package crews_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/crew"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/crews"
)

type fakeStore struct {
	row domain.ProjectRecord
}

func (f *fakeStore) GetProject(_ context.Context, id string) (domain.ProjectRecord, bool, error) {
	if id != f.row.ID {
		return domain.ProjectRecord{}, false, nil
	}
	return f.row, true, nil
}

func (f *fakeStore) SetProjectDefaultCrew(_ context.Context, id string, sel *domain.CrewSelection) (domain.ProjectRecord, bool, error) {
	if id != f.row.ID {
		return domain.ProjectRecord{}, false, nil
	}
	f.row.Config.DefaultCrew = sel
	return f.row, true, nil
}

const builderYAML = `version: 1
id: builder
description: Builder
instructions: build things
`

const testerYAML = `version: 1
id: tester
description: Tester
instructions: test things
allowedPaths: ["e2e/**"]
`

const workflowYAML = `version: 1
id: bt
description: Build then test
roles: [builder, tester]
order: [builder, tester, review]
returns:
  tester: [builder]
`

const badWorkflowYAML = `version: 1
id: bad
description: Broken
roles: [builder, ghost]
order: [builder, ghost]
`

func newFixture(t *testing.T, files map[string]string) (*crews.Service, *fakeStore) {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st := &fakeStore{row: domain.ProjectRecord{ID: "p", Path: root, Config: domain.ProjectConfig{AgentRules: "keep"}}}
	return crews.New(st), st
}

func apiCode(t *testing.T, err error) string {
	t.Helper()
	var ae *apierr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("want apierr, got %v", err)
	}
	return ae.Code
}

var allFiles = map[string]string{
	crew.RolesDir + "/builder.yaml": builderYAML,
	crew.RolesDir + "/tester.yaml":  testerYAML,
	crew.WorkflowsDir + "/bt.yaml":  workflowYAML,
}

func TestCatalogListsRolesWorkflowsAndDefault(t *testing.T) {
	svc, _ := newFixture(t, allFiles)
	got, err := svc.Catalog(context.Background(), "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Roles) != 2 || len(got.Workflows) != 1 || !got.Workflows[0].Valid {
		t.Fatalf("catalog: %+v", got)
	}
	if got.Default.State != crews.StateUnset {
		t.Fatalf("default: %+v", got.Default)
	}
}

func TestCatalogUnknownProject(t *testing.T) {
	svc, _ := newFixture(t, nil)
	if _, err := svc.Catalog(context.Background(), "ghost"); apiCode(t, err) != "PROJECT_NOT_FOUND" {
		t.Fatalf("err = %v", err)
	}
}

func TestSetDefaultPersistsWithoutReplacingOtherSettings(t *testing.T) {
	svc, st := newFixture(t, allFiles)
	sel := &domain.CrewSelection{Mode: domain.CrewModeWorkflow, WorkflowID: "bt"}
	status, err := svc.SetDefault(context.Background(), "p", crews.SetDefaultInput{Selection: sel})
	if err != nil {
		t.Fatal(err)
	}
	if status.State != crews.StateWorkflowAvailable {
		t.Fatalf("a valid selected workflow reads as available: %+v", status)
	}
	if st.row.Config.AgentRules != "keep" || st.row.Config.DefaultCrew == nil {
		t.Fatalf("config: %+v", st.row.Config)
	}
	read, err := svc.GetDefault(context.Background(), "p")
	if err != nil || read.Selection == nil || read.Selection.WorkflowID != "bt" {
		t.Fatalf("read back: %+v err=%v", read, err)
	}
}

func TestSetDefaultExplicitNoCrewAndClear(t *testing.T) {
	svc, st := newFixture(t, nil)
	status, err := svc.SetDefault(context.Background(), "p", crews.SetDefaultInput{Selection: &domain.CrewSelection{Mode: domain.CrewModeNoCrew}})
	if err != nil || status.State != crews.StateNoCrew {
		t.Fatalf("no crew: %+v err=%v", status, err)
	}
	if st.row.Config.DefaultCrew == nil {
		t.Fatal("explicit no-crew choice must persist, distinct from unset")
	}
	status, err = svc.SetDefault(context.Background(), "p", crews.SetDefaultInput{})
	if err != nil || status.State != crews.StateUnset || st.row.Config.DefaultCrew != nil {
		t.Fatalf("clear: %+v err=%v", status, err)
	}
	if st.row.Config.AgentRules != "keep" {
		t.Fatalf("clear must not touch other settings: %+v", st.row.Config)
	}
}

func TestSetDefaultRejectsMissingInvalidAndMalformedSelections(t *testing.T) {
	files := map[string]string{
		crew.RolesDir + "/builder.yaml": builderYAML,
		crew.WorkflowsDir + "/bad.yaml": badWorkflowYAML,
	}
	svc, st := newFixture(t, files)
	ctx := context.Background()
	cases := []struct {
		name string
		sel  *domain.CrewSelection
		code string
	}{
		{"missing", &domain.CrewSelection{Mode: domain.CrewModeWorkflow, WorkflowID: "nope"}, "CREW_WORKFLOW_NOT_FOUND"},
		{"invalid", &domain.CrewSelection{Mode: domain.CrewModeWorkflow, WorkflowID: "bad"}, "CREW_WORKFLOW_INVALID"},
		{"no workflow id", &domain.CrewSelection{Mode: domain.CrewModeWorkflow}, "INVALID_CREW_SELECTION"},
		{"no-crew with id", &domain.CrewSelection{Mode: domain.CrewModeNoCrew, WorkflowID: "x"}, "INVALID_CREW_SELECTION"},
		{"unknown mode", &domain.CrewSelection{Mode: "bogus"}, "INVALID_CREW_SELECTION"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.SetDefault(ctx, "p", crews.SetDefaultInput{Selection: tc.sel})
			if got := apiCode(t, err); got != tc.code {
				t.Fatalf("code = %s, want %s", got, tc.code)
			}
			if st.row.Config.DefaultCrew != nil {
				t.Fatal("a rejected selection must not be stored")
			}
		})
	}
}

func TestDefaultResolutionTracksRepositoryEdits(t *testing.T) {
	svc, st := newFixture(t, allFiles)
	ctx := context.Background()
	if _, err := svc.SetDefault(ctx, "p", crews.SetDefaultInput{Selection: &domain.CrewSelection{Mode: domain.CrewModeWorkflow, WorkflowID: "bt"}}); err != nil {
		t.Fatal(err)
	}
	wf := filepath.Join(st.row.Path, filepath.FromSlash(crew.WorkflowsDir), "bt.yaml")
	if err := os.WriteFile(wf, []byte("version: 1\nid: bt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.GetDefault(ctx, "p"); got.State != crews.StateWorkflowInvalid {
		t.Fatalf("broken workflow: %+v", got)
	}
	if err := os.Remove(wf); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.GetDefault(ctx, "p")
	if got.State != crews.StateWorkflowMissing || got.Selection == nil {
		t.Fatalf("removed workflow stays selected but missing: %+v", got)
	}
}
