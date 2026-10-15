package store_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

func batonFixture(t *testing.T, roles ...string) (*sqlite.Store, domain.SessionID, map[string]domain.SessionID) {
	t.Helper()
	ctx := context.Background()
	s := newTestStore(t)
	seedProject(t, s, "crw")
	owner, err := s.CreateSession(ctx, sampleRecord("crw"))
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]domain.SessionID{}
	for _, role := range roles {
		rec := sampleRecord("crw")
		rec.Mode = domain.SessionModeChat
		created, err := s.CreateAttachedSessionForAttempt(ctx, rec, owner.ID, domain.CrewRoleMarker(owner.ID, role))
		if err != nil {
			t.Fatal(err)
		}
		ids[role] = created.ID
	}
	return s, owner.ID, ids
}

func role(id string) domain.BatonHolder {
	return domain.BatonHolder{Kind: domain.BatonHolderRole, Role: id}
}

func TestBatonDefaultsToTheWorkerAndRecordsEveryMove(t *testing.T) {
	ctx := context.Background()
	s, owner, _ := batonFixture(t, "builder", "tester")
	if got, err := s.GetCrewBaton(ctx, owner); err != nil || got.Kind != domain.BatonHolderWorker {
		t.Fatalf("default holder = %+v err=%v", got, err)
	}
	now := time.Now().UTC()
	from, changed, err := s.MoveCrewBaton(ctx, owner, role("builder"), domain.BatonActorHuman, "build it", now)
	if err != nil || !changed || from.Kind != domain.BatonHolderWorker {
		t.Fatalf("move: from=%+v changed=%v err=%v", from, changed, err)
	}
	// Re-giving the baton to its holder changes and records nothing.
	if _, changed, err := s.MoveCrewBaton(ctx, owner, role("builder"), domain.BatonActorHuman, "again", now); err != nil || changed {
		t.Fatalf("no-op move: changed=%v err=%v", changed, err)
	}
	if _, _, err := s.MoveCrewBaton(ctx, owner, domain.BatonHolder{Kind: domain.BatonHolderHuman}, domain.BatonActorHuman, "mine", now); err != nil {
		t.Fatal(err)
	}
	events, err := s.ListCrewBatonEvents(ctx, owner)
	if err != nil || len(events) != 2 {
		t.Fatalf("events: %v %+v", err, events)
	}
	if events[0].From.Kind != domain.BatonHolderWorker || events[0].To != role("builder") || events[0].Actor != domain.BatonActorHuman || events[0].Reason != "build it" {
		t.Fatalf("first event: %+v", events[0])
	}
	if events[1].From != role("builder") || events[1].To.Kind != domain.BatonHolderHuman {
		t.Fatalf("second event: %+v", events[1])
	}
}

func TestBatonRefusesARoleWithoutALiveSession(t *testing.T) {
	ctx := context.Background()
	s, owner, ids := batonFixture(t, "builder")
	if _, _, err := s.MoveCrewBaton(ctx, owner, role("ghost"), domain.BatonActorHuman, "", time.Now()); !errors.Is(err, domain.ErrBatonRoleNotLive) {
		t.Fatalf("ghost role: %v", err)
	}
	if _, _, err := s.MoveCrewBaton(ctx, owner, domain.BatonHolder{Kind: domain.BatonHolderRole}, domain.BatonActorHuman, "", time.Now()); err == nil {
		t.Fatal("a role holder needs a role id")
	}
	if _, _, err := s.MoveCrewBaton(ctx, owner, role("builder"), domain.BatonActorHuman, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	// A holder whose session ended holds nothing: the worker is the holder again.
	rec, _, _ := s.GetSession(ctx, ids["builder"])
	rec.IsTerminated = true
	if err := s.UpdateSession(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetCrewBaton(ctx, owner); got.Kind != domain.BatonHolderWorker {
		t.Fatalf("dead holder must not keep the baton: %+v", got)
	}
}

// Concurrent requests serialize: the table can never hold two holders, and the
// history is one unbroken chain (each move starts where the previous ended).
func TestBatonSingleHolderUnderConcurrentMoves(t *testing.T) {
	ctx := context.Background()
	s, owner, _ := batonFixture(t, "a", "b", "c")
	targets := []domain.BatonHolder{role("a"), role("b"), role("c"), {Kind: domain.BatonHolderHuman}, {Kind: domain.BatonHolderWorker}}
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, _, err := s.MoveCrewBaton(ctx, owner, targets[i%len(targets)], domain.BatonActorHuman, fmt.Sprint(i), time.Now()); err != nil {
				t.Errorf("move %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	events, err := s.ListCrewBatonEvents(ctx, owner)
	if err != nil || len(events) == 0 {
		t.Fatalf("events: %v %d", err, len(events))
	}
	prev := domain.BatonHolder{Kind: domain.BatonHolderWorker}
	for i, e := range events {
		if e.From != prev {
			t.Fatalf("event %d starts at %+v but the previous move ended at %+v", i, e.From, prev)
		}
		prev = e.To
	}
	if got, _ := s.GetCrewBaton(ctx, owner); got != prev {
		t.Fatalf("holder %+v != last event target %+v", got, prev)
	}
}

func TestResolveNudgeTargetRoutesToTheHolder(t *testing.T) {
	ctx := context.Background()
	s, owner, ids := batonFixture(t, "builder")
	check := func(name string, want domain.SessionID, wantDeliver bool) {
		t.Helper()
		got, deliver, err := s.ResolveNudgeTarget(ctx, owner)
		if err != nil || got != want || deliver != wantDeliver {
			t.Fatalf("%s: target=%q deliver=%v err=%v, want %q/%v", name, got, deliver, err, want, wantDeliver)
		}
	}
	check("worker holds", owner, true)
	_, _, _ = s.MoveCrewBaton(ctx, owner, role("builder"), domain.BatonActorHuman, "", time.Now())
	check("role holds", ids["builder"], true)
	_, _, _ = s.MoveCrewBaton(ctx, owner, domain.BatonHolder{Kind: domain.BatonHolderHuman}, domain.BatonActorHuman, "", time.Now())
	check("human holds", "", false)
}

// A role session that ended while holding the baton hands it back durably, and a
// replacement session for the same role does not inherit the grant.
func TestReplacementRoleDoesNotInheritAnEndedHoldersBaton(t *testing.T) {
	ctx := context.Background()
	s, owner, ids := batonFixture(t, "builder")
	now := time.Now().UTC()
	if _, _, err := s.MoveCrewBaton(ctx, owner, role("builder"), domain.BatonActorHuman, "build", now); err != nil {
		t.Fatal(err)
	}
	rec, _, _ := s.GetSession(ctx, ids["builder"])
	rec.IsTerminated = true
	if err := s.UpdateSession(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearAttachedSessionAttempt(ctx, ids["builder"]); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetCrewBaton(ctx, owner); got.Kind != domain.BatonHolderWorker {
		t.Fatalf("an ended holder no longer holds: %+v", got)
	}
	// Explicitly giving the baton to the worker persists the hand-back.
	if _, _, err := s.MoveCrewBaton(ctx, owner, domain.BatonHolder{Kind: domain.BatonHolderWorker}, domain.BatonActorHuman, "", now); err != nil {
		t.Fatal(err)
	}
	fresh := sampleRecord("crw")
	fresh.Mode = domain.SessionModeChat
	if _, err := s.CreateAttachedSessionForAttempt(ctx, fresh, owner, domain.CrewRoleMarker(owner, "builder")); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetCrewBaton(ctx, owner); got.Kind != domain.BatonHolderWorker {
		t.Fatalf("a replacement role must not resurrect the old grant: %+v", got)
	}
	events, _ := s.ListCrewBatonEvents(ctx, owner)
	last := events[len(events)-1]
	if last.Actor != domain.BatonActorSystem || last.To.Kind != domain.BatonHolderWorker {
		t.Fatalf("the hand-back is recorded: %+v", events)
	}
}
