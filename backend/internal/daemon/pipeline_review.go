package daemon

import (
	"context"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/pipelineruns"
	reportsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/report"
	reviewsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/review"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite"
)

// pipelineReviewGateway adapts AO's existing review subsystem and the SCM
// observer's stored pull request facts for a pipeline Review stage. It adds no
// reviewer of its own: facts are read from the same tables the Review panel
// reads, and a pass is started through the same service auto-review uses.
type pipelineReviewGateway struct {
	store   *sqlite.Store
	reviews reviewsvc.Manager
}

var _ pipelineruns.ReviewGateway = pipelineReviewGateway{}

// Facts implements pipelineruns.ReviewGateway.
func (g pipelineReviewGateway) Facts(ctx context.Context, id domain.SessionID) (pipelineruns.ReviewFacts, error) {
	prs, err := g.store.ListPRsBySession(ctx, id)
	if err != nil {
		return pipelineruns.ReviewFacts{}, err
	}
	facts := pipelineruns.ReviewFacts{PRs: prs, Checks: map[string][]domain.PullRequestCheck{}}
	for _, pr := range prs {
		checks, cerr := g.store.ListChecks(ctx, pr.URL)
		if cerr != nil {
			return pipelineruns.ReviewFacts{}, cerr
		}
		facts.Checks[pr.URL] = checks
	}
	if facts.Runs, err = g.store.ListReviewRunsBySession(ctx, id); err != nil {
		return pipelineruns.ReviewFacts{}, err
	}
	return facts, nil
}

// TriggerAuto implements pipelineruns.ReviewGateway.
func (g pipelineReviewGateway) TriggerAuto(ctx context.Context, id domain.SessionID) (string, error) {
	res, err := g.reviews.TriggerAuto(ctx, id, "")
	if err != nil {
		return "", err
	}
	return res.SkipReason, nil
}

// pipelineReporter delivers a pipeline run's AO-authored reports through the
// ordinary worker-report outbox, so orchestrators see them exactly where they
// see their workers' own reports.
type pipelineReporter struct {
	reports *reportsvc.Service
}

var _ pipelineruns.Reporter = pipelineReporter{}

// Report implements pipelineruns.Reporter.
func (r pipelineReporter) Report(ctx context.Context, id domain.SessionID, state domain.ReportState, note string) error {
	_, err := r.reports.Create(ctx, reportsvc.CreateInput{SessionID: id, State: state, Note: note})
	return err
}
