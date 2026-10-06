package pipelines_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/pipeline"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/pipelines"
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

func (f *fakeStore) SetProjectDefaultPipeline(_ context.Context, id string, sel *domain.PipelineSelection) (domain.ProjectRecord, bool, error) {
	if id != f.row.ID {
		return domain.ProjectRecord{}, false, nil
	}
	f.row.Config.DefaultPipeline = sel
	return f.row, true, nil
}

const profileYAML = `version: 1
id: tester
description: Tester
instructions: test things
allowedPaths: ["**/*_test.go"]
`

const workflowYAML = `version: 1
id: bt
description: Build then test
stages:
  - {id: build, kind: build}
  - {id: review, kind: review, repairTo: build}
`

func newFixture(t *testing.T, files map[string]string) (*pipelines.Service, *fakeStore) {
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
	return pipelines.New(st), st
}

func apiCode(t *testing.T, err error) string {
	t.Helper()
	var ae *apierr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("want apierr, got %v", err)
	}
	return ae.Code
}

func TestCatalogReportsValidAndUnavailable(t *testing.T) {
	svc, _ := newFixture(t, map[string]string{
		pipeline.ProfilesDir + "/tester.yaml": profileYAML,
		pipeline.WorkflowsDir + "/bt.yaml":    workflowYAML,
	})
	got, err := svc.Catalog(context.Background(), "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Profiles) != 1 || len(got.Workflows) != 1 || !got.Workflows[0].Valid {
		t.Fatalf("catalog: %+v", got)
	}
	if got.Workflows[0].Executable || got.Workflows[0].UnavailableReason == "" {
		t.Fatal("valid workflows must be reported unavailable until execution ships")
	}
	if got.Default.State != pipelines.StateUnset {
		t.Fatalf("default: %+v", got.Default)
	}
}

func TestSetDefaultPersistsWithoutReplacingOtherSettings(t *testing.T) {
	svc, st := newFixture(t, map[string]string{
		pipeline.ProfilesDir + "/tester.yaml": profileYAML,
		pipeline.WorkflowsDir + "/bt.yaml":    workflowYAML,
	})
	sel := &domain.PipelineSelection{Mode: domain.PipelineModeWorkflow, WorkflowID: "bt"}
	status, err := svc.SetDefault(context.Background(), "p", pipelines.SetDefaultInput{Selection: sel})
	if err != nil {
		t.Fatal(err)
	}
	if status.State != pipelines.StateWorkflowUnavailable || status.Executable {
		t.Fatalf("selected workflow must read as visibly unavailable: %+v", status)
	}
	if st.row.Config.AgentRules != "keep" || st.row.Config.DefaultPipeline == nil {
		t.Fatalf("config: %+v", st.row.Config)
	}
	read, err := svc.GetDefault(context.Background(), "p")
	if err != nil || read.Selection == nil || read.Selection.WorkflowID != "bt" {
		t.Fatalf("read back: %+v err=%v", read, err)
	}
}

func TestSetDefaultExplicitNormalWorkerAndClear(t *testing.T) {
	svc, st := newFixture(t, nil)
	status, err := svc.SetDefault(context.Background(), "p", pipelines.SetDefaultInput{Selection: &domain.PipelineSelection{Mode: domain.PipelineModeNormalWorker}})
	if err != nil || status.State != pipelines.StateNormalWorker {
		t.Fatalf("normal worker: %+v err=%v", status, err)
	}
	if st.row.Config.DefaultPipeline == nil {
		t.Fatal("explicit normal worker choice must persist, distinct from unset")
	}
	status, err = svc.SetDefault(context.Background(), "p", pipelines.SetDefaultInput{})
	if err != nil || status.State != pipelines.StateUnset || st.row.Config.DefaultPipeline != nil {
		t.Fatalf("clear: %+v err=%v", status, err)
	}
}

func TestSetDefaultRejectsMissingAndInvalidWorkflows(t *testing.T) {
	svc, st := newFixture(t, map[string]string{
		pipeline.WorkflowsDir + "/bad.yaml": "version: 3\nid: bad\ndescription: x\nstages:\n  - {id: build, kind: build}\n",
	})
	for name, tc := range map[string]struct{ id, code string }{
		"missing": {"ghost", "PIPELINE_WORKFLOW_NOT_FOUND"},
		"invalid": {"bad", "PIPELINE_WORKFLOW_INVALID"},
		"badid":   {"Bad_ID", "INVALID_PIPELINE_SELECTION"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.SetDefault(context.Background(), "p", pipelines.SetDefaultInput{Selection: &domain.PipelineSelection{Mode: domain.PipelineModeWorkflow, WorkflowID: tc.id}})
			if apiCode(t, err) != tc.code {
				t.Fatalf("want %s, got %v", tc.code, err)
			}
			if st.row.Config.DefaultPipeline != nil {
				t.Fatal("rejected selections must not persist")
			}
		})
	}
}

func TestStoredSelectionBecomesMissingWhenDefinitionRemoved(t *testing.T) {
	svc, st := newFixture(t, nil)
	st.row.Config.DefaultPipeline = &domain.PipelineSelection{Mode: domain.PipelineModeWorkflow, WorkflowID: "gone"}
	status, err := svc.GetDefault(context.Background(), "p")
	if err != nil {
		t.Fatal(err)
	}
	if status.State != pipelines.StateWorkflowMissing || status.Executable || !strings.Contains(status.Message, "gone") {
		t.Fatalf("status: %+v", status)
	}
}

func TestUnknownProject(t *testing.T) {
	svc, _ := newFixture(t, nil)
	if _, err := svc.Catalog(context.Background(), "nope"); apiCode(t, err) != "PROJECT_NOT_FOUND" {
		t.Fatalf("got %v", err)
	}
	if _, err := svc.SetDefault(context.Background(), "nope", pipelines.SetDefaultInput{}); apiCode(t, err) != "PROJECT_NOT_FOUND" {
		t.Fatalf("got %v", err)
	}
}
