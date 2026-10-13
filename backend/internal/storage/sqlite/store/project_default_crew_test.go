package store_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestSetProjectDefaultCrewPreservesOtherConfig(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	cfg := domain.ProjectConfig{
		AgentRules: "keep me",
		Env:        map[string]string{"A": "1"},
		AutoReview: true,
	}
	if err := s.UpsertProject(ctx, domain.ProjectRecord{ID: "crews", Path: "/tmp/crews", RegisteredAt: time.Now().UTC().Truncate(time.Second), Config: cfg}); err != nil {
		t.Fatal(err)
	}

	sel := &domain.CrewSelection{Mode: domain.CrewModeWorkflow, WorkflowID: "bt"}
	row, ok, err := s.SetProjectDefaultCrew(ctx, "crews", sel)
	if err != nil || !ok {
		t.Fatalf("set: ok=%v err=%v", ok, err)
	}
	if !reflect.DeepEqual(row.Config.DefaultCrew, sel) {
		t.Fatalf("returned selection: %+v", row.Config.DefaultCrew)
	}
	got, ok, err := s.GetProject(ctx, "crews")
	if err != nil || !ok {
		t.Fatal(err)
	}
	if got.Config.AgentRules != "keep me" || got.Config.Env["A"] != "1" || !got.Config.AutoReview {
		t.Fatalf("unrelated settings were replaced: %+v", got.Config)
	}
	if !reflect.DeepEqual(got.Config.DefaultCrew, sel) {
		t.Fatalf("persisted selection: %+v", got.Config.DefaultCrew)
	}

	if _, ok, err := s.SetProjectDefaultCrew(ctx, "crews", nil); err != nil || !ok {
		t.Fatalf("clear: ok=%v err=%v", ok, err)
	}
	got, _, _ = s.GetProject(ctx, "crews")
	if got.Config.DefaultCrew != nil || got.Config.AgentRules != "keep me" {
		t.Fatalf("clear must only remove the selection: %+v", got.Config)
	}
}

func TestSetProjectDefaultCrewUnknownProject(t *testing.T) {
	s := newTestStore(t)
	_, ok, err := s.SetProjectDefaultCrew(context.Background(), "ghost", &domain.CrewSelection{Mode: domain.CrewModeNoCrew})
	if err != nil || ok {
		t.Fatalf("unknown project: ok=%v err=%v", ok, err)
	}
}
