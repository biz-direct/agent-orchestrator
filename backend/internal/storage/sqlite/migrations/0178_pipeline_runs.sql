-- +goose Up
-- +goose StatementBegin
-- A pipeline run is durable execution state attached to an existing worker
-- session. The worker stays the owner of the worktree, branch, and PR; the run
-- only records which stage is active, the frozen definition snapshot, and the
-- facts needed to accept or reject stage results.
CREATE TABLE pipeline_runs (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    workflow_id TEXT NOT NULL CHECK (length(workflow_id) > 0),
    state TEXT NOT NULL CHECK (state IN ('running', 'paused', 'completed', 'cancelled')),
    pause_reason TEXT NOT NULL DEFAULT '',
    pause_detail TEXT NOT NULL DEFAULT '',
    current_stage_id TEXT NOT NULL DEFAULT '',
    requested_by TEXT NOT NULL CHECK (requested_by IN ('user', 'orchestrator')),
    expected_branch TEXT NOT NULL DEFAULT '',
    repair_budget INTEGER NOT NULL DEFAULT 3 CHECK (repair_budget >= 0),
    repairs_used INTEGER NOT NULL DEFAULT 0 CHECK (repairs_used >= 0),
    -- The frozen workflow/profile definitions, resolved instruction contents,
    -- validation configuration, and resolved harness/model settings.
    snapshot TEXT NOT NULL,
    snapshot_sha256 TEXT NOT NULL,
    revision INTEGER NOT NULL DEFAULT 1,
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    completed_at TIMESTAMP,
    CHECK ((state = 'paused' AND pause_reason <> '') OR (state <> 'paused' AND pause_reason = ''))
);

-- At most one run per session may be unfinished: only the active stage executes.
CREATE UNIQUE INDEX idx_pipeline_runs_active_session
    ON pipeline_runs(session_id) WHERE state IN ('running', 'paused');
CREATE INDEX idx_pipeline_runs_session ON pipeline_runs(session_id, created_at, id);

CREATE TABLE pipeline_stage_attempts (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES pipeline_runs(id) ON DELETE CASCADE,
    stage_id TEXT NOT NULL,
    stage_kind TEXT NOT NULL,
    attempt_no INTEGER NOT NULL CHECK (attempt_no >= 1),
    state TEXT NOT NULL CHECK (state IN ('active', 'accepted', 'failed', 'interrupted', 'cancelled')),
    -- The session that executes this attempt (the worker for Build) and the
    -- controller generation it was started under; results from any other
    -- generation are stale.
    executor_session_id TEXT NOT NULL,
    controller_generation TEXT NOT NULL DEFAULT '',
    input_commit TEXT NOT NULL DEFAULT '',
    output_commit TEXT NOT NULL DEFAULT '',
    no_change INTEGER NOT NULL DEFAULT 0 CHECK (no_change IN (0, 1)),
    outcome TEXT NOT NULL DEFAULT '',
    summary TEXT NOT NULL DEFAULT '',
    result_key TEXT NOT NULL DEFAULT '',
    instruction_delivery TEXT NOT NULL DEFAULT 'pending' CHECK (instruction_delivery IN ('pending', 'delivered', 'failed')),
    started_at TIMESTAMP NOT NULL,
    finished_at TIMESTAMP,
    UNIQUE (run_id, stage_id, attempt_no)
);
CREATE INDEX idx_pipeline_attempts_run ON pipeline_stage_attempts(run_id, started_at, id);

-- Append-only execution facts: rejected submissions, pauses, deliveries.
CREATE TABLE pipeline_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id TEXT NOT NULL REFERENCES pipeline_runs(id) ON DELETE CASCADE,
    attempt_id TEXT NOT NULL DEFAULT '',
    kind TEXT NOT NULL CHECK (length(kind) > 0),
    detail TEXT NOT NULL DEFAULT '{}',
    created_at TIMESTAMP NOT NULL
);
CREATE INDEX idx_pipeline_events_run ON pipeline_events(run_id, id);

-- Pipeline changes surface through the existing session CDC stream.
CREATE TRIGGER pipeline_runs_insert_cdc
AFTER INSERT ON pipeline_runs
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (NEW.project_id, NEW.session_id, 'session_updated', json_object('id', NEW.session_id), NEW.updated_at);
END;

CREATE TRIGGER pipeline_runs_update_cdc
AFTER UPDATE ON pipeline_runs
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (NEW.project_id, NEW.session_id, 'session_updated', json_object('id', NEW.session_id), NEW.updated_at);
END;

CREATE TRIGGER pipeline_attempts_insert_cdc
AFTER INSERT ON pipeline_stage_attempts
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT r.project_id, r.session_id, 'session_updated', json_object('id', r.session_id), NEW.started_at
    FROM pipeline_runs r WHERE r.id = NEW.run_id;
END;

CREATE TRIGGER pipeline_attempts_update_cdc
AFTER UPDATE ON pipeline_stage_attempts
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT r.project_id, r.session_id, 'session_updated', json_object('id', r.session_id), COALESCE(NEW.finished_at, NEW.started_at)
    FROM pipeline_runs r WHERE r.id = NEW.run_id;
END;

CREATE TRIGGER pipeline_events_insert_cdc
AFTER INSERT ON pipeline_events
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT r.project_id, r.session_id, 'session_updated', json_object('id', r.session_id), NEW.created_at
    FROM pipeline_runs r WHERE r.id = NEW.run_id;
END;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS pipeline_events_insert_cdc;
DROP TRIGGER IF EXISTS pipeline_attempts_update_cdc;
DROP TRIGGER IF EXISTS pipeline_attempts_insert_cdc;
DROP TRIGGER IF EXISTS pipeline_runs_update_cdc;
DROP TRIGGER IF EXISTS pipeline_runs_insert_cdc;
DROP INDEX IF EXISTS idx_pipeline_events_run;
DROP TABLE IF EXISTS pipeline_events;
DROP INDEX IF EXISTS idx_pipeline_attempts_run;
DROP TABLE IF EXISTS pipeline_stage_attempts;
DROP INDEX IF EXISTS idx_pipeline_runs_session;
DROP INDEX IF EXISTS idx_pipeline_runs_active_session;
DROP TABLE IF EXISTS pipeline_runs;
-- +goose StatementEnd
