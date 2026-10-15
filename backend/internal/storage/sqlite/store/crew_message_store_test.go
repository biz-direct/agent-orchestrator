package store_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func crewMsg(id, key string, at time.Time) domain.CrewMessage {
	return domain.CrewMessage{
		ID: id, OwnerSessionID: "w-1", SenderRole: "builder", SenderSessionID: "w-att-1",
		TargetRole: "reviewer", TargetSessionID: "w-att-2", IdempotencyKey: key, Body: "hello",
		Mode: domain.CrewMessageModeQueue, CommitSHA: "abc", BatonHolder: "builder", CreatedAt: at,
	}
}

func TestCrewMessageCreateIsIdempotentUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	var wg sync.WaitGroup
	var mu sync.Mutex
	dupes, ids := 0, map[string]bool{}
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, dup, err := s.CreateCrewMessage(ctx, crewMsg(fmt.Sprintf("m-%d", i), "one-key", now), 0, time.Hour)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			ids[got.ID] = true
			if dup {
				dupes++
			}
		}()
	}
	wg.Wait()
	if len(ids) != 1 || dupes != 11 {
		t.Fatalf("one stored message, 11 duplicate answers: ids=%v dupes=%d", ids, dupes)
	}
}

func TestCrewMessageRateLimitIsAtomicPerPair(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := s.CreateCrewMessage(ctx, crewMsg(fmt.Sprintf("m-%d", i), fmt.Sprintf("k-%d", i), now), 4, time.Hour); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	msgs, err := s.ListCrewMessages(ctx, "w-1")
	if err != nil {
		t.Fatal(err)
	}
	queued, held := 0, 0
	for _, m := range msgs {
		switch m.State {
		case domain.CrewMessageQueued:
			queued++
		case domain.CrewMessageHeld:
			held++
		}
	}
	if queued != 4 || held != 6 {
		t.Fatalf("concurrent sends cannot slip under the limit: queued=%d held=%d", queued, held)
	}
	// Messages outside the window no longer count, and dropped ones never do.
	later := now.Add(2 * time.Hour)
	if got, _, _ := s.CreateCrewMessage(ctx, crewMsg("late", "late", later), 4, time.Hour); got.State != domain.CrewMessageQueued {
		t.Fatalf("a new window starts fresh: %+v", got)
	}
}

func TestCrewMessageClaimIsExclusiveAndGuardsPersonEdits(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	if _, _, err := s.CreateCrewMessage(ctx, crewMsg("m-1", "k", now), 0, time.Hour); err != nil {
		t.Fatal(err)
	}
	wins := 0
	var wg sync.WaitGroup
	var mu sync.Mutex
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.ClaimCrewMessage(ctx, "m-1", now)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if ok {
				wins++
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("exactly one claimer wins: %d", wins)
	}
	if ok, _ := s.EditCrewMessage(ctx, "m-1", "late edit", now); ok {
		t.Fatal("a claimed message is no longer editable")
	}
	if ok, _ := s.DropCrewMessage(ctx, "m-1", now); ok {
		t.Fatal("a claimed message is no longer droppable")
	}
	if n, err := s.RequeueStaleCrewMessages(ctx, now.Add(-time.Minute), now); err != nil || n != 0 {
		t.Fatalf("a fresh claim is not stale: %d %v", n, err)
	}
	if n, err := s.RequeueStaleCrewMessages(ctx, now.Add(time.Minute), now); err != nil || n != 1 {
		t.Fatalf("an old claim returns to the queue: %d %v", n, err)
	}
	if ok, _ := s.ClaimCrewMessage(ctx, "m-1", now); !ok {
		t.Fatal("a requeued message can be claimed again")
	}
	if ok, _ := s.MarkCrewMessageDelivered(ctx, "m-1", now); !ok {
		t.Fatal("delivered")
	}
	got, found, err := s.GetCrewMessage(ctx, "m-1")
	if err != nil || !found || got.State != domain.CrewMessageDelivered || got.DeliveredAt.IsZero() || got.Attempts != 2 {
		t.Fatalf("receipt: %+v %v %v", got, found, err)
	}
}
