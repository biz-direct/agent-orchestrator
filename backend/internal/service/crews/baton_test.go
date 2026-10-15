package crews_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/crews"
)

// fakeBaton keeps the holder and history under one lock, like the real store's
// transaction does.
type fakeBaton struct {
	mu     sync.Mutex
	holder domain.BatonHolder
	live   map[string]bool
	events []domain.BatonEvent
}

func (f *fakeBaton) GetCrewBaton(context.Context, domain.SessionID) (domain.BatonHolder, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current(), nil
}

func (f *fakeBaton) current() domain.BatonHolder {
	if f.holder.Kind == "" {
		return domain.BatonHolder{Kind: domain.BatonHolderWorker}
	}
	return f.holder
}

func (f *fakeBaton) MoveCrewBaton(_ context.Context, _ domain.SessionID, to domain.BatonHolder, actor domain.BatonActor, reason string, now time.Time) (domain.BatonHolder, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	from := f.current()
	if to.Kind == domain.BatonHolderRole && !f.live[to.Role] {
		return from, false, domain.ErrBatonRoleNotLive
	}
	if from == to {
		return from, false, nil
	}
	f.holder = to
	f.events = append(f.events, domain.BatonEvent{From: from, To: to, Actor: actor, Reason: reason, CreatedAt: now})
	return from, true, nil
}

func (f *fakeBaton) ListCrewBatonEvents(context.Context, domain.SessionID) ([]domain.BatonEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.BatonEvent{}, f.events...), nil
}

type recordingMessenger struct {
	mu   sync.Mutex
	sent map[domain.SessionID][]string
}

func (r *recordingMessenger) Send(_ context.Context, id domain.SessionID, msg string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sent == nil {
		r.sent = map[domain.SessionID][]string{}
	}
	r.sent[id] = append(r.sent[id], msg)
	return nil
}

func batonService(t *testing.T) (*crews.Service, *fakeBaton, *recordingMessenger, *fakeHost) {
	t.Helper()
	svc, host, _ := newRoleFixture(t, allFiles)
	baton := &fakeBaton{live: map[string]bool{"builder": true}}
	msg := &recordingMessenger{}
	if _, err := svc.AddRole(context.Background(), "p-1", crews.AddRoleInput{Role: "builder"}); err != nil {
		t.Fatal(err)
	}
	return svc.WithBaton(baton, msg), baton, msg, host
}

func TestMoveBatonTellsEveryRoleWhetherItMayWrite(t *testing.T) {
	svc, _, msg, _ := batonService(t)
	ctx := context.Background()
	got, err := svc.GetBaton(ctx, "p-1")
	if err != nil || got.Holder.Kind != domain.BatonHolderWorker {
		t.Fatalf("the worker holds by default: %+v %v", got, err)
	}
	got, err = svc.MoveBaton(ctx, "p-1", crews.MoveBatonInput{To: domain.BatonHolder{Kind: domain.BatonHolderRole, Role: "builder"}, Reason: "implement it"})
	if err != nil || got.Holder.Role != "builder" || len(got.History) != 1 || got.History[0].Actor != domain.BatonActorHuman || got.History[0].Reason != "implement it" {
		t.Fatalf("move: %+v %v", got, err)
	}
	if n := msg.sent["p-att-builder"]; len(n) != 1 || !strings.Contains(n[0], "you now hold the write baton") {
		t.Fatalf("the new holder is told it may write: %v", n)
	}
	if n := msg.sent["p-1"]; len(n) != 1 || !strings.Contains(n[0], "read-only") || !strings.Contains(n[0], "role builder now holds") {
		t.Fatalf("the worker is told it is read-only: %v", n)
	}

	if _, err := svc.MoveBaton(ctx, "p-1", crews.MoveBatonInput{To: domain.BatonHolder{Kind: domain.BatonHolderHuman}}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []domain.SessionID{"p-1", "p-att-builder"} {
		if n := msg.sent[id]; !strings.Contains(n[len(n)-1], "human now holds") || !strings.Contains(n[len(n)-1], "read-only") {
			t.Fatalf("%s must be read-only while the human holds the baton: %v", id, n)
		}
	}
}

func TestMoveBatonValidation(t *testing.T) {
	svc, _, msg, _ := batonService(t)
	ctx := context.Background()
	cases := map[string]struct {
		owner domain.SessionID
		in    crews.MoveBatonInput
		want  string
	}{
		"unknown kind":    {"p-1", crews.MoveBatonInput{To: domain.BatonHolder{Kind: "robot"}}, "INVALID_CREW_BATON_HOLDER"},
		"role without id": {"p-1", crews.MoveBatonInput{To: domain.BatonHolder{Kind: domain.BatonHolderRole}}, "INVALID_CREW_BATON_HOLDER"},
		"worker with id":  {"p-1", crews.MoveBatonInput{To: domain.BatonHolder{Kind: domain.BatonHolderWorker, Role: "x"}}, "INVALID_CREW_BATON_HOLDER"},
		"role not live":   {"p-1", crews.MoveBatonInput{To: domain.BatonHolder{Kind: domain.BatonHolderRole, Role: "tester"}}, "CREW_BATON_ROLE_NOT_LIVE"},
		"unknown task":    {"p-9", crews.MoveBatonInput{To: domain.BatonHolder{Kind: domain.BatonHolderHuman}}, "SESSION_NOT_FOUND"},
		"long reason":     {"p-1", crews.MoveBatonInput{To: domain.BatonHolder{Kind: domain.BatonHolderHuman}, Reason: strings.Repeat("x", 501)}, "INVALID_CREW_BATON_REASON"},
	}
	for name, tc := range cases {
		if _, err := svc.MoveBaton(ctx, tc.owner, tc.in); apiCode(t, err) != tc.want {
			t.Errorf("%s: err=%v want %s", name, err, tc.want)
		}
	}
	if len(msg.sent) != 0 {
		t.Fatalf("refused moves tell nobody: %v", msg.sent)
	}
}

func TestMovingTheBatonToItsHolderIsSilent(t *testing.T) {
	svc, _, msg, _ := batonService(t)
	if _, err := svc.MoveBaton(context.Background(), "p-1", crews.MoveBatonInput{To: domain.BatonHolder{Kind: domain.BatonHolderWorker}}); err != nil {
		t.Fatal(err)
	}
	if len(msg.sent) != 0 {
		t.Fatalf("no move, no notices: %v", msg.sent)
	}
}

func TestRemovingTheHoldingRoleHandsTheBatonBackAndRecordsIt(t *testing.T) {
	svc, baton, msg, _ := batonService(t)
	ctx := context.Background()
	if _, err := svc.MoveBaton(ctx, "p-1", crews.MoveBatonInput{To: domain.BatonHolder{Kind: domain.BatonHolderRole, Role: "builder"}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.RemoveRole(ctx, "p-1", "builder"); err != nil {
		t.Fatal(err)
	}
	if h, _ := baton.GetCrewBaton(ctx, "p-1"); h.Kind != domain.BatonHolderWorker {
		t.Fatalf("the worker holds again: %+v", h)
	}
	last := baton.events[len(baton.events)-1]
	if last.Actor != domain.BatonActorSystem || !strings.Contains(last.Reason, "builder was removed") {
		t.Fatalf("the hand-back is recorded with its cause: %+v", last)
	}
	if n := msg.sent["p-1"]; !strings.Contains(n[len(n)-1], "you now hold the write baton") {
		t.Fatalf("the worker is told: %v", n)
	}
}

func TestBatonUnavailableWithoutAStore(t *testing.T) {
	svc, _, _ := newRoleFixture(t, allFiles)
	if _, err := svc.GetBaton(context.Background(), "p-1"); apiCode(t, err) != "CREW_BATON_UNAVAILABLE" {
		t.Fatalf("err = %v", err)
	}
}

// gatedMessenger blocks the first send until released.
type gatedMessenger struct {
	recordingMessenger
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (g *gatedMessenger) Send(ctx context.Context, id domain.SessionID, msg string) error {
	g.once.Do(func() {
		close(g.entered)
		<-g.release
	})
	return g.recordingMessenger.Send(ctx, id, msg)
}

// An older move stalled in delivery must not tell a role it may write after a
// newer move took the baton away.
func TestBatonNoticesKeepTheOrderOfMoves(t *testing.T) {
	svc, host, _ := newRoleFixture(t, allFiles)
	g := &gatedMessenger{entered: make(chan struct{}), release: make(chan struct{})}
	if _, err := svc.AddRole(context.Background(), "p-1", crews.AddRoleInput{Role: "builder"}); err != nil {
		t.Fatal(err)
	}
	_ = host
	svc.WithBaton(&fakeBaton{live: map[string]bool{"builder": true}}, g)
	ctx := context.Background()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = svc.MoveBaton(ctx, "p-1", crews.MoveBatonInput{To: domain.BatonHolder{Kind: domain.BatonHolderRole, Role: "builder"}})
	}()
	<-g.entered
	second := make(chan struct{})
	go func() {
		defer close(second)
		_, _ = svc.MoveBaton(ctx, "p-1", crews.MoveBatonInput{To: domain.BatonHolder{Kind: domain.BatonHolderHuman}})
	}()
	select {
	case <-second:
		t.Fatal("the second move must wait for the first move's notices")
	case <-time.After(50 * time.Millisecond):
	}
	close(g.release)
	<-done
	<-second
	n := g.sent["p-att-builder"]
	if len(n) != 2 || !strings.Contains(n[0], "you now hold") || !strings.Contains(n[1], "human now holds") {
		t.Fatalf("notices must follow move order, last must be the revocation: %v", n)
	}
}
