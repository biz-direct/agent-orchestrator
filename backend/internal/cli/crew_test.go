package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

const crewCatalogBody = `{
 "projectId":"demo",
 "roles":[
  {"id":"builder","file":".ao/crew/roles/builder.yaml","valid":true,"role":{"id":"builder","description":"Builds","instructionsSource":"inline"},"diagnostics":[]},
  {"id":"tester","file":".ao/crew/roles/tester.yaml","valid":false,"diagnostics":[{"file":".ao/crew/roles/tester.yaml","field":"harness","message":"unknown harness \"x\""}]}
 ],
 "workflows":[
  {"id":"bt","file":".ao/crew/workflows/bt.yaml","valid":true,"workflow":{"id":"bt","description":"Build then test","roles":["builder","tester"],"order":["builder","tester","review"],"returns":[],"loopCap":3},"diagnostics":[]}
 ],
 "diagnostics":[],
 "default":{"selection":{"mode":"workflow","workflowId":"bt"},"state":"workflow_available","message":"ok"}
}`

func crewDeps() Deps { return Deps{ProcessAlive: func(int) bool { return true }} }

func TestCrewListShowsRolesWorkflowsAndDefault(t *testing.T) {
	pipelineServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/demo/crew" {
			_, _ = io.WriteString(w, crewCatalogBody)
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	out, stderr, err := executeCLI(t, crewDeps(), "crew", "ls", "--project", "demo")
	if err != nil {
		t.Fatalf("ls: %v stderr=%s", err, stderr)
	}
	for _, want := range []string{"role", "builder", "tester", "invalid", "workflow", "bt", "builder → tester → review", "workflow bt (workflow_available)", "ao crew validate"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	jsonOut, _, err := executeCLI(t, crewDeps(), "crew", "ls", "--json", "--project", "demo")
	if err != nil {
		t.Fatal(err)
	}
	var parsed crewCatalogDTO
	if err := json.Unmarshal([]byte(jsonOut), &parsed); err != nil || len(parsed.Roles) != 2 || parsed.Default.State != "workflow_available" {
		t.Fatalf("json output: %v %s", err, jsonOut)
	}
}

func TestCrewValidateFailsWithFileFieldMessage(t *testing.T) {
	pipelineServer(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, crewCatalogBody) })
	out, _, err := executeCLI(t, crewDeps(), "crew", "validate", "--project", "demo")
	if err == nil || ExitCode(err) != 1 {
		t.Fatalf("invalid definitions must exit 1: err=%v", err)
	}
	if !strings.Contains(out, "tester.yaml [harness]: unknown harness") {
		t.Fatalf("diagnostic missing file/field/message:\n%s", out)
	}
}

func TestCrewValidateSucceedsOnCleanCatalog(t *testing.T) {
	pipelineServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"projectId":"demo","roles":[],"workflows":[],"diagnostics":[],"default":{"selection":null,"state":"unset","message":"none"}}`)
	})
	out, _, err := executeCLI(t, crewDeps(), "crew", "validate", "--project", "demo")
	if err != nil || !strings.Contains(out, "ok:") {
		t.Fatalf("err=%v out=%s", err, out)
	}
}

func TestCrewDefaultSetAndClear(t *testing.T) {
	var bodies []string
	pipelineServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v1/projects/demo/crew/default" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		var in crewSetDefaultDTO
		_ = json.Unmarshal(raw, &in)
		_ = json.NewEncoder(w).Encode(crewDefaultDTO{Selection: in.Selection, State: "unset", Message: "ok"})
	})
	if out, stderr, err := executeCLI(t, crewDeps(), "crew", "default", "set", "bt", "--project", "demo"); err != nil || !strings.Contains(out, "workflow bt") {
		t.Fatalf("set: err=%v out=%s stderr=%s", err, out, stderr)
	}
	if out, _, err := executeCLI(t, crewDeps(), "crew", "default", "set", "--no-crew", "--project", "demo"); err != nil || !strings.Contains(out, "no crew") {
		t.Fatalf("no crew: err=%v out=%s", err, out)
	}
	if out, _, err := executeCLI(t, crewDeps(), "crew", "default", "clear", "--project", "demo"); err != nil || !strings.Contains(out, "(none)") {
		t.Fatalf("clear: err=%v out=%s", err, out)
	}
	want := []string{
		`{"selection":{"mode":"workflow","workflowId":"bt"}}`,
		`{"selection":{"mode":"no_crew"}}`,
		`{"selection":null}`,
	}
	for i, w := range want {
		if strings.TrimSpace(bodies[i]) != w {
			t.Fatalf("body %d = %s, want %s", i, bodies[i], w)
		}
	}
}

func TestCrewDefaultGet(t *testing.T) {
	pipelineServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/projects/demo/crew/default" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"selection":{"mode":"no_crew"},"state":"no_crew","message":"explicit"}`)
	})
	out, _, err := executeCLI(t, crewDeps(), "crew", "default", "get", "--project", "demo")
	if err != nil || !strings.Contains(out, "default: no crew") {
		t.Fatalf("err=%v out=%s", err, out)
	}
}

func TestCrewDefaultSetRequiresExactlyOneChoice(t *testing.T) {
	setConfigEnv(t)
	for _, args := range [][]string{
		{"crew", "default", "set"},
		{"crew", "default", "set", "bt", "--no-crew"},
	} {
		_, _, err := executeCLI(t, Deps{}, args...)
		if err == nil || ExitCode(err) != 2 {
			t.Fatalf("args=%v err=%v", args, err)
		}
	}
}
