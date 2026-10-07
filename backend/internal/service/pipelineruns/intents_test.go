package pipelineruns_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/pipelineruns"
)

type fakeReporter struct {
	mu      sync.Mutex
	reports []struct {
		ID    domain.SessionID
		State domain.ReportState
		Note  string
	}
}

func (r *fakeReporter) Report(_ context.Context, id domain.SessionID, state domain.ReportState, note string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reports = append(r.reports, struct {
		ID    domain.SessionID
		State domain.ReportState
		Note  string
	}{id, state, note})
	return nil
}

func (r *fakeReporter) states() []domain.ReportState {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []domain.ReportState
	for _, rep := range r.reports {
		out = append(out, rep.State)
	}
	return out
}

// orchestrated is a fixture seen from an orchestrator's side: several tasks in
// one project, a project default, and a recording reporter.
type orchestrated struct {
	*staged
	reporter *fakeReporter
	now      time.Time
	clockMu  sync.Mutex
}

func newOrchestrated(t *testing.T) *orchestrated {
	t.Helper()
	f := newFixture(t, map[string]string{
		".ao/pipelines/workflows/build-only.yaml": buildOnlyWorkflow,
		".ao/pipelines/workflows/build-test.yaml": buildTestWorkflow,
		".ao/pipelines/profiles/tester.yaml":      testerProfileYAML,
	})
	exec := &fakeExecutor{store: f.store, repo: f.repo}
	o := &orchestrated{reporter: &fakeReporter{}, now: time.Now().UTC()}
	f.svc = pipelineruns.New(pipelineruns.Deps{
		Store: f.store, Messenger: f.messenger, Executor: exec, Reporter: o.reporter,
		Clock: func() time.Time { o.clockMu.Lock(); defer o.clockMu.Unlock(); return o.now },
	})
	o.staged = &staged{fixture: f, exec: exec, gate: pipelineruns.NewStoreGate(f.store, nil)}
	return o
}

func (o *orchestrated) advance(d time.Duration) {
	o.clockMu.Lock()
	defer o.clockMu.Unlock()
	o.now = o.now.Add(d)
}

// task creates another worker task in the project, as spawn would.
func (o *orchestrated) task(mode domain.SessionMode, provision domain.SessionProvisionState, gen string) domain.SessionID {
	o.t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	rec, err := o.store.CreateSession(context.Background(), domain.SessionRecord{
		ProjectID: "proj", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode, Mode: mode,
		Activity:       domain.Activity{State: domain.ActivityIdle, LastActivityAt: now},
		Metadata:       domain.SessionMetadata{Branch: "task-branch", WorkspacePath: o.repo, ControllerGeneration: gen},
		ProvisionState: provision, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		o.t.Fatal(err)
	}
	return rec.ID
}

func (o *orchestrated) setDefault(sel *domain.PipelineSelection) {
	o.t.Helper()
	if _, _, err := o.store.SetProjectDefaultPipeline(context.Background(), "proj", sel); err != nil {
		o.t.Fatal(err)
	}
}

func (o *orchestrated) intent(id domain.SessionID, explicit *domain.PipelineSelection, by domain.PipelineRequester) *pipelineruns.IntentView {
	o.t.Helper()
	view, err := o.svc.RecordIntent(context.Background(), pipelineruns.IntentInput{SessionID: id, ProjectID: "proj", Explicit: explicit, RequestedBy: by})
	if err != nil {
		o.t.Fatal(err)
	}
	return view
}

func wf(id string) *domain.PipelineSelection {
	return &domain.PipelineSelection{Mode: domain.PipelineModeWorkflow, WorkflowID: id}
}

var normalWorker = &domain.PipelineSelection{Mode: domain.PipelineModeNormalWorker}

func TestOrdinaryTasksAreUntouchedWithoutASelectionOrDefault(t *testing.T) {
	o := newOrchestrated(t)
	id := o.task(domain.SessionModeChat, domain.SessionProvisionReady, "g1")
	if view := o.intent(id, nil, domain.PipelineRequestedByUser); view != nil {
		t.Fatalf("no default and no selection means an ordinary worker: %+v", view)
	}
	if err := o.svc.StartPendingIntents(context.Background()); err != nil {
		t.Fatal(err)
	}
	env, err := o.svc.Get(context.Background(), id)
	if err != nil || env.Run != nil || env.Intent != nil {
		t.Fatalf("env=%+v err=%v", env, err)
	}
	// A default that says "normal worker" is the same thing.
	o.setDefault(&domain.PipelineSelection{Mode: domain.PipelineModeNormalWorker})
	if view := o.intent(o.task(domain.SessionModeChat, domain.SessionProvisionReady, "g1"), nil, domain.PipelineRequestedByUser); view != nil {
		t.Fatalf("%+v", view)
	}
}

func TestProjectDefaultStartsOnNewTasksAndExplicitChoicesOverrideIt(t *testing.T) {
	o := newOrchestrated(t)
	ctx := context.Background()
	o.setDefault(wf("build-only"))

	// Default applies when nothing is chosen, and starts once the task is ready.
	a := o.task(domain.SessionModeChat, domain.SessionProvisionReady, "g1")
	v := o.intent(a, nil, domain.PipelineRequestedByOrchestrator)
	if v == nil || v.Source != "default" || v.WorkflowID != "build-only" || v.State != "pending" {
		t.Fatalf("default intent: %+v", v)
	}
	if again := o.intent(a, wf("build-test"), domain.PipelineRequestedByUser); again.WorkflowID != "build-only" {
		t.Fatalf("a repeated spawn request keeps the original selection: %+v", again)
	}
	if err := o.svc.StartPendingIntents(ctx); err != nil {
		t.Fatal(err)
	}
	env, err := o.svc.Get(ctx, a)
	if err != nil || env.Run == nil || env.Run.WorkflowID != "build-only" || env.Run.RequestedBy != "orchestrator" || env.Intent == nil || env.Intent.State != "started" || env.Intent.RunID != env.Run.ID {
		t.Fatalf("started: %+v err=%v", env, err)
	}
	// A pipeline starts exactly once however often the loop runs.
	for i := 0; i < 3; i++ {
		_ = o.svc.StartPendingIntents(ctx)
	}
	if env2, _ := o.svc.Get(ctx, a); env2.Run.ID != env.Run.ID {
		t.Fatal("the same run must not be started twice")
	}

	// An explicit workflow beats the default.
	b := o.task(domain.SessionModeChat, domain.SessionProvisionReady, "g1")
	o.setDefault(wf("build-only"))
	if v := o.intent(b, wf("build-test"), domain.PipelineRequestedByUser); v.Source != "explicit" || v.WorkflowID != "build-test" {
		t.Fatalf("%+v", v)
	}
	_ = o.svc.StartPendingIntents(ctx)
	if env, _ := o.svc.Get(ctx, b); env.Run == nil || env.Run.WorkflowID != "build-test" || env.Run.RequestedBy != "user" {
		t.Fatalf("explicit selection: %+v", env.Run)
	}

	// The explicit normal-worker override is recorded and nothing starts.
	c := o.task(domain.SessionModeChat, domain.SessionProvisionReady, "g1")
	v = o.intent(c, normalWorker, domain.PipelineRequestedByOrchestrator)
	if v == nil || !v.NormalWorker || v.State != "skipped" || !strings.Contains(v.Detail, `"build-only"`) {
		t.Fatalf("normal-worker override: %+v", v)
	}
	_ = o.svc.StartPendingIntents(ctx)
	if env, _ := o.svc.Get(ctx, c); env.Run != nil {
		t.Fatal("an explicit normal worker never starts a pipeline")
	}
}

func TestSelectionIsValidatedUpFrontAndNeverDegradesSilently(t *testing.T) {
	o := newOrchestrated(t)
	ctx := context.Background()
	writeFile(t, o.repo, ".ao/pipelines/workflows/broken.yaml", "version: 1\nid: broken\ndescription: x\nstages:\n  - {id: build, kind: build}\n  - {id: t, kind: specialist, profile: ghost}\n")
	cases := []struct {
		name string
		sel  *domain.PipelineSelection
		code string
	}{
		{"unknown workflow", wf("ghost"), "PIPELINE_WORKFLOW_NOT_FOUND"},
		{"invalid workflow", wf("broken"), "PIPELINE_WORKFLOW_INVALID"},
		{"missing id", &domain.PipelineSelection{Mode: domain.PipelineModeWorkflow}, "INVALID_PIPELINE_SELECTION"},
		{"unknown mode", &domain.PipelineSelection{Mode: "bogus"}, "INVALID_PIPELINE_SELECTION"},
		{"normal worker with an id", &domain.PipelineSelection{Mode: domain.PipelineModeNormalWorker, WorkflowID: "build-only"}, "INVALID_PIPELINE_SELECTION"},
	}
	for _, tc := range cases {
		if err := o.svc.ValidateSelection(ctx, "proj", tc.sel); err == nil || code(t, err) != tc.code {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	if err := o.svc.ValidateSelection(ctx, "proj", wf("build-test")); err != nil {
		t.Fatalf("an executable workflow is accepted: %v", err)
	}
	if err := o.svc.ValidateSelection(ctx, "proj", normalWorker); err != nil {
		t.Fatalf("the normal worker is always valid: %v", err)
	}
	if err := o.svc.ValidateSelection(ctx, "proj", nil); err != nil {
		t.Fatal(err)
	}

	// A default that stopped being usable fails visibly instead of becoming a
	// normal worker.
	o.setDefault(wf("broken"))
	id := o.task(domain.SessionModeChat, domain.SessionProvisionReady, "g1")
	o.intent(id, nil, domain.PipelineRequestedByUser)
	_ = o.svc.StartPendingIntents(ctx)
	env, _ := o.svc.Get(ctx, id)
	if env.Run != nil || env.Intent == nil || env.Intent.State != "failed" || !strings.Contains(env.Intent.Detail, "broken") {
		t.Fatalf("an unusable default must be reported, not hidden: %+v", env.Intent)
	}
}

func TestIntentWaitsForProvisioningAndSettlesWithAReason(t *testing.T) {
	o := newOrchestrated(t)
	ctx := context.Background()
	o.setDefault(wf("build-only"))

	// Still provisioning: waits, then starts once ready.
	id := o.task(domain.SessionModeChat, domain.SessionProvisionProvisioning, "g1")
	o.intent(id, nil, domain.PipelineRequestedByUser)
	_ = o.svc.StartPendingIntents(ctx)
	if env, _ := o.svc.Get(ctx, id); env.Run != nil || env.Intent.State != "pending" {
		t.Fatalf("a task that is not ready yet waits: %+v", env.Intent)
	}
	if _, err := o.store.SetSessionProvisionState(ctx, id, domain.SessionProvisionReady, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	_ = o.svc.StartPendingIntents(ctx)
	if env, _ := o.svc.Get(ctx, id); env.Run == nil || env.Intent.State != "started" {
		t.Fatalf("starts when ready: run=%v intent=%+v", env.Run, env.Intent)
	}

	// Never ready: fails visibly after the wait limit.
	stuck := o.task(domain.SessionModeChat, domain.SessionProvisionProvisioning, "g1")
	o.intent(stuck, nil, domain.PipelineRequestedByUser)
	o.advance(16 * time.Minute)
	_ = o.svc.StartPendingIntents(ctx)
	if env, _ := o.svc.Get(ctx, stuck); env.Intent.State != "failed" || !strings.Contains(env.Intent.Detail, "did not become ready") {
		t.Fatalf("%+v", env.Intent)
	}

	// Ended before it could start.
	gone := o.task(domain.SessionModeChat, domain.SessionProvisionReady, "g1")
	o.intent(gone, nil, domain.PipelineRequestedByUser)
	sess, _, _ := o.store.GetSession(ctx, gone)
	sess.IsTerminated = true
	if err := o.store.UpdateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	_ = o.svc.StartPendingIntents(ctx)
	if env, _ := o.svc.Get(ctx, gone); env.Intent.State != "skipped" || !strings.Contains(env.Intent.Detail, "ended") {
		t.Fatalf("%+v", env.Intent)
	}
}

func TestTerminalTasksAreNeverSilentlyUpgradedOrDowngraded(t *testing.T) {
	o := newOrchestrated(t)
	o.setDefault(wf("build-only"))
	tui := o.task(domain.SessionModeTUI, domain.SessionProvisionReady, "g1")
	if v := o.intent(tui, nil, domain.PipelineRequestedByUser); v.State != "skipped" || !strings.Contains(v.Detail, "Chat-only") {
		t.Fatalf("the default is skipped with a reason for a Terminal task: %+v", v)
	}
	tui2 := o.task(domain.SessionModeTUI, domain.SessionProvisionReady, "g1")
	if v := o.intent(tui2, wf("build-only"), domain.PipelineRequestedByUser); v.State != "failed" {
		t.Fatalf("an explicit choice that cannot run is a visible failure: %+v", v)
	}
}

func TestAOReportsCompletionOnlyFromValidatedPipelineCompletionAndAttentionWhenNeeded(t *testing.T) {
	o := newOrchestrated(t)
	ctx := context.Background()

	// A user-requested pause needs no announcement; a failing stage does.
	run, err := o.svc.Start(ctx, pipelineruns.StartInput{SessionID: o.sessionID, WorkflowID: "build-only", RequestedBy: "user"})
	if err != nil {
		t.Fatal(err)
	}
	o.mustControl(run, "pause", "orchestrator")
	if got := o.reporter.states(); len(got) != 0 {
		t.Fatalf("a requested pause is not news: %v", got)
	}
	o.mustControl(run, "resume", "orchestrator")
	// The worker claims it failed the stage: somebody has to decide.
	att := o.get().Attempts
	last := att[len(att)-1]
	if last.State != "handoff" && last.State != "active" {
		t.Fatalf("setup: %+v", last)
	}
	if err := o.svc.DriveHandoffs(ctx); err != nil {
		t.Fatal(err)
	}
	active := o.get().Attempts[len(att)-1]
	if _, err := o.svc.Submit(ctx, pipelineruns.SubmitInput{
		SessionID: o.sessionID, RunID: run.ID, AttemptID: active.ID, ControllerGeneration: active.ControllerGeneration,
		IdempotencyKey: "f", Outcome: "failed", ExpectedInputCommit: active.InputCommit, Summary: "cannot do it",
	}); err != nil {
		t.Fatal(err)
	}
	states := o.reporter.states()
	if len(states) != 1 || states[0] != domain.ReportNeedsInput {
		t.Fatalf("an actionable pause is reported once: %v", states)
	}
	if note := o.reporter.reports[0].Note; !strings.Contains(note, "stage_failed") || !strings.Contains(note, "ao pipeline status --session") {
		t.Fatalf("the report tells the orchestrator where to look: %q", note)
	}
	// A worker saying done does nothing here; only validated completion reports done.
	o.mustControl(run, "cancel", "user")
	if got := o.reporter.states(); len(got) != 1 {
		t.Fatalf("cancelling is not completion: %v", got)
	}

	run2, err := o.svc.Start(ctx, pipelineruns.StartInput{SessionID: o.sessionID, WorkflowID: "build-only", RequestedBy: "user"})
	if err != nil {
		t.Fatal(err)
	}
	head := o.commitWork("done.txt")
	if _, err := o.svc.Submit(ctx, pipelineruns.SubmitInput{
		SessionID: o.sessionID, RunID: run2.ID, AttemptID: run2.Attempts[0].ID, ControllerGeneration: run2.Attempts[0].ControllerGeneration,
		IdempotencyKey: "ok", Outcome: "succeeded", ExpectedInputCommit: run2.Attempts[0].InputCommit, OutputCommit: head, Summary: "done",
	}); err != nil {
		t.Fatal(err)
	}
	states = o.reporter.states()
	if len(states) != 2 || states[1] != domain.ReportDone {
		t.Fatalf("validated completion is reported as done: %v", states)
	}
	if note := o.reporter.reports[1].Note; !strings.Contains(note, "does not merge") {
		t.Fatalf("completion never implies a merge: %q", note)
	}
}

// The orchestrator-driven demonstration: everything an orchestrator does with
// pipelines, in the order it would, with what the daemon refuses along the way.
func TestOrchestratorDrivenDemonstration(t *testing.T) {
	o := newOrchestrated(t)
	ctx := context.Background()

	// 1. Discover: the catalog lists executable workflows; the project default is
	// a reference the orchestrator can read.
	if err := o.svc.ValidateSelection(ctx, "proj", wf("build-test")); err != nil {
		t.Fatalf("an executable workflow can be selected: %v", err)
	}

	// 2. Select at task creation: explicit workflow, as an orchestrator.
	id := o.task(domain.SessionModeChat, domain.SessionProvisionReady, "g1")
	v := o.intent(id, wf("build-test"), domain.PipelineRequestedByOrchestrator)
	if v.State != "pending" || v.Source != "explicit" {
		t.Fatalf("%+v", v)
	}
	if err := o.svc.StartPendingIntents(ctx); err != nil {
		t.Fatal(err)
	}

	// 3. Supervise: readable progress without reading anything private.
	env, _ := o.svc.Get(ctx, id)
	run := *env.Run
	if run.RequestedBy != "orchestrator" || run.CurrentStageID != "build" || run.RepairsRemaining != 3 || run.Stages[1].SettingsSource != pipelineruns.SourceProfile {
		t.Fatalf("progress: %s %+v", brief(run), run.Stages)
	}

	// 4. What the daemon refuses an orchestrator, however it asks.
	if _, err := o.svc.Start(ctx, pipelineruns.StartInput{SessionID: o.task(domain.SessionModeChat, domain.SessionProvisionReady, "g1"), WorkflowID: "build-test", RequestedBy: "orchestrator", Overrides: map[string]pipelineruns.StageOverride{"test": {Model: "cheap"}}}); err == nil || code(t, err) != "PIPELINE_OVERRIDE_USER_ONLY" {
		t.Fatalf("harness/model overrides are user-only: %v", err)
	}
	if _, err := o.svc.Control(ctx, pipelineruns.ControlInput{SessionID: id, RunID: run.ID, Action: "authorize_repairs", RequestedBy: "orchestrator", AdditionalRepairs: 5, RequestKey: "x"}); err == nil || code(t, err) != "PIPELINE_HUMAN_AUTHORIZATION_REQUIRED" {
		t.Fatalf("an orchestrator cannot extend the budget: %v", err)
	}
	// The worker reporting done (ao report) does not advance the stage.
	if env, _ := o.svc.Get(ctx, id); env.Run.CurrentStageID != "build" || env.Run.State != "running" {
		t.Fatal("worker reports are not stage authority")
	}

	// 5. Control: pause and resume an operational pause.
	if paused := mustCtl(t, o.svc, id, run.ID, "pause", "orchestrator"); paused.Run.State != "paused" {
		t.Fatalf("%+v", paused.Run)
	}
	if resumed := mustCtl(t, o.svc, id, run.ID, "resume", "orchestrator"); resumed.Run.State != "running" {
		t.Fatalf("%+v", resumed.Run)
	}
	// A stale instruction (an older revision) is refused rather than applied.
	if _, err := o.svc.Control(ctx, pipelineruns.ControlInput{SessionID: id, RunID: run.ID, Action: "pause", RequestedBy: "orchestrator", ExpectedRevision: run.Revision}); err == nil || code(t, err) != "PIPELINE_STALE_CONTROL" {
		t.Fatalf("stale control: %v", err)
	}
}

func mustCtl(t *testing.T, svc *pipelineruns.Service, session domain.SessionID, runID, action, by string) pipelineruns.ControlResult {
	t.Helper()
	res, err := svc.Control(context.Background(), pipelineruns.ControlInput{SessionID: session, RunID: runID, Action: action, RequestedBy: by})
	if err != nil {
		t.Fatalf("%s: %v", action, err)
	}
	return res
}
