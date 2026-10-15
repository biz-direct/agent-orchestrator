package crews_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/crews"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

// msgStore is the real SQLite message store with role lookup answered from the
// fake session table, so tests need no session rows or attachment markers.
type msgStore struct {
	*sqlite.Store
	sessions *fakeSessions
}

func (m msgStore) FindCrewRoleSession(_ context.Context, owner domain.SessionID, role string) (domain.SessionID, bool, error) {
	for id, o := range m.sessions.attached {
		if o == owner && m.sessions.roles[id] == role {
			return id, true, nil
		}
	}
	return "", false, nil
}

type fakeDeliverer struct {
	mu        sync.Mutex
	delivered []ports.CrewMessageDelivery
	failures  []error // consumed one per call before succeeding
}

func (d *fakeDeliverer) DeliverCrewMessage(_ context.Context, m ports.CrewMessageDelivery) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.failures) > 0 {
		err := d.failures[0]
		d.failures = d.failures[1:]
		if err != nil {
			return err
		}
	}
	// Like the chat layer, a repeated client message id is accepted but lands once.
	for _, prior := range d.delivered {
		if prior.ClientMessageID == m.ClientMessageID {
			return nil
		}
	}
	d.delivered = append(d.delivered, m)
	return nil
}

func (d *fakeDeliverer) texts() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, 0, len(d.delivered))
	for _, m := range d.delivered {
		out = append(out, m.Text)
	}
	return out
}

type msgFixture struct {
	svc      *crews.Service
	sessions *fakeSessions
	store    msgStore
	out      *fakeDeliverer
	now      time.Time
	mu       sync.Mutex
}

func (f *msgFixture) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(time.Second)
	return f.now
}

func newMsgFixture(t *testing.T, rateLimit int) *msgFixture {
	t.Helper()
	svc, _ := newFixture(t, allFiles)
	live := func(id domain.SessionID, kind domain.SessionKind) domain.SessionRecord {
		return domain.SessionRecord{ID: id, ProjectID: "p", Kind: kind, Harness: domain.HarnessClaudeCode,
			Activity: domain.Activity{State: domain.ActivityIdle}, Metadata: domain.SessionMetadata{WorkspacePath: "/ws"}}
	}
	sessions := &fakeSessions{
		recs: map[domain.SessionID]domain.SessionRecord{
			"p-1": live("p-1", domain.KindWorker), "p-2": live("p-2", domain.KindWorker),
			"a": live("a", domain.KindWorker), "b": live("b", domain.KindWorker), "c": live("c", domain.KindWorker),
			"other": live("other", domain.KindWorker),
		},
		attached: map[domain.SessionID]domain.SessionID{"a": "p-1", "b": "p-1", "c": "p-1", "other": "p-2"},
		roles:    map[domain.SessionID]string{"a": "builder", "b": "reviewer", "c": "tester", "other": "reviewer"},
	}
	f := &msgFixture{sessions: sessions, out: &fakeDeliverer{}, now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	f.store = msgStore{Store: sqlitetest.MustOpen(t), sessions: sessions}
	var ids int
	var idMu sync.Mutex
	f.svc = svc.WithRoleSessions(sessions, &fakeHost{sessions: sessions}).WithMessages(f.store, f.out, crews.MessageOptions{
		Clock: f.clock,
		NewID: func() string {
			idMu.Lock()
			defer idMu.Unlock()
			ids++
			return fmt.Sprintf("m-%d", ids)
		},
		HeadCommit: func(context.Context, string) string { return "0123456789abcdef0123" },
		Baton:      func(context.Context, domain.SessionID) string { return "builder" },
		RateLimit:  rateLimit, RateWindow: time.Hour,
	})
	return f
}

func (f *msgFixture) setActivity(id domain.SessionID, state domain.ActivityState) {
	rec := f.sessions.recs[id]
	rec.Activity.State = state
	f.sessions.recs[id] = rec
}

func send(t *testing.T, f *msgFixture, from domain.SessionID, in crews.SendMessageInput) crews.CrewMessage {
	t.Helper()
	got, err := f.svc.SendMessage(context.Background(), from, in)
	if err != nil {
		t.Fatalf("send %+v: %v", in, err)
	}
	return got
}

func TestSendDeliversATaggedTurnToTheTarget(t *testing.T) {
	f := newMsgFixture(t, 0)
	got := send(t, f, "a", crews.SendMessageInput{To: "reviewer", Message: "please review", IdempotencyKey: "k1"})
	if got.State != "delivered" || got.SenderRole != "builder" || got.TargetRole != "reviewer" || got.Commit == "" || got.BatonHolder != "builder" {
		t.Fatalf("stored message: %+v", got)
	}
	if len(f.out.delivered) != 1 {
		t.Fatalf("deliveries: %+v", f.out.delivered)
	}
	d := f.out.delivered[0]
	if d.Target != "b" || d.Sender != "a" || d.ClientMessageID != got.ID || d.Steer {
		t.Fatalf("delivery: %+v", d)
	}
	for _, want := range []string{"from builder", "baton: builder", "commit 0123456789ab", "please review", "ao crew send --to builder"} {
		if !strings.Contains(d.Text, want) {
			t.Errorf("turn text lacks %q: %q", want, d.Text)
		}
	}
}

func TestSteerFlagReachesTheDelivery(t *testing.T) {
	f := newMsgFixture(t, 0)
	send(t, f, "a", crews.SendMessageInput{To: "reviewer", Message: "change of plan", Steer: true, IdempotencyKey: "k"})
	if len(f.out.delivered) != 1 || !f.out.delivered[0].Steer {
		t.Fatalf("a steer message is delivered as a steer: %+v", f.out.delivered)
	}
}

func TestIdempotentRetryDeliversOnce(t *testing.T) {
	f := newMsgFixture(t, 0)
	in := crews.SendMessageInput{To: "reviewer", Message: "once", IdempotencyKey: "same"}
	first := send(t, f, "a", in)
	again := send(t, f, "a", in)
	if again.ID != first.ID || !again.Duplicate || first.Duplicate {
		t.Fatalf("retry returns the original: first=%+v again=%+v", first, again)
	}
	if n := len(f.out.delivered); n != 1 {
		t.Fatalf("delivered %d times", n)
	}
	if _, err := f.svc.SendMessage(context.Background(), "a", crews.SendMessageInput{To: "tester", Message: "other", IdempotencyKey: "same"}); apiCode(t, err) != "CREW_MESSAGE_KEY_REUSED" {
		t.Fatalf("a key reused for another target is refused: %v", err)
	}
}

func TestConcurrentRequestsDeliverExactlyOnce(t *testing.T) {
	f := newMsgFixture(t, 0)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = f.svc.SendMessage(context.Background(), "a", crews.SendMessageInput{To: "reviewer", Message: "race", IdempotencyKey: "race"})
			f.svc.DeliverPending(context.Background())
		}()
	}
	wg.Wait()
	msgs, err := f.svc.ListMessages(context.Background(), "p-1")
	if err != nil || len(msgs) != 1 || msgs[0].State != "delivered" || len(f.out.delivered) != 1 {
		t.Fatalf("one message, delivered once: err=%v msgs=%+v deliveries=%d", err, msgs, len(f.out.delivered))
	}
}

func TestRestartResumesAnInterruptedDeliveryWithoutDuplicating(t *testing.T) {
	f := newMsgFixture(t, 0)
	f.setActivity("b", domain.ActivityBlocked)
	m := send(t, f, "a", crews.SendMessageInput{To: "reviewer", Message: "survive", IdempotencyKey: "k"})
	// A daemon that died mid-delivery leaves the row claimed.
	if ok, err := f.store.ClaimCrewMessage(context.Background(), m.ID, f.clock()); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	f.setActivity("b", domain.ActivityIdle)
	f.svc.Recover(context.Background())
	f.svc.Recover(context.Background())
	if n := len(f.out.delivered); n != 1 || f.out.delivered[0].ClientMessageID != m.ID {
		t.Fatalf("recovered delivery: %+v", f.out.delivered)
	}
	msgs, _ := f.svc.ListMessages(context.Background(), "p-1")
	if msgs[0].State != "delivered" {
		t.Fatalf("state: %+v", msgs[0])
	}
}

func TestBlockedTargetWaitsAndKeepsOrder(t *testing.T) {
	f := newMsgFixture(t, 0)
	f.setActivity("b", domain.ActivityBlocked)
	first := send(t, f, "a", crews.SendMessageInput{To: "reviewer", Message: "one", IdempotencyKey: "1"})
	send(t, f, "c", crews.SendMessageInput{To: "reviewer", Message: "two", IdempotencyKey: "2"})
	send(t, f, "a", crews.SendMessageInput{To: "reviewer", Message: "three", Steer: true, IdempotencyKey: "3"})
	if len(f.out.delivered) != 0 {
		t.Fatalf("nothing is injected behind a permission prompt: %+v", f.out.delivered)
	}
	msgs, _ := f.svc.ListMessages(context.Background(), "p-1")
	if len(msgs) != 3 || msgs[0].State != "queued" || !strings.Contains(msgs[0].StateReason, "blocked on a permission prompt") {
		t.Fatalf("queued with a reason: %+v", msgs)
	}
	f.svc.DeliverPending(context.Background())
	if len(f.out.delivered) != 0 {
		t.Fatal("still blocked")
	}
	f.setActivity("b", domain.ActivityActive) // busy, not blocked: queue/steer rules apply
	f.svc.DeliverPending(context.Background())
	texts := f.out.texts()
	if len(texts) != 3 || !strings.Contains(texts[0], "one") || !strings.Contains(texts[1], "two") || !strings.Contains(texts[2], "three") {
		t.Fatalf("delivered in send order: %q", texts)
	}
	if f.out.delivered[0].ClientMessageID != first.ID || f.out.delivered[2].Steer != true {
		t.Fatalf("modes survive the wait: %+v", f.out.delivered)
	}
}

func TestCrossCrewAndNonRoleSendsAreRefused(t *testing.T) {
	f := newMsgFixture(t, 0)
	ctx := context.Background()
	f.sessions.roles["other"] = "auditor" // a role that exists only in another task
	cases := map[string]struct {
		from domain.SessionID
		in   crews.SendMessageInput
		want string
	}{
		"role of another task":  {"a", crews.SendMessageInput{To: "auditor", Message: "x"}, "CREW_MESSAGE_TARGET_NOT_IN_CREW"},
		"unknown role":          {"a", crews.SendMessageInput{To: "ghost", Message: "x"}, "CREW_MESSAGE_TARGET_NOT_IN_CREW"},
		"the worker is no role": {"p-1", crews.SendMessageInput{To: "reviewer", Message: "x"}, "CREW_MESSAGE_SENDER_NOT_ROLE"},
		"unknown sender":        {"nope", crews.SendMessageInput{To: "reviewer", Message: "x"}, "SESSION_NOT_FOUND"},
		"self":                  {"a", crews.SendMessageInput{To: "builder", Message: "x"}, "CREW_MESSAGE_SELF"},
		"no target":             {"a", crews.SendMessageInput{Message: "x"}, "CREW_MESSAGE_TARGET_REQUIRED"},
		"empty":                 {"a", crews.SendMessageInput{To: "reviewer", Message: "  "}, "CREW_MESSAGE_EMPTY"},
		"oversized":             {"a", crews.SendMessageInput{To: "reviewer", Message: strings.Repeat("x", crews.MaxMessageBytes+1)}, "CREW_MESSAGE_TOO_LONG"},
	}
	for name, tc := range cases {
		if _, err := f.svc.SendMessage(ctx, tc.from, tc.in); apiCode(t, err) != tc.want {
			t.Errorf("%s: err=%v want %s", name, err, tc.want)
		}
	}
	// A target that ended is not addressable either.
	rec := f.sessions.recs["b"]
	rec.IsTerminated = true
	f.sessions.recs["b"] = rec
	if _, err := f.svc.SendMessage(ctx, "a", crews.SendMessageInput{To: "reviewer", Message: "x"}); apiCode(t, err) != "CREW_MESSAGE_TARGET_NOT_IN_CREW" {
		t.Errorf("ended target: %v", err)
	}
	if len(f.out.delivered) != 0 {
		t.Fatalf("refusals deliver nothing: %+v", f.out.delivered)
	}
}

func TestRateLimitHoldsExcessMessagesForThePerson(t *testing.T) {
	f := newMsgFixture(t, 3)
	ctx := context.Background()
	for i := range 3 {
		send(t, f, "a", crews.SendMessageInput{To: "reviewer", Message: fmt.Sprintf("m%d", i), IdempotencyKey: fmt.Sprint(i)})
	}
	held := send(t, f, "a", crews.SendMessageInput{To: "reviewer", Message: "too many", IdempotencyKey: "4"})
	if held.State != "held" || !strings.Contains(held.StateReason, "rate limit") || !held.Editable {
		t.Fatalf("held: %+v", held)
	}
	// Another pair is counted separately.
	if other := send(t, f, "a", crews.SendMessageInput{To: "tester", Message: "fine", IdempotencyKey: "5"}); other.State != "delivered" {
		t.Fatalf("other pair: %+v", other)
	}
	f.svc.DeliverPending(ctx)
	if len(f.out.delivered) != 4 {
		t.Fatalf("held messages are not delivered by the dispatcher: %d", len(f.out.delivered))
	}
	released, err := f.svc.ReleaseMessage(ctx, "p-1", held.ID)
	if err != nil || released.State != "delivered" || len(f.out.delivered) != 5 {
		t.Fatalf("release: %v %+v", err, released)
	}
	if _, err := f.svc.ReleaseMessage(ctx, "p-1", held.ID); apiCode(t, err) != "CREW_MESSAGE_NOT_PENDING" {
		t.Fatalf("a delivered message cannot be released: %v", err)
	}
}

func TestPersonCanEditOrDropAQueuedMessage(t *testing.T) {
	f := newMsgFixture(t, 0)
	ctx := context.Background()
	f.setActivity("b", domain.ActivityBlocked)
	edited := send(t, f, "a", crews.SendMessageInput{To: "reviewer", Message: "draft", IdempotencyKey: "e"})
	dropped := send(t, f, "a", crews.SendMessageInput{To: "reviewer", Message: "never mind", IdempotencyKey: "d"})
	if got, err := f.svc.EditMessage(ctx, "p-1", edited.ID, crews.EditMessageInput{Message: "final"}); err != nil || got.Message != "final" {
		t.Fatalf("edit: %v %+v", err, got)
	}
	if got, err := f.svc.DropMessage(ctx, "p-1", dropped.ID); err != nil || got.State != "dropped" {
		t.Fatalf("drop: %v %+v", err, got)
	}
	f.setActivity("b", domain.ActivityIdle)
	f.svc.DeliverPending(ctx)
	texts := f.out.texts()
	if len(texts) != 1 || !strings.Contains(texts[0], "final") || strings.Contains(texts[0], "draft") {
		t.Fatalf("only the edited message goes out: %q", texts)
	}
	for name, err := range map[string]error{
		"edit delivered": func() error {
			_, e := f.svc.EditMessage(ctx, "p-1", edited.ID, crews.EditMessageInput{Message: "late"})
			return e
		}(),
		"drop delivered": func() error { _, e := f.svc.DropMessage(ctx, "p-1", edited.ID); return e }(),
		"edit dropped": func() error {
			_, e := f.svc.EditMessage(ctx, "p-1", dropped.ID, crews.EditMessageInput{Message: "x"})
			return e
		}(),
	} {
		if apiCode(t, err) != "CREW_MESSAGE_NOT_PENDING" {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := f.svc.EditMessage(ctx, "p-2", edited.ID, crews.EditMessageInput{Message: "x"}); apiCode(t, err) != "CREW_MESSAGE_NOT_FOUND" {
		t.Errorf("another task's thread cannot touch the message: %v", err)
	}
	// The thread is the same from the task or from any of its roles.
	if msgs, err := f.svc.ListMessages(ctx, "c"); err != nil || len(msgs) != 2 {
		t.Fatalf("thread via a role session: %v %+v", err, msgs)
	}
}

func TestFailedDeliveriesRetryThenGiveUp(t *testing.T) {
	f := newMsgFixture(t, 0)
	ctx := context.Background()
	f.out.failures = []error{errors.New("provider hiccup"), nil}
	m := send(t, f, "a", crews.SendMessageInput{To: "reviewer", Message: "retry me", IdempotencyKey: "r"})
	if m.State != "queued" || !strings.Contains(m.StateReason, "provider hiccup") {
		t.Fatalf("a failed attempt stays queued: %+v", m)
	}
	f.svc.DeliverPending(ctx)
	if msgs, _ := f.svc.ListMessages(ctx, "p-1"); msgs[0].State != "delivered" || msgs[0].Attempts != 2 {
		t.Fatalf("retry delivers: %+v", msgs[0])
	}

	f.out.failures = []error{errors.New("down"), errors.New("down"), errors.New("down"), errors.New("down"), errors.New("down")}
	g := send(t, f, "a", crews.SendMessageInput{To: "reviewer", Message: "doomed", IdempotencyKey: "g"})
	for range 6 {
		f.svc.DeliverPending(ctx)
	}
	if msgs, _ := f.svc.ListMessages(ctx, "p-1"); msgs[len(msgs)-1].ID != g.ID || msgs[len(msgs)-1].State != "failed" {
		t.Fatalf("retries are bounded: %+v", msgs)
	}

	f.out.failures = []error{fmt.Errorf("%w: gone", ports.ErrCrewMessageTargetGone)}
	h := send(t, f, "a", crews.SendMessageInput{To: "reviewer", Message: "too late", IdempotencyKey: "h"})
	if h.State != "failed" || !strings.Contains(h.StateReason, "ended") {
		t.Fatalf("an ended target fails the message: %+v", h)
	}
}

func TestMessagesUnavailableWithoutWiring(t *testing.T) {
	svc, _, _ := newRoleFixture(t, allFiles)
	if _, err := svc.SendMessage(context.Background(), "a", crews.SendMessageInput{To: "x", Message: "y"}); apiCode(t, err) != "CREW_MESSAGES_UNAVAILABLE" {
		t.Fatalf("err = %v", err)
	}
}
