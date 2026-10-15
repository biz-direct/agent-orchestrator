package lifecycle

import (
	"context"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// batonStore routes the worker's automatic work the way the SQLite store does.
type batonStore struct {
	*fakeStore
	target  domain.SessionID
	deliver bool
}

func (b *batonStore) ResolveNudgeTarget(context.Context, domain.SessionID) (domain.SessionID, bool, error) {
	return b.target, b.deliver, nil
}

func batonManager(target domain.SessionID, deliver bool) (*Manager, *fakeMessenger) {
	st := &batonStore{fakeStore: newFakeStore(), target: target, deliver: deliver}
	msg := &fakeMessenger{}
	st.sessions["mer-1"] = working("mer-1")
	st.sessions["mer-1-role"] = working("mer-1-role")
	return New(st, msg), msg
}

func batonObservations() []ports.PRObservation {
	return []ports.PRObservation{
		{Fetched: true, URL: "pr-ci", CI: domain.CIFailing, Checks: []ports.PRCheckObservation{{Name: "build", CommitHash: "c1", Status: domain.PRCheckFailed}}},
		{Fetched: true, URL: "pr-conflict", Mergeability: domain.MergeConflicting},
	}
}

// CI-failure and merge-conflict nudges go to the baton holder, and only to it.
func TestNudgesGoOnlyToTheBatonHolder(t *testing.T) {
	m, msg := batonManager("mer-1-role", true)
	for _, o := range batonObservations() {
		if err := m.ApplyPRObservation(ctx, "mer-1", o); err != nil {
			t.Fatal(err)
		}
	}
	if len(msg.msgs) != 2 {
		t.Fatalf("want 2 nudges, got %v", msg.msgs)
	}
	for _, id := range msg.ids {
		if id != "mer-1-role" {
			t.Fatalf("a nudge reached %s instead of the holding role: %v", id, msg.ids)
		}
	}
}

// While the human holds the baton nobody is nudged, and the nudge is not lost:
// it fires once the baton goes back to an agent.
func TestNudgesWaitWhileTheHumanHoldsTheBaton(t *testing.T) {
	m, msg := batonManager("", false)
	st := m.store.(*batonStore)
	for _, o := range batonObservations() {
		if err := m.ApplyPRObservation(ctx, "mer-1", o); err != nil {
			t.Fatal(err)
		}
	}
	if len(msg.msgs) != 0 {
		t.Fatalf("the human holds the baton, nobody is nudged: %v", msg.ids)
	}
	st.target, st.deliver = "mer-1", true
	for _, o := range batonObservations() {
		if err := m.ApplyPRObservation(ctx, "mer-1", o); err != nil {
			t.Fatal(err)
		}
	}
	if len(msg.msgs) != 2 || msg.ids[0] != "mer-1" {
		t.Fatalf("held-back nudges fire once an agent holds the baton: %v %v", msg.ids, msg.msgs)
	}
}

// A parked owner does not block automatic work an active holding role can do;
// an exited or waiting holder still does.
func TestNudgeEligibilityFollowsTheBatonHolderNotTheOwner(t *testing.T) {
	m, msg := batonManager("mer-1-role", true)
	st := m.store.(*batonStore)
	parked := st.sessions["mer-1"]
	parked.Activity.State = domain.ActivityWaitingInput
	st.sessions["mer-1"] = parked
	if err := m.ApplyPRObservation(ctx, "mer-1", batonObservations()[0]); err != nil {
		t.Fatal(err)
	}
	if len(msg.msgs) != 1 || msg.ids[0] != "mer-1-role" {
		t.Fatalf("an active holder gets the CI nudge although the owner is parked: %v", msg.ids)
	}

	m, msg = batonManager("mer-1-role", true)
	st = m.store.(*batonStore)
	role := st.sessions["mer-1-role"]
	role.Activity.State = domain.ActivityWaitingInput
	st.sessions["mer-1-role"] = role
	if err := m.ApplyPRObservation(ctx, "mer-1", batonObservations()[0]); err != nil {
		t.Fatal(err)
	}
	if len(msg.msgs) != 0 {
		t.Fatalf("a holder waiting on input keeps its permission-dialog protection: %v", msg.ids)
	}
}

// Ending the task vetoes automatic work even while its holding role is live.
func TestTerminatedTaskDoesNotNudgeItsLiveHolder(t *testing.T) {
	m, msg := batonManager("mer-1-role", true)
	if err := m.MarkTerminated(ctx, "mer-1"); err != nil {
		t.Fatal(err)
	}
	for _, o := range batonObservations() {
		if err := m.ApplyPRObservation(ctx, "mer-1", o); err != nil {
			t.Fatal(err)
		}
	}
	if len(msg.msgs) != 0 {
		t.Fatalf("a terminated task must not nudge its holder: %v", msg.ids)
	}
}
