package store_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestSetProjectDefaultPipelinePreservesOtherConfig(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	cfg := domain.ProjectConfig{
		AgentRules: "keep me",
		Env:        map[string]string{"A": "1"},
		AutoReview: true,
	}
	if err := s.UpsertProject(ctx, domain.ProjectRecord{ID: "pipes", Path: "/tmp/pipes", RegisteredAt: time.Now().UTC().Truncate(time.Second), Config: cfg}); err != nil {
		t.Fatal(err)
	}

	sel := &domain.PipelineSelection{Mode: domain.PipelineModeWorkflow, WorkflowID: "build-test-review"}
	row, ok, err := s.SetProjectDefaultPipeline(ctx, "pipes", sel)
	if err != nil || !ok {
		t.Fatalf("set: ok=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(row.Config.DefaultPipeline, sel) {
		t.Fatalf("returned selection: %+v", row.Config.DefaultPipeline)
	}
	got, ok, err := s.GetProject(ctx, "pipes")
	if err != nil || !ok {
		t.Fatal(err)
	}
	if got.Config.AgentRules != "keep me" || got.Config.Env["A"] != "1" || !got.Config.AutoReview {
		t.Fatalf("unrelated settings were replaced: %+v", got.Config)
	}
	if !reflect.DeepEqual(got.Config.DefaultPipeline, sel) {
		t.Fatalf("persisted selection: %+v", got.Config.DefaultPipeline)
	}

	if _, ok, err := s.SetProjectDefaultPipeline(ctx, "pipes", nil); err != nil || !ok {
		t.Fatalf("clear: ok=%v err=%v", ok, err)
	}
	got, _, _ = s.GetProject(ctx, "pipes")
	if got.Config.DefaultPipeline != nil || got.Config.AgentRules != "keep me" {
		t.Fatalf("clear must only remove the selection: %+v", got.Config)
	}
}

func TestSetProjectDefaultPipelineUnknownProject(t *testing.T) {
	s := newTestStore(t)
	_, ok, err := s.SetProjectDefaultPipeline(context.Background(), "ghost", &domain.PipelineSelection{Mode: domain.PipelineModeNormalWorker})
	if err != nil || ok {
		t.Fatalf("unknown project: ok=%v err=%v", ok, err)
	}
}
