package ports

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

// PipelineGuard lets ordinary lifecycle shortcuts (automatic review, merge-driven
// completion/cleanup) stay out of the way while a pipeline run still owns a
// worker's lifecycle. A nil guard means "no pipelines": behavior is unchanged
// for every ordinary worker.
type PipelineGuard interface {
	// SuppressesLifecycleShortcuts reports whether an unfinished pipeline run
	// owns the session. It fails closed: an undeterminable answer suppresses.
	SuppressesLifecycleShortcuts(ctx context.Context, id domain.SessionID) bool
}
