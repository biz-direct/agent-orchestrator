package controllers_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/config"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/pipelineruns"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/sqlitetest"
)

type captureMessenger struct{ sent []string }

func (m *captureMessenger) Send(_ context.Context, _ domain.SessionID, msg string, _ *ports.SpawnAttachment) error {
	m.sent = append(m.sent, msg)
	return nil
}

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestPipelineRunsAPI_StartInspectSubmit(t *testing.T) {
	repo := t.TempDir()
	gitCmd(t, repo, "init", "-q", "-b", "task")
	writeRepoFile(t, repo, "README.md", "x\n")
	writeRepoFile(t, repo, ".ao/pipelines/workflows/build-only.yaml", "version: 1\nid: build-only\ndescription: d\nstages:\n  - {id: build, kind: build}\n")
	gitCmd(t, repo, "add", "-A")
	gitCmd(t, repo, "commit", "-q", "-m", "init")

	store := sqlitetest.MustOpen(t)
	now := time.Now().UTC().Truncate(time.Second)
	ctx := context.Background()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "proj", Path: repo, RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	sess, err := store.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "proj", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat,
		Activity: domain.Activity{State: domain.ActivityActive, LastActivityAt: now},
		Metadata: domain.SessionMetadata{WorkspacePath: repo, ControllerGeneration: "gen-1"}, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	msgr := &captureMessenger{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, log, nil, httpd.APIDeps{
		PipelineRuns: pipelineruns.New(pipelineruns.Deps{Store: store, Messenger: msgr}),
	}, httpd.ControlDeps{}))
	t.Cleanup(srv.Close)
	base := "/api/v1/sessions/" + string(sess.ID) + "/pipeline"

	// Before a run exists the task is an ordinary worker.
	body, status, _ := doRequest(t, srv, "GET", base, "")
	if status != http.StatusOK || strings.TrimSpace(string(body)) != `{"run":null}` {
		t.Fatalf("no run yet: %d %s", status, body)
	}

	// Orchestrator override requests are refused; explicit selection is required.
	body, status, _ = doRequest(t, srv, "POST", base, `{"workflowId":"build-only","requestedBy":"orchestrator","overrides":{"build":{"model":"x"}}}`)
	assertErrorCode(t, body, status, http.StatusForbidden, "PIPELINE_OVERRIDE_USER_ONLY")
	body, status, _ = doRequest(t, srv, "POST", base, `{"requestedBy":"user"}`)
	assertErrorCode(t, body, status, http.StatusBadRequest, "PIPELINE_WORKFLOW_REQUIRED")

	body, status, _ = doRequest(t, srv, "POST", base, `{"workflowId":"build-only","requestedBy":"user"}`)
	if status != http.StatusCreated {
		t.Fatalf("start = %d %s", status, body)
	}
	var started struct {
		Run struct {
			ID       string `json:"id"`
			State    string `json:"state"`
			Attempts []struct {
				ID                   string `json:"id"`
				InputCommit          string `json:"inputCommit"`
				ControllerGeneration string `json:"controllerGeneration"`
			} `json:"attempts"`
		} `json:"run"`
	}
	mustJSON(t, body, &started)
	att := started.Run.Attempts[0]
	if started.Run.State != "running" || att.InputCommit == "" || len(msgr.sent) != 1 {
		t.Fatalf("started: %s", body)
	}
	body, status, _ = doRequest(t, srv, "POST", base, `{"workflowId":"build-only","requestedBy":"user"}`)
	assertErrorCode(t, body, status, http.StatusConflict, "PIPELINE_RUN_ACTIVE")

	submit := func(output, key string) ([]byte, int) {
		payload, _ := json.Marshal(map[string]any{
			"runId": started.Run.ID, "attemptId": att.ID, "controllerGeneration": att.ControllerGeneration,
			"idempotencyKey": key, "outcome": "succeeded", "expectedInputCommit": att.InputCommit, "outputCommit": output, "summary": "ok",
		})
		b, s, _ := doRequest(t, srv, "POST", base+"/results", string(payload))
		return b, s
	}

	// A dirty worktree blocks acceptance, is preserved, and is explained.
	writeRepoFile(t, repo, "wip.txt", "wip\n")
	body, status = submit(att.InputCommit, "k1")
	assertErrorCode(t, body, status, http.StatusConflict, "PIPELINE_DIRTY_WORKSPACE")
	if !strings.Contains(string(body), "wip.txt") {
		t.Fatalf("dirty paths must be explained: %s", body)
	}
	if _, err := os.Stat(repo + "/wip.txt"); err != nil {
		t.Fatal("dirty work must be preserved")
	}
	gitCmd(t, repo, "add", "-A")
	gitCmd(t, repo, "commit", "-q", "-m", "work")
	head := gitCmd(t, repo, "rev-parse", "HEAD")

	body, status = submit(head, "k2")
	if status != http.StatusOK || !strings.Contains(string(body), `"accepted":true`) || !strings.Contains(string(body), `"state":"completed"`) {
		t.Fatalf("accept = %d %s", status, body)
	}
	body, status = submit(head, "k2")
	if status != http.StatusOK || !strings.Contains(string(body), `"replayed":true`) {
		t.Fatalf("replay = %d %s", status, body)
	}
	body, status, _ = doRequest(t, srv, "GET", base, "")
	if status != http.StatusOK || !strings.Contains(string(body), `"outputCommit":"`+head+`"`) {
		t.Fatalf("checkpoint must be readable: %d %s", status, body)
	}
}

func TestPipelineRunsAPI_Control(t *testing.T) {
	repo := t.TempDir()
	gitCmd(t, repo, "init", "-q", "-b", "task")
	writeRepoFile(t, repo, "README.md", "x\n")
	writeRepoFile(t, repo, ".ao/pipelines/workflows/build-only.yaml", "version: 1\nid: build-only\ndescription: d\nstages:\n  - {id: build, kind: build}\n")
	gitCmd(t, repo, "add", "-A")
	gitCmd(t, repo, "commit", "-q", "-m", "init")
	store := sqlitetest.MustOpen(t)
	now := time.Now().UTC().Truncate(time.Second)
	ctx := context.Background()
	if err := store.UpsertProject(ctx, domain.ProjectRecord{ID: "proj", Path: repo, RegisteredAt: now}); err != nil {
		t.Fatal(err)
	}
	sess, err := store.CreateSession(ctx, domain.SessionRecord{
		ProjectID: "proj", Kind: domain.KindWorker, Harness: domain.HarnessClaudeCode, Mode: domain.SessionModeChat,
		Activity: domain.Activity{State: domain.ActivityActive, LastActivityAt: now},
		Metadata: domain.SessionMetadata{WorkspacePath: repo, ControllerGeneration: "gen-1"}, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, log, nil, httpd.APIDeps{
		PipelineRuns: pipelineruns.New(pipelineruns.Deps{Store: store, Messenger: &captureMessenger{}}),
	}, httpd.ControlDeps{}))
	t.Cleanup(srv.Close)
	base := "/api/v1/sessions/" + string(sess.ID) + "/pipeline"

	body, status, _ := doRequest(t, srv, "POST", base+"/control", `{"runId":"prun_x","action":"pause","requestedBy":"user"}`)
	assertErrorCode(t, body, status, http.StatusNotFound, "PIPELINE_RUN_NOT_FOUND")

	body, status, _ = doRequest(t, srv, "POST", base, `{"workflowId":"build-only","requestedBy":"user"}`)
	if status != http.StatusCreated {
		t.Fatalf("start = %d %s", status, body)
	}
	var started struct {
		Run struct {
			ID       string `json:"id"`
			Revision int64  `json:"revision"`
		} `json:"run"`
	}
	mustJSON(t, body, &started)
	control := func(action, by string, extra string) ([]byte, int) {
		b, st, _ := doRequest(t, srv, "POST", base+"/control", `{"runId":"`+started.Run.ID+`","action":"`+action+`","requestedBy":"`+by+`"`+extra+`}`)
		return b, st
	}

	body, status = control("pause", "user", `,"reason":"lunch"`)
	if status != http.StatusOK || !strings.Contains(string(body), `"changed":true`) || !strings.Contains(string(body), `"pauseReason":"paused_by_user"`) || !strings.Contains(string(body), `"canResume":true`) {
		t.Fatalf("pause = %d %s", status, body)
	}
	body, status = control("pause", "user", "")
	if status != http.StatusOK || !strings.Contains(string(body), `"changed":false`) {
		t.Fatalf("a repeated pause is idempotent: %d %s", status, body)
	}
	body, status = control("resume", "user", `,"expectedRevision":1`)
	assertErrorCode(t, body, status, http.StatusConflict, "PIPELINE_STALE_CONTROL")
	body, status = control("authorize_repairs", "orchestrator", `,"additionalRepairs":1,"requestKey":"k"`)
	assertErrorCode(t, body, status, http.StatusForbidden, "PIPELINE_HUMAN_AUTHORIZATION_REQUIRED")
	body, status = control("resume", "orchestrator", "")
	if status != http.StatusOK || !strings.Contains(string(body), `"state":"running"`) {
		t.Fatalf("resume = %d %s", status, body)
	}
	body, status = control("cancel", "user", `,"reason":"changed my mind"`)
	if status != http.StatusOK || !strings.Contains(string(body), `"state":"cancelled"`) {
		t.Fatalf("cancel = %d %s", status, body)
	}
	body, status = control("cancel", "user", "")
	if status != http.StatusOK || !strings.Contains(string(body), `"changed":false`) {
		t.Fatalf("a repeated cancel is idempotent: %d %s", status, body)
	}
	body, status = control("resume", "user", "")
	assertErrorCode(t, body, status, http.StatusConflict, "PIPELINE_RUN_FINISHED")
	body, status, _ = doRequest(t, srv, "POST", base+"/control", `{"runId":"`+started.Run.ID+`","action":"nope","requestedBy":"user"}`)
	assertErrorCode(t, body, status, http.StatusBadRequest, "INVALID_PIPELINE_CONTROL")
}

func TestPipelineRunsRoutes_DefaultToStubsWithoutManager(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(httpd.NewRouterWithControl(config.Config{}, log, nil, httpd.APIDeps{}, httpd.ControlDeps{}))
	t.Cleanup(srv.Close)
	body, status, _ := doRequest(t, srv, "GET", "/api/v1/sessions/s/pipeline", "")
	assertErrorCode(t, body, status, http.StatusNotImplemented, "NOT_IMPLEMENTED")
}
