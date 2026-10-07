package report

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

type fakeStore struct {
	session domain.SessionRecord
	ok      bool
	created domain.ReportRecord
	reports []domain.ReportRecord
}

func (f *fakeStore) GetSession(context.Context, domain.SessionID) (domain.SessionRecord, bool, error) {
	return f.session, f.ok, nil
}
func (f *fakeStore) CreateReport(_ context.Context, r domain.ReportRecord) (domain.ReportRecord, error) {
	f.created = r
	return r, nil
}
func (f *fakeStore) ListReportsByProject(context.Context, domain.ProjectID) ([]domain.ReportRecord, error) {
	return append([]domain.ReportRecord(nil), f.reports...), nil
}

func TestListProjectIsReadOnlyAndOrdered(t *testing.T) {
	at := time.Date(2026, 9, 15, 1, 2, 3, 0, time.UTC)
	outputs := []domain.ReportOutput{
		{Kind: domain.ReportOutputArtifact, Reference: "first"},
		{Kind: domain.ReportOutputArtifact, Reference: "second"},
	}
	st := &fakeStore{reports: []domain.ReportRecord{
		{ID: "c", CreatedAt: at.Add(time.Second)},
		{ID: "b", CreatedAt: at, Outputs: outputs},
		{ID: "a", CreatedAt: at},
	}}
	got, err := New(Deps{Store: st}).ListProject(context.Background(), "ao")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].ID != "a" || got[1].ID != "b" || got[2].ID != "c" {
		t.Fatalf("report order = %+v", got)
	}
	if len(got[1].Outputs) != 2 || got[1].Outputs[0].Reference != "first" || got[1].Outputs[1].Reference != "second" {
		t.Fatalf("output position order = %+v", got[1].Outputs)
	}
}

func TestCreateDerivesOwnershipAndFixedDeliveryDeadlines(t *testing.T) {
	now := time.Date(2026, 9, 7, 1, 2, 3, 0, time.UTC)
	tests := []struct {
		name      string
		input     CreateInput
		available time.Time
		settles   time.Time
	}{
		{name: "free form batches for one hour", input: CreateInput{Message: "status"}, available: now.Add(time.Hour)},
		{name: "output only batches for one hour", input: CreateInput{Outputs: []domain.ReportOutput{{Kind: domain.ReportOutputArtifact, Reference: "result"}}}, available: now.Add(time.Hour)},
		{name: "checkpoint batches for one hour", input: CreateInput{State: domain.ReportCheckpoint, Note: "checkpoint"}, available: now.Add(time.Hour)},
		{name: "needs input is immediate", input: CreateInput{State: domain.ReportNeedsInput, Note: "decision"}, available: now},
		{name: "stuck is immediate", input: CreateInput{State: domain.ReportStuck, Note: "blocked"}, available: now},
		{name: "done settles for five minutes", input: CreateInput{State: domain.ReportDone, Note: "finished"}, available: now.Add(5 * time.Minute), settles: now.Add(5 * time.Minute)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeStore{ok: true, session: domain.SessionRecord{ID: "ao-7", ProjectID: "ao", Kind: domain.KindWorker}}
			s := New(Deps{Store: st, Now: func() time.Time { return now }, NewID: func() string { return "rpt_1" }})
			tc.input.SessionID = "ao-7"
			r, err := s.Create(context.Background(), tc.input)
			if err != nil {
				t.Fatal(err)
			}
			if r.ID != "rpt_1" || r.ProjectID != "ao" || r.DeliveryState != domain.ReportPending || r.RepeatCount != 1 || !r.AvailableAt.Equal(tc.available) || !r.SettlementDeadline.Equal(tc.settles) {
				t.Fatalf("report=%+v", r)
			}
		})
	}
}

func TestCreateDefendsValidationAndOwnership(t *testing.T) {
	worker := &fakeStore{ok: true, session: domain.SessionRecord{ID: "ao-2", ProjectID: "ao", Kind: domain.KindWorker}}
	for _, tc := range []struct {
		name  string
		store *fakeStore
		input CreateInput
	}{
		{"missing session", worker, CreateInput{Message: "message"}},
		{"unknown session", &fakeStore{}, CreateInput{SessionID: "ao-2", State: domain.ReportDone, Note: "done"}},
		{"orchestrator", &fakeStore{ok: true, session: domain.SessionRecord{ID: "ao-1", ProjectID: "ao", Kind: domain.KindOrchestrator}}, CreateInput{SessionID: "ao-1", State: domain.ReportDone, Note: "done"}},
		{"bad state", worker, CreateInput{SessionID: "ao-2", State: "bad", Note: "note"}},
		{"bad PR", worker, CreateInput{SessionID: "ao-2", Outputs: []domain.ReportOutput{{Kind: domain.ReportOutputPRCreated, Reference: "https://example.com/x"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(Deps{Store: tc.store}).Create(context.Background(), tc.input)
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

type fakePipelineGuard struct{ unfinished bool }

func (g fakePipelineGuard) SuppressesLifecycleShortcuts(context.Context, domain.SessionID) bool {
	return g.unfinished
}

// A worker's own "done" is a claim, not completion, while its pipeline run is
// unfinished: it reaches the orchestrator as a labelled checkpoint instead.
func TestWorkerDoneDuringAnUnfinishedPipelineIsAnInformationalCheckpoint(t *testing.T) {
	now := time.Date(2026, 9, 7, 1, 2, 3, 0, time.UTC)
	newSvc := func(g fakePipelineGuard) (*Service, *fakeStore) {
		st := &fakeStore{ok: true, session: domain.SessionRecord{ID: "ao-7", ProjectID: "ao", Kind: domain.KindWorker}}
		return New(Deps{Store: st, Pipelines: g, Now: func() time.Time { return now }, NewID: func() string { return "rpt_1" }}), st
	}

	svc, st := newSvc(fakePipelineGuard{unfinished: true})
	got, err := svc.Create(context.Background(), CreateInput{SessionID: "ao-7", State: domain.ReportDone, Note: "all finished"})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.ReportCheckpoint || got.SettlementDeadline != (time.Time{}) || !got.AvailableAt.Equal(now.Add(domain.ReportBatchFallback)) {
		t.Fatalf("done must not open a completion settlement window mid-pipeline: %+v", got)
	}
	if got.Note == "all finished" || len(got.Note) < len("all finished") || !contains(got.Note, "not task completion") || !contains(got.Note, "all finished") {
		t.Fatalf("the worker's words are kept under an explanation: %q", got.Note)
	}
	if st.created.State != domain.ReportCheckpoint {
		t.Fatalf("stored: %+v", st.created)
	}

	// A very long note still fits the report limit.
	long := make([]rune, domain.MaxReportTextCharacters)
	for i := range long {
		long[i] = 'x'
	}
	got, err = svc.Create(context.Background(), CreateInput{SessionID: "ao-7", State: domain.ReportDone, Note: string(long)})
	if err != nil || len([]rune(got.Note)) > domain.MaxReportTextCharacters {
		t.Fatalf("note length %d err=%v", len([]rune(got.Note)), err)
	}

	// Other states are untouched, and a finished or absent pipeline changes nothing.
	got, _ = svc.Create(context.Background(), CreateInput{SessionID: "ao-7", State: domain.ReportStuck, Note: "blocked"})
	if got.State != domain.ReportStuck || got.Note != "blocked" {
		t.Fatalf("%+v", got)
	}
	for name, s := range map[string]*Service{"finished pipeline": mustSvc(newSvc(fakePipelineGuard{})), "no pipelines": New(Deps{Store: &fakeStore{ok: true, session: domain.SessionRecord{ID: "ao-7", ProjectID: "ao", Kind: domain.KindWorker}}, Now: func() time.Time { return now }})} {
		got, err := s.Create(context.Background(), CreateInput{SessionID: "ao-7", State: domain.ReportDone, Note: "finished"})
		if err != nil || got.State != domain.ReportDone || got.Note != "finished" {
			t.Fatalf("%s: %+v err=%v", name, got, err)
		}
	}
}

func mustSvc(s *Service, _ *fakeStore) *Service { return s }

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
