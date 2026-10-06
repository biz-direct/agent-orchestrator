package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func pipelineServer(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) {
	t.Helper()
	cfg := setConfigEnv(t)
	t.Setenv("AO_PROJECT_ID", "")
	t.Setenv("AO_SESSION_ID", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/internal/telemetry/cli-invoked":
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/demo":
			_, _ = io.WriteString(w, `{"project":{"id":"demo","kind":"single_repo"}}`)
		default:
			handle(w, r)
		}
	}))
	t.Cleanup(server.Close)
	writeRunFileFor(t, cfg, server)
}

const catalogJSON = `{
 "projectId":"demo",
 "profiles":[{"id":"tester","file":".ao/pipelines/profiles/tester.yaml","valid":true,"profile":{"id":"tester","description":"Writes tests","instructionsSource":"inline"},"diagnostics":[]}],
 "workflows":[
  {"id":"bt","file":".ao/pipelines/workflows/bt.yaml","valid":true,"workflow":{"id":"bt","description":"Build then test","repairBudget":3,"stages":[]},"diagnostics":[],"executable":false,"unavailableReason":"Workflow execution is not available in this build."},
  {"id":"bad","file":".ao/pipelines/workflows/bad.yaml","valid":false,"diagnostics":[{"file":".ao/pipelines/workflows/bad.yaml","field":"stages[1].profile","message":"references unknown profile \"ghost\""}],"executable":false}
 ],
 "diagnostics":[],
 "default":{"selection":{"mode":"workflow","workflowId":"bt"},"state":"workflow_unavailable","executable":false,"message":"Workflow execution is not available in this build."}
}`

func TestPipelineListShowsStatusAndUnavailableDefault(t *testing.T) {
	pipelineServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/demo/pipelines" {
			_, _ = io.WriteString(w, catalogJSON)
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	out, stderr, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "pipeline", "ls", "--project", "demo")
	if err != nil {
		t.Fatalf("ls: %v stderr=%s", err, stderr)
	}
	for _, want := range []string{"profile", "tester", "workflow", "bt", "unavailable", "bad", "invalid", "workflow bt (workflow_unavailable)", "ao pipeline validate"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestPipelineValidateFailsWithActionableDiagnostics(t *testing.T) {
	pipelineServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, catalogJSON)
	})
	out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "pipeline", "validate", "--project", "demo")
	if err == nil || ExitCode(err) != 1 {
		t.Fatalf("invalid definitions must exit 1: err=%v", err)
	}
	if !strings.Contains(out, "bad.yaml [stages[1].profile]: references unknown profile") {
		t.Fatalf("diagnostic missing file/field/message:\n%s", out)
	}
}

func TestPipelineValidateSucceedsOnCleanCatalog(t *testing.T) {
	pipelineServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"projectId":"demo","profiles":[],"workflows":[],"diagnostics":[],"default":{"selection":null,"state":"unset","executable":false,"message":"none"}}`)
	})
	out, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "pipeline", "validate", "--project", "demo")
	if err != nil || !strings.Contains(out, "ok:") {
		t.Fatalf("err=%v out=%s", err, out)
	}
}

func TestPipelineDefaultSetAndClear(t *testing.T) {
	var bodies []string
	pipelineServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v1/projects/demo/pipelines/default" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		var in pipelineSetDefaultDTO
		_ = json.Unmarshal(raw, &in)
		resp := pipelineDefaultDTO{Selection: in.Selection, State: "unset", Message: "ok"}
		if in.Selection != nil {
			resp.State = "workflow_unavailable"
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	deps := Deps{ProcessAlive: func(int) bool { return true }}
	if out, stderr, err := executeCLI(t, deps, "pipeline", "default", "set", "bt", "--project", "demo"); err != nil || !strings.Contains(out, "workflow bt") {
		t.Fatalf("set: err=%v out=%s stderr=%s", err, out, stderr)
	}
	if out, _, err := executeCLI(t, deps, "pipeline", "default", "set", "--normal-worker", "--project", "demo"); err != nil || !strings.Contains(out, "normal worker") {
		t.Fatalf("normal worker: err=%v out=%s", err, out)
	}
	if out, _, err := executeCLI(t, deps, "pipeline", "default", "clear", "--project", "demo"); err != nil || !strings.Contains(out, "(none)") {
		t.Fatalf("clear: err=%v out=%s", err, out)
	}
	want := []string{
		`{"selection":{"mode":"workflow","workflowId":"bt"}}`,
		`{"selection":{"mode":"normal_worker"}}`,
		`{"selection":null}`,
	}
	for i, w := range want {
		if strings.TrimSpace(bodies[i]) != w {
			t.Fatalf("body %d = %s, want %s", i, bodies[i], w)
		}
	}
}

func TestPipelineDefaultSetRequiresExactlyOneChoice(t *testing.T) {
	setConfigEnv(t)
	for _, args := range [][]string{
		{"pipeline", "default", "set"},
		{"pipeline", "default", "set", "bt", "--normal-worker"},
	} {
		_, _, err := executeCLI(t, Deps{}, args...)
		if err == nil || ExitCode(err) != 2 {
			t.Fatalf("args=%v err=%v", args, err)
		}
	}
}

func TestPipelineKeepsDaemonErrorEnvelope(t *testing.T) {
	pipelineServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid","code":"PIPELINE_WORKFLOW_NOT_FOUND","message":"Workflow \"ghost\" is not defined","requestId":"req-9"}`)
	})
	_, _, err := executeCLI(t, Deps{ProcessAlive: func(int) bool { return true }}, "pipeline", "default", "set", "ghost", "--project", "demo")
	if err == nil || ExitCode(err) != 1 || !strings.Contains(err.Error(), "PIPELINE_WORKFLOW_NOT_FOUND") || !strings.Contains(err.Error(), "req-9") {
		t.Fatalf("err=%v", err)
	}
}

func TestPipelineTrustAuthorizesAndRevokes(t *testing.T) {
	var bodies []string
	pipelineServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v1/projects/demo/pipelines/command-trust" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, strings.TrimSpace(string(raw)))
		_, _ = w.Write(raw)
	})
	deps := Deps{ProcessAlive: func(int) bool { return true }}
	if out, _, err := executeCLI(t, deps, "pipeline", "trust", "--project", "demo"); err != nil || !strings.Contains(out, "authorized") {
		t.Fatalf("trust: err=%v out=%s", err, out)
	}
	if out, _, err := executeCLI(t, deps, "pipeline", "trust", "--revoke", "--project", "demo"); err != nil || !strings.Contains(out, "revoked") {
		t.Fatalf("revoke: err=%v out=%s", err, out)
	}
	if len(bodies) != 2 || bodies[0] != `{"trusted":true}` || bodies[1] != `{"trusted":false}` {
		t.Fatalf("bodies: %v", bodies)
	}
}
