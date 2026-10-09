package daemon

import (
	"context"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	settingssvc "github.com/aoagents/agent-orchestrator/backend/internal/service/settings"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// settingsStore adapts the SQLite store to the settings service's Store.
//
// The two define their own snapshot types so neither depends on the other's; this
// is the one place that knows both, keeping the translation in the wiring.
type settingsStore struct{ store *sqlite.Store }

var _ settingssvc.Store = settingsStore{}

func (s settingsStore) GetAppSettings(ctx context.Context) (settingssvc.Snapshot, error) {
	row, err := s.store.GetAppSettings(ctx)
	if err != nil {
		return settingssvc.Snapshot{}, err
	}
	return settingssvc.Snapshot{
		DefaultSessionMode:      row.DefaultSessionMode,
		CloudOffering:           row.CloudOffering,
		GlobalOrchestratorRules: row.GlobalOrchestratorRules,
		UpdatedAt:               row.UpdatedAt,
	}, nil
}

func (s settingsStore) SetDefaultSessionMode(
	ctx context.Context,
	mode domain.SessionMode,
	now time.Time,
) error {
	return s.store.SetDefaultSessionMode(ctx, mode, now)
}

func (s settingsStore) SetCloudOffering(ctx context.Context, enabled bool, now time.Time) error {
	return s.store.SetCloudOffering(ctx, enabled, now)
}

func (s settingsStore) SetGlobalOrchestratorRules(ctx context.Context, rules string, now time.Time) error {
	return s.store.SetGlobalOrchestratorRules(ctx, rules, now)
}
