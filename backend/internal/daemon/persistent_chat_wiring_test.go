package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestPersistentChatHostKeepSetUsesDurableOwnership(t *testing.T) {
	before := time.Now().UTC()
	records := []domain.SessionRecord{
		{ID: "live-chat", Mode: domain.SessionModeChat, Harness: domain.HarnessCodex},
		{ID: "terminated-chat", Mode: domain.SessionModeChat, Harness: domain.HarnessCodex, IsTerminated: true},
		{ID: "tui", Mode: domain.SessionModeTUI, Harness: domain.HarnessCodex},
		{ID: "other-provider", Mode: domain.SessionModeChat, Harness: domain.HarnessClaudeCode},
		{ID: "hibernated-chat", Mode: domain.SessionModeChat, Harness: domain.HarnessCodex, HibernatedAt: &before},
	}
	keep := persistentChatHostKeepSet(records, []domain.Review{{ID: "recoverable-review"}})
	if len(keep) != 3 {
		t.Fatalf("keep = %v, want both live Chat providers and the reviewer", keep)
	}
	if _, ok := keep["live-chat"]; !ok {
		t.Fatalf("keep = %v, missing live-chat", keep)
	}
	if _, ok := keep["other-provider"]; !ok {
		t.Fatalf("keep = %v, missing other-provider", keep)
	}
	if _, ok := keep["review-recoverable-review"]; !ok {
		t.Fatalf("keep = %v, missing recoverable reviewer host", keep)
	}
}

func TestSideHostsSurviveOnlyTheirDesktopLaunch(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"btw-current-one", "btw-current-two", "btw-previous-one", "main"} {
		if err := os.MkdirAll(filepath.Join(dir, "chat-hosts", id), 0700); err != nil {
			t.Fatal(err)
		}
	}
	keep := map[string]struct{}{}
	keepLaunchSideHosts(dir, "current", keep)
	if len(keep) != 2 {
		t.Fatalf("kept side hosts: %v", keep)
	}
	if _, ok := keep["btw-previous-one"]; ok {
		t.Fatal("kept a previous desktop launch")
	}
}

type roleListingStore struct {
	ids []domain.SessionID
	err error
}

func (s roleListingStore) ListLiveCrewRoleSessionIDs(context.Context) ([]domain.SessionID, error) {
	return s.ids, s.err
}

// Role sessions are hidden from ListAllSessions, so their live hosts must be
// added to the keep set explicitly or a restart would shut them down mid-turn.
func TestKeepCrewRoleHostsPreservesLiveRoleHosts(t *testing.T) {
	keep := map[string]struct{}{"ordinary": {}}
	if err := keepCrewRoleHosts(context.Background(), roleListingStore{ids: []domain.SessionID{"p-att-1"}}, keep); err != nil {
		t.Fatal(err)
	}
	if _, ok := keep["p-att-1"]; !ok || len(keep) != 2 {
		t.Fatalf("keep = %v", keep)
	}
	if err := keepCrewRoleHosts(context.Background(), struct{}{}, keep); err != nil {
		t.Fatalf("stores without role listing keep nothing extra: %v", err)
	}
	if err := keepCrewRoleHosts(context.Background(), roleListingStore{err: errors.New("db")}, keep); err == nil {
		t.Fatal("an unreadable role set is not evidence that hosts are orphaned")
	}
}
