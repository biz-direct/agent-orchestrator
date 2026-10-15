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

func TestCrewRoleSessionsAreHiddenDedupedAndReleasedOnEnd(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedProject(t, s, "crw")
	owner, err := s.CreateSession(ctx, sampleRecord("crw"))
	if err != nil {
		t.Fatal(err)
	}
	rec := sampleRecord("crw")
	rec.Mode = domain.SessionModeChat
	marker := domain.CrewRoleMarker(owner.ID, "builder")
	role, err := s.CreateAttachedSessionForAttempt(ctx, rec, owner.ID, marker)
	if err != nil {
		t.Fatal(err)
	}

	for name, list := range map[string]func() ([]domain.SessionRecord, error){
		"ListSessions":    func() ([]domain.SessionRecord, error) { return s.ListSessions(ctx, "crw") },
		"ListAllSessions": func() ([]domain.SessionRecord, error) { return s.ListAllSessions(ctx) },
	} {
		got, err := list()
		if err != nil || len(got) != 1 || got[0].ID != owner.ID {
			t.Fatalf("%s must not list the role (board, reaper, SCM observers): %v %+v", name, err, got)
		}
	}
	if got, err := s.GetSessionCrewRole(ctx, role.ID); err != nil || got != "builder" {
		t.Fatalf("role = %q err=%v", got, err)
	}
	if got, _ := s.GetSessionCrewRole(ctx, owner.ID); got != "" {
		t.Fatalf("an ordinary worker has no role, got %q", got)
	}
	stage, err := s.CreateAttachedSessionForAttempt(ctx, sampleRecord("crw"), owner.ID, "attempt-1")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetSessionCrewRole(ctx, stage.ID); got != "" {
		t.Fatalf("a pipeline stage is not a crew role, got %q", got)
	}
	if id, ok, _ := s.FindCrewRoleSession(ctx, owner.ID, "builder"); !ok || id != role.ID {
		t.Fatalf("find: %v %v", id, ok)
	}
	if _, err := s.CreateAttachedSessionForAttempt(ctx, sampleRecord("crw"), owner.ID, marker); err == nil {
		t.Fatal("a task holds one live session per role")
	}
	if err := s.ClearAttachedSessionAttempt(ctx, role.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAttachedSessionForAttempt(ctx, sampleRecord("crw"), owner.ID, marker); err != nil {
		t.Fatalf("a removed role can be added again: %v", err)
	}
}

func TestListLiveCrewRoleSessionIDsOnlyReturnsLiveRoles(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedProject(t, s, "lvr")
	owner, err := s.CreateSession(ctx, sampleRecord("lvr"))
	if err != nil {
		t.Fatal(err)
	}
	live, err := s.CreateAttachedSessionForAttempt(ctx, sampleRecord("lvr"), owner.ID, domain.CrewRoleMarker(owner.ID, "builder"))
	if err != nil {
		t.Fatal(err)
	}
	ended, err := s.CreateAttachedSessionForAttempt(ctx, sampleRecord("lvr"), owner.ID, domain.CrewRoleMarker(owner.ID, "tester"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAttachedSessionForAttempt(ctx, sampleRecord("lvr"), owner.ID, "pipeline-attempt-1"); err != nil {
		t.Fatal(err)
	}
	rec, _, _ := s.GetSession(ctx, ended.ID)
	rec.IsTerminated = true
	if err := s.UpdateSession(ctx, rec); err != nil {
		t.Fatal(err)
	}
	ids, err := s.ListLiveCrewRoleSessionIDs(ctx)
	if err != nil || len(ids) != 1 || ids[0] != live.ID {
		t.Fatalf("live roles only (no stage, no ended role): %v %v", ids, err)
	}
}
