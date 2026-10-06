package controllers_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	pipelinessvc "github.com/aoagents/agent-orchestrator/backend/internal/service/pipelines"
	projectsvc "github.com/aoagents/agent-orchestrator/backend/internal/service/project"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

func newPipelinesServer(t *testing.T) *httptest.Server {
	t.Helper()
	t.Setenv("GIT_CEILING_DIRECTORIES", os.TempDir())
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store, err := sqlitetest.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, log, nil, httpd.APIDeps{
		Projects:  projectsvc.New(store),
		Pipelines: pipelinessvc.New(store),
	}, httpd.ControlDeps{}))
	t.Cleanup(srv.Close)
	return srv
}

func writeRepoFile(t *testing.T, repo, rel, body string) {
	t.Helper()
	abs := filepath.Join(repo, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const apiTesterProfile = `version: 1
id: tester
description: Tester
instructions: write tests
allowedPaths: ["**/*_test.go"]
validation:
  - {id: unit, command: "go test ./..."}
`

const apiWorkflow = `version: 1
id: build-test
description: Build then test
stages:
  - {id: build, kind: build}
  - {id: test, kind: specialist, profile: tester, repairTo: build}
`

func TestPipelinesRoutes_DefaultToStubsWithoutManager(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, log, nil, httpd.APIDeps{}, httpd.ControlDeps{}))
	t.Cleanup(srv.Close)
	body, status, _ := doRequest(t, srv, "GET", "/api/v1/projects/p/pipelines", "")
	assertErrorCode(t, body, status, http.StatusNotImplemented, "NOT_IMPLEMENTED")
}

func TestPipelinesAPI_DiscoveryValidationAndDefaultPersistence(t *testing.T) {
	srv := newPipelinesServer(t)
	repo := gitRepo(t, "pipes")
	writeRepoFile(t, repo, ".ao/pipelines/profiles/tester.yaml", apiTesterProfile)
	writeRepoFile(t, repo, ".ao/pipelines/workflows/build-test.yaml", apiWorkflow)
	writeRepoFile(t, repo, ".ao/pipelines/workflows/broken.yaml", "version: 1\nid: broken\ndescription: x\nstages:\n  - {id: build, kind: build}\n  - {id: t, kind: specialist, profile: ghost}\n")

	_, status, _ := doRequest(t, srv, "POST", "/api/v1/projects", `{"path":`+quote(repo)+`,"projectId":"pipes","config":{"agentRules":"keep me","autoReview":true}}`)
	if status != http.StatusCreated {
		t.Fatalf("add project = %d", status)
	}

	// Discovery: one valid (but not yet executable) and one invalid workflow.
	body, status, headers := doRequest(t, srv, "GET", "/api/v1/projects/pipes/pipelines", "")
	if status != http.StatusOK {
		t.Fatalf("catalog = %d body=%s", status, body)
	}
	assertJSON(t, headers)
	var cat struct {
		Profiles []struct {
			ID    string `json:"id"`
			Valid bool   `json:"valid"`
		} `json:"profiles"`
		Workflows []struct {
			ID                string `json:"id"`
			Valid             bool   `json:"valid"`
			Executable        bool   `json:"executable"`
			UnavailableReason string `json:"unavailableReason"`
			Diagnostics       []struct {
				File    string `json:"file"`
				Message string `json:"message"`
			} `json:"diagnostics"`
		} `json:"workflows"`
		Default struct {
			State string `json:"state"`
		} `json:"default"`
	}
	mustJSON(t, body, &cat)
	if len(cat.Profiles) != 1 || !cat.Profiles[0].Valid || len(cat.Workflows) != 2 || cat.Default.State != "unset" {
		t.Fatalf("catalog: %s", body)
	}
	byID := map[string]int{}
	for i, w := range cat.Workflows {
		byID[w.ID] = i
	}
	good, bad := cat.Workflows[byID["build-test"]], cat.Workflows[byID["broken"]]
	if !good.Valid || good.Executable || good.UnavailableReason == "" {
		t.Fatalf("valid workflow must be visibly unavailable: %+v", good)
	}
	if bad.Valid || len(bad.Diagnostics) == 0 || !strings.Contains(bad.Diagnostics[0].Message, `unknown profile "ghost"`) || !strings.HasSuffix(bad.Diagnostics[0].File, "broken.yaml") {
		t.Fatalf("invalid workflow diagnostics: %+v", bad)
	}

	// Selecting an invalid or undefined workflow is refused with an actionable error.
	body, status, _ = doRequest(t, srv, "PUT", "/api/v1/projects/pipes/pipelines/default", `{"selection":{"mode":"workflow","workflowId":"broken"}}`)
	assertErrorCode(t, body, status, http.StatusBadRequest, "PIPELINE_WORKFLOW_INVALID")
	body, status, _ = doRequest(t, srv, "PUT", "/api/v1/projects/pipes/pipelines/default", `{"selection":{"mode":"workflow","workflowId":"ghost"}}`)
	assertErrorCode(t, body, status, http.StatusBadRequest, "PIPELINE_WORKFLOW_NOT_FOUND")

	// Saving a valid selection persists it and reports it as unavailable.
	body, status, _ = doRequest(t, srv, "PUT", "/api/v1/projects/pipes/pipelines/default", `{"selection":{"mode":"workflow","workflowId":"build-test"}}`)
	if status != http.StatusOK || !strings.Contains(string(body), `"state":"workflow_unavailable"`) || !strings.Contains(string(body), `"executable":false`) {
		t.Fatalf("set default = %d body=%s", status, body)
	}
	body, status, _ = doRequest(t, srv, "GET", "/api/v1/projects/pipes/pipelines/default", "")
	if status != http.StatusOK || !strings.Contains(string(body), `"workflowId":"build-test"`) {
		t.Fatalf("get default = %d body=%s", status, body)
	}

	// Unrelated project settings are untouched.
	body, status, _ = doRequest(t, srv, "GET", "/api/v1/projects/pipes", "")
	if status != http.StatusOK || !strings.Contains(string(body), `"agentRules":"keep me"`) || !strings.Contains(string(body), `"autoReview":true`) {
		t.Fatalf("project config was disturbed: %s", body)
	}

	// Explicit normal-worker choice persists and differs from clearing.
	body, status, _ = doRequest(t, srv, "PUT", "/api/v1/projects/pipes/pipelines/default", `{"selection":{"mode":"normal_worker"}}`)
	if status != http.StatusOK || !strings.Contains(string(body), `"state":"normal_worker"`) {
		t.Fatalf("normal worker = %d body=%s", status, body)
	}
	body, status, _ = doRequest(t, srv, "PUT", "/api/v1/projects/pipes/pipelines/default", `{"selection":null}`)
	if status != http.StatusOK || !strings.Contains(string(body), `"state":"unset"`) {
		t.Fatalf("clear = %d body=%s", status, body)
	}

	// Unknown request fields and unknown projects are rejected cleanly.
	body, status, _ = doRequest(t, srv, "PUT", "/api/v1/projects/pipes/pipelines/default", `{"selection":null,"extra":1}`)
	assertErrorCode(t, body, status, http.StatusBadRequest, "INVALID_JSON")
	body, status, _ = doRequest(t, srv, "GET", "/api/v1/projects/nope/pipelines", "")
	assertErrorCode(t, body, status, http.StatusNotFound, "PROJECT_NOT_FOUND")
}
