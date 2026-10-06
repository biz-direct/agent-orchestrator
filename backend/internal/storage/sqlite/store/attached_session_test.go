package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestAttachedSessionsAreHiddenFromEverySessionListing(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedProject(t, s, "att")
	owner, err := s.CreateSession(ctx, sampleRecord("att"))
	if err != nil {
		t.Fatal(err)
	}
	rec := sampleRecord("att")
	rec.Mode = domain.SessionModeChat
	rec.CreatedAt = time.Now().UTC().Truncate(time.Second)
	attached, err := s.CreateAttachedSession(ctx, rec, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if attached.ID == owner.ID || attached.ID == "" {
		t.Fatalf("attached id: %q", attached.ID)
	}

	for name, list := range map[string]func() ([]domain.SessionRecord, error){
		"ListSessions":    func() ([]domain.SessionRecord, error) { return s.ListSessions(ctx, "att") },
		"ListAllSessions": func() ([]domain.SessionRecord, error) { return s.ListAllSessions(ctx) },
	} {
		got, err := list()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(got) != 1 || got[0].ID != owner.ID {
			t.Fatalf("%s must hide attached rows from every consumer (board, reaper, observers): %+v", name, got)
		}
	}
	// It is still addressable by id, which is how its conversation is served.
	if _, ok, err := s.GetSession(ctx, attached.ID); err != nil || !ok {
		t.Fatalf("attached row must be readable by id: ok=%v err=%v", ok, err)
	}
	if got, err := s.GetSessionAttachedTo(ctx, attached.ID); err != nil || got != owner.ID {
		t.Fatalf("owner = %q err=%v", got, err)
	}
	if got, _ := s.GetSessionAttachedTo(ctx, owner.ID); got != "" {
		t.Fatalf("an ordinary session has no owner, got %q", got)
	}
	ids, _ := s.ListAttachedSessionIDs(ctx, owner.ID)
	if len(ids) != 1 || ids[0] != attached.ID {
		t.Fatalf("attached ids: %v", ids)
	}
}
