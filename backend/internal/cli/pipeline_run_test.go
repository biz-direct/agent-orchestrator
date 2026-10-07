package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func runServer(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) {
	t.Helper()
	cfg := setConfigEnv(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/internal/telemetry/cli-invoked" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		handle(w, r)
	}))
	t.Cleanup(server.Close)
	writeRunFileFor(t, cfg, server)
}

const runningRunJSON = `{"run":{"id":"prun_1","workflowId":"wf","state":"running","currentStageId":"build","requestedBy":"user","expectedBranch":"task","repairBudget":3,"repairsUsed":0,"repairsRemaining":3,
 "stages":[{"id":"build","kind":"build","state":"active","harness":"claude-code","model":"opus","settingsSource":"worker"}],
 "attempts":[{"id":"pstg_1","stageId":"build","attemptNo":1,"state":"active","controllerGeneration":"gen-1","inputCommit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}}`

func TestPipelineStartIdentifiesCallerAndRefusesAgentOverrides(t *testing.T) {
	var got pipelineStartRequestDTO
	runServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/sessions/w-1/pipeline" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, runningRunJSON)
	})
	deps := Deps{ProcessAlive: func(int) bool { return true }}

	t.Setenv("AO_SESSION_ID", "")
	out, stderr, err := executeCLI(t, deps, "pipeline", "start", "wf", "--session", "w-1")
	if err != nil || got.RequestedBy != "user" || got.WorkflowID != "wf" || !strings.Contains(out, "prun_1") {
		t.Fatalf("user start: err=%v stderr=%s body=%+v out=%s", err, stderr, got, out)
	}

	t.Setenv("AO_SESSION_ID", "orchestrator-1")
	if _, _, err := executeCLI(t, deps, "pipeline", "start", "wf", "--session", "w-1"); err != nil || got.RequestedBy != "orchestrator" {
		t.Fatalf("a session caller is an orchestrator: err=%v body=%+v", err, got)
	}
	if _, _, err := executeCLI(t, deps, "pipeline", "start", "wf", "--session", "w-1", "--override-stage", "test", "--model", "x"); err == nil || ExitCode(err) != 2 {
		t.Fatalf("agents cannot override stage settings: %v", err)
	}

	t.Setenv("AO_SESSION_ID", "")
	if _, _, err := executeCLI(t, deps, "pipeline", "start", "wf", "--session", "w-1", "--override-stage", "test", "--model", "x"); err != nil || got.Overrides["test"].Model != "x" || got.RequestedBy != "user" {
		t.Fatalf("user override: err=%v body=%+v", err, got)
	}
	if _, _, err := executeCLI(t, deps, "pipeline", "start", "wf", "--session", "w-1", "--model", "x"); err == nil || ExitCode(err) != 2 {
		t.Fatalf("--model without --override-stage is a usage error: %v", err)
	}
}

func TestPipelineRequestsCarryTheSessionCapabilityForServerSideBinding(t *testing.T) {
	var headers http.Header
	runServer(t, func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, runningRunJSON)
	})
	deps := Deps{ProcessAlive: func(int) bool { return true }}

	t.Setenv("AO_SESSION_ID", "orchestrator-1")
	t.Setenv("AO_BROWSER_CAPABILITY", "cap-secret")
	if _, _, err := executeCLI(t, deps, "pipeline", "start", "wf", "--session", "w-1"); err != nil {
		t.Fatal(err)
	}
	if headers.Get("X-AO-Caller-Session") != "orchestrator-1" || headers.Get("X-AO-Browser-Capability") != "cap-secret" {
		t.Fatalf("the daemon needs the session's identity to bind the requester: %v", headers)
	}

	t.Setenv("AO_SESSION_ID", "")
	t.Setenv("AO_BROWSER_CAPABILITY", "")
	if _, _, err := executeCLI(t, deps, "pipeline", "start", "wf", "--session", "w-1"); err != nil {
		t.Fatal(err)
	}
	if headers.Get("X-AO-Caller-Session") != "" || headers.Get("X-AO-Browser-Capability") != "" {
		t.Fatalf("a person at a shell sends no session identity: %v", headers)
	}
}

func TestPipelineStatusWithoutRunSaysOrdinaryWorker(t *testing.T) {
	runServer(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `{"run":null}`) })
	t.Setenv("AO_SESSION_ID", "w-1")
	out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "pipeline", "status")
	if err != nil || !strings.Contains(out, "ordinary worker") {
		t.Fatalf("err=%v out=%s", err, out)
	}
}

func TestPipelineSubmitReportsVerifiedHeadWithStableIdempotencyKey(t *testing.T) {
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "task"}, {"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "x"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	head, _ := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	wantHead := strings.TrimSpace(string(head))
	var bodies []pipelineSubmitRequestDTO
	runServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, runningRunJSON)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sessions/w-1/pipeline/results":
			var b pipelineSubmitRequestDTO
			_ = json.NewDecoder(r.Body).Decode(&b)
			bodies = append(bodies, b)
			_, _ = io.WriteString(w, `{"run":{"id":"prun_1","workflowId":"wf","state":"completed","stages":[{"id":"build","kind":"build","state":"accepted","settingsSource":"worker"}],"attempts":[],"checkpoint":{"stageId":"build","inputCommit":"aaaa","outputCommit":"`+wantHead+`","noChange":false}},"attempt":{"id":"pstg_1","stageId":"build","attemptNo":1,"state":"accepted"},"accepted":true,"replayed":false}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	t.Setenv("AO_SESSION_ID", "w-1")
	old, _ := os.Getwd()
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	deps := Deps{ProcessAlive: func(int) bool { return true }}
	out, stderr, err := executeCLI(t, deps, "pipeline", "submit", "--outcome", "succeeded", "--summary", "done")
	if err != nil {
		t.Fatalf("submit: %v stderr=%s", err, stderr)
	}
	if _, _, err := executeCLI(t, deps, "pipeline", "submit", "--outcome", "succeeded", "--summary", "done"); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatalf("bodies: %+v", bodies)
	}
	b := bodies[0]
	if b.OutputCommit != wantHead || b.RunID != "prun_1" || b.AttemptID != "pstg_1" || b.ControllerGeneration != "gen-1" || b.ExpectedInputCommit != strings.Repeat("a", 40) || b.Outcome != "succeeded" {
		t.Fatalf("body: %+v", b)
	}
	if b.IdempotencyKey == "" || b.IdempotencyKey != bodies[1].IdempotencyKey {
		t.Fatalf("a retry must reuse the same idempotency key: %q vs %q", b.IdempotencyKey, bodies[1].IdempotencyKey)
	}
	if !strings.Contains(out, "accepted") || !strings.Contains(out, "checkpoint") {
		t.Fatalf("output: %s", out)
	}
}

func TestPipelineSubmitValidatesBeforeCallingDaemon(t *testing.T) {
	setConfigEnv(t)
	t.Setenv("AO_SESSION_ID", "")
	for _, args := range [][]string{
		{"pipeline", "submit"},
		{"pipeline", "submit", "--outcome", "done"},
		{"pipeline", "submit", "--outcome", "failed"},
	} {
		if _, _, err := executeCLI(t, Deps{}, args...); err == nil || ExitCode(err) != 2 {
			t.Fatalf("args=%v err=%v", args, err)
		}
	}
}

func TestPipelineSubmitSendsASpecialistReportVerbatim(t *testing.T) {
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "task"}, {"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "x"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	report := `{"findings":[{"criterion":"c","status":"met"}],"commands":[],"remainingIssues":[],"defects":[]}`
	reportPath := repo + "/../report-" + t.Name() + ".json"
	if err := os.WriteFile(reportPath, []byte(report), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(reportPath) })
	var got pipelineSubmitRequestDTO
	runServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, runningRunJSON)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = io.WriteString(w, `{"run":{"id":"prun_1","workflowId":"wf","state":"completed","stages":[],"attempts":[]},"attempt":{"id":"pstg_1","stageId":"test","attemptNo":1,"state":"accepted"},"accepted":true,"replayed":false}`)
	})
	t.Setenv("AO_SESSION_ID", "w-1-att-2")
	old, _ := os.Getwd()
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	deps := Deps{ProcessAlive: func(int) bool { return true }}
	if _, stderr, err := executeCLI(t, deps, "pipeline", "submit", "--outcome", "succeeded", "--report-file", reportPath); err != nil {
		t.Fatalf("submit: %v %s", err, stderr)
	}
	if string(got.Report) != report || got.OutputCommit == "" {
		t.Fatalf("the report must reach the daemon untouched, with the verified HEAD: %+v", got)
	}
	if _, _, err := executeCLI(t, deps, "pipeline", "submit", "--outcome", "succeeded", "--report-file", repo+"/missing.json"); err == nil || ExitCode(err) != 2 {
		t.Fatalf("an unreadable report is a usage error: %v", err)
	}
	if err := os.WriteFile(reportPath, []byte("[1]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := executeCLI(t, deps, "pipeline", "submit", "--outcome", "succeeded", "--report-file", reportPath); err == nil || ExitCode(err) != 2 {
		t.Fatalf("a non-object report is a usage error: %v", err)
	}
}

func TestPipelineStatusShowsCurrentHeadReviewReadiness(t *testing.T) {
	runServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"run":{"id":"prun_1","workflowId":"wf","state":"running","currentStageId":"review","requestedBy":"user","expectedBranch":"task","repairBudget":3,"repairsUsed":0,"repairsRemaining":3,
 "stages":[{"id":"build","kind":"build","state":"accepted","settingsSource":"worker"},{"id":"review","kind":"review","state":"active","settingsSource":"reviewer"}],
 "attempts":[],"repairs":[],
 "reviewGate":{"state":"waiting","code":"awaiting_manual_review","message":"Auto review is off. Trigger the review for this revision from the task's review controls","manual":true,"autoReview":false,"checkpoint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","headSha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","prUrl":"https://github.com/o/r/pull/1","ci":"no_checks","ciDetail":"no CI requirement"},
 "reviews":[{"stageId":"review","revision":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","headSha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","reviewRunId":"rr0","outcome":"changes_requested","current":false}]}}`)
	})
	t.Setenv("AO_SESSION_ID", "w-1")
	out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "pipeline", "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"review: waiting [awaiting_manual_review] (auto review off) at aaaaaaaaaa", "Trigger the review", "pull request https://github.com/o/r/pull/1 head aaaaaaaaaa", "required CI: no_checks", "review of bbbbbbbbbb (review): changes_requested head bbbbbbbbbb run rr0 [superseded]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status output missing %q:\n%s", want, out)
		}
	}
}

func TestPipelineControlCommandsSendConditionalIdempotentRequests(t *testing.T) {
	var bodies []pipelineControlRequestDTO
	runServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"run":{"id":"prun_1","workflowId":"wf","state":"paused","pauseReason":"repair_budget_exhausted","revision":7,"repairBudget":3,"repairsUsed":3,"repairsRemaining":0,"stages":[],"attempts":[],"repairs":[],"reviews":[],"repairGrants":[],"control":{"canPause":false,"canResume":false,"canCancel":true,"resumeNeedsUser":true,"needsRepairAuthorization":true}}}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sessions/w-1/pipeline/control":
			var b pipelineControlRequestDTO
			_ = json.NewDecoder(r.Body).Decode(&b)
			bodies = append(bodies, b)
			_, _ = io.WriteString(w, `{"changed":false,"stop":{"requested":true,"confirmed":false,"detail":"waiting on a permission request"},"run":{"id":"prun_1","workflowId":"wf","state":"paused","revision":8,"stages":[],"attempts":[],"repairs":[],"reviews":[],"repairGrants":[],"control":{}}}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	deps := Deps{ProcessAlive: func(int) bool { return true }}

	t.Setenv("AO_SESSION_ID", "")
	out, _, err := executeCLI(t, deps, "pipeline", "pause", "--session", "w-1", "--reason", "lunch")
	if err != nil || len(bodies) != 1 {
		t.Fatalf("pause: err=%v bodies=%d", err, len(bodies))
	}
	if b := bodies[0]; b.Action != "pause" || b.RunID != "prun_1" || b.RequestedBy != "user" || b.ExpectedRevision != 7 || b.Reason != "lunch" {
		t.Fatalf("request: %+v", b)
	}
	for _, want := range []string{"already paused (no change)", "not confirmed stopped", "waiting on a permission request"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}

	if _, _, err := executeCLI(t, deps, "pipeline", "authorize-repairs", "--session", "w-1", "--count", "2", "--request-key", "k1"); err != nil {
		t.Fatal(err)
	}
	if b := bodies[1]; b.Action != "authorize_repairs" || b.AdditionalRepairs != 2 || b.RequestKey != "k1" || b.RequestedBy != "user" {
		t.Fatalf("authorize request: %+v", b)
	}
	// The default request key is stable, so an accidental repeat is not a second grant.
	for i := 0; i < 2; i++ {
		if _, _, err := executeCLI(t, deps, "pipeline", "authorize-repairs", "--session", "w-1"); err != nil {
			t.Fatal(err)
		}
	}
	if bodies[2].RequestKey == "" || bodies[2].RequestKey != bodies[3].RequestKey {
		t.Fatalf("default keys: %q %q", bodies[2].RequestKey, bodies[3].RequestKey)
	}

	// Inside an AO session the caller is an orchestrator: it can pause, but it
	// cannot authorize repairs.
	t.Setenv("AO_SESSION_ID", "orchestrator-1")
	if _, _, err := executeCLI(t, deps, "pipeline", "resume", "--session", "w-1"); err != nil {
		t.Fatal(err)
	}
	if b := bodies[len(bodies)-1]; b.Action != "resume" || b.RequestedBy != "orchestrator" {
		t.Fatalf("an agent identifies itself as an orchestrator: %+v", b)
	}
	n := len(bodies)
	if _, _, err := executeCLI(t, deps, "pipeline", "authorize-repairs", "--session", "w-1"); err == nil || ExitCode(err) != 2 || len(bodies) != n {
		t.Fatalf("authorizing repairs is user-only and must not reach the daemon: %v", err)
	}
}

func TestPipelineStatusExplainsASelectionThatHasNotStartedOrCouldNot(t *testing.T) {
	runServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"run":null,"intent":{"workflowId":"build-test-review","normalWorker":false,"source":"default","state":"failed","detail":"Workflow \"build-test-review\" could not start: Workflow is invalid"}}`)
	})
	t.Setenv("AO_SESSION_ID", "w-1")
	out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "pipeline", "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"no pipeline run yet: workflow build-test-review was selected at creation (default): failed", "could not start"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
}
