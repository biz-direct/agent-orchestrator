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

func TestCrewAddRolesRemoveTalkToTheTaskRoleRoutes(t *testing.T) {
	var seen []string
	pipelineServer(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sessions/w-1/crew/roles":
			var body crewAddRoleDTO
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Role != "builder" || body.Harness != "codex" {
				t.Errorf("body = %+v err=%v", body, err)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"sessionId":"w-att-2","ownerSessionId":"w-1","role":"builder","harness":"codex","controllerActive":true}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/sessions/w-1/crew/roles":
			_, _ = io.WriteString(w, `{"roles":[{"sessionId":"w-att-2","ownerSessionId":"w-1","role":"builder","harness":"codex","controllerActive":true}]}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/sessions/w-1/crew/roles/builder":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	out, stderr, err := executeCLI(t, crewDeps(), "crew", "add", "builder", "--session", "w-1", "--harness", "codex")
	if err != nil || !strings.Contains(out, "added role builder to w-1 as session w-att-2") {
		t.Fatalf("add: %v out=%s stderr=%s", err, out, stderr)
	}
	out, _, err = executeCLI(t, crewDeps(), "crew", "roles", "--session", "w-1")
	if err != nil || !strings.Contains(out, "builder") || !strings.Contains(out, "w-att-2") {
		t.Fatalf("roles: %v %s", err, out)
	}
	out, _, err = executeCLI(t, crewDeps(), "crew", "remove", "builder", "--session", "w-1")
	if err != nil || !strings.Contains(out, "removed role builder") {
		t.Fatalf("remove: %v %s", err, out)
	}
	if len(seen) != 3 {
		t.Fatalf("requests: %v", seen)
	}
}

func TestCrewAddRefusalIsVisibleAndNeedsASession(t *testing.T) {
	pipelineServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"bad_request","code":"CREW_ROLE_HARNESS_UNSUPPORTED","message":"harness x has no Chat driver"}`)
	})
	_, stderr, err := executeCLI(t, crewDeps(), "crew", "add", "builder", "--session", "w-1")
	if err == nil || !strings.Contains(err.Error()+stderr, "no Chat driver") {
		t.Fatalf("an unsupported harness must be refused visibly: err=%v stderr=%s", err, stderr)
	}
	_, _, err = executeCLI(t, crewDeps(), "crew", "add", "builder")
	if err == nil || ExitCode(err) != 2 {
		t.Fatalf("missing session is a usage error: %v", err)
	}
}

func TestCrewBatonGiveTakeAndStatus(t *testing.T) {
	var bodies []string
	pipelineServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/api/v1/sessions/w-1/crew/baton":
			raw, _ := io.ReadAll(r.Body)
			bodies = append(bodies, string(raw))
			var in crewMoveBatonDTO
			_ = json.Unmarshal(raw, &in)
			_ = json.NewEncoder(w).Encode(crewBatonStatusDTO{OwnerSessionID: "w-1", Holder: in.To})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/sessions/w-1/crew/baton":
			_, _ = io.WriteString(w, `{"ownerSessionId":"w-1","holder":{"kind":"role","role":"builder"},"history":[{"from":{"kind":"worker"},"to":{"kind":"role","role":"builder"},"actor":"human","reason":"build it","createdAt":"2026-10-14T10:00:00Z"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/sessions/w-1/crew/roles":
			_, _ = io.WriteString(w, `{"roles":[{"sessionId":"w-att-2","ownerSessionId":"w-1","role":"builder","harness":"codex","controllerActive":true},{"sessionId":"w-att-3","ownerSessionId":"w-1","role":"tester","harness":"codex","controllerActive":true}]}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	if out, stderr, err := executeCLI(t, crewDeps(), "crew", "baton", "give", "builder", "--reason", "build it", "--session", "w-1"); err != nil || !strings.Contains(out, "role builder") {
		t.Fatalf("give: err=%v out=%s stderr=%s", err, out, stderr)
	}
	if out, _, err := executeCLI(t, crewDeps(), "crew", "baton", "take", "--session", "w-1"); err != nil || !strings.Contains(out, "human") {
		t.Fatalf("take: err=%v out=%s", err, out)
	}
	if out, _, err := executeCLI(t, crewDeps(), "crew", "baton", "give", "worker", "--session", "w-1"); err != nil || !strings.Contains(out, "worker") {
		t.Fatalf("give worker: err=%v out=%s", err, out)
	}
	want := []string{
		`{"to":{"kind":"role","role":"builder"},"reason":"build it"}`,
		`{"to":{"kind":"human"}}`,
		`{"to":{"kind":"worker"}}`,
	}
	if strings.Join(bodies, "\n") != strings.Join(want, "\n") {
		t.Fatalf("bodies = %v", bodies)
	}
	out, _, err := executeCLI(t, crewDeps(), "crew", "status", "--session", "w-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"write baton: role builder", "writes (baton)", "read-only", "worker -> role builder  by human: build it", "tester"} {
		if !strings.Contains(strings.ReplaceAll(out, "  ", " "), strings.ReplaceAll(want, "  ", " ")) {
			t.Fatalf("status missing %q:\n%s", want, out)
		}
	}
	if _, _, err := executeCLI(t, crewDeps(), "crew", "baton", "give", "--session", "w-1"); err == nil {
		t.Fatal("give needs a target")
	}
}
