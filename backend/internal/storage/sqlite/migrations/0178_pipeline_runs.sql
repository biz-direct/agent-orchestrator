-- +goose Up
-- +goose StatementBegin
-- Specialist pipelines. A pipeline run is durable execution state attached to an
-- existing worker session. The worker stays the owner of the worktree, branch,
-- and PR; the run only records which stage is active, the frozen definition
-- snapshot, and the facts needed to accept or reject stage results.
--
-- Every object here is new, so the whole migration is additive and runs inside
-- one transaction: a crash leaves the database at the previous version.

-- Attached specialist stage sessions are hidden `sessions` rows that execute a
-- pipeline stage inside the owner worker's worktree. attached_to_session_id is
-- the single marker: store-level session listings exclude attached rows, so the
-- board, reaper, SCM observer and startup reconcile never see (or restart) them.
-- attached_for_attempt_id records the attempt a stage session was created for:
-- after a crash between creating the session and confirming the attempt, restart
-- finds the surviving session by attempt instead of launching a duplicate.
ALTER TABLE sessions ADD COLUMN attached_to_session_id TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN attached_for_attempt_id TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_sessions_attached_to ON sessions(attached_to_session_id) WHERE attached_to_session_id <> '';
CREATE UNIQUE INDEX idx_sessions_attached_for_attempt ON sessions(attached_for_attempt_id) WHERE attached_for_attempt_id <> '';

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
    -- `handoff` is a successor created but not yet confirmed started;
    -- `validating` is a verified specialist result whose AO-run deterministic
    -- checks have not finished, so nothing may execute in that state.
    state TEXT NOT NULL CHECK (state IN ('handoff', 'active', 'validating', 'accepted', 'failed', 'interrupted', 'cancelled')),
    -- The session that executes this attempt (the worker for Build) and the
    -- controller generation it was started under; results from any other
    -- generation are stale.
    executor_session_id TEXT NOT NULL DEFAULT '',
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
    predecessor_attempt_id TEXT NOT NULL DEFAULT '',
    result_json TEXT NOT NULL DEFAULT '',
    -- A repair is one automatic return to Build, counted exactly once across the
    -- durable run (see pipeline_repairs).
    repair_source_attempt_id TEXT NOT NULL DEFAULT '',
    return_stage_id TEXT NOT NULL DEFAULT '',
    feedback_json TEXT NOT NULL DEFAULT '',
    -- A resumed stage is a new attempt that records the interrupted attempt it
    -- continues, so a retry is never mistaken for a replay of the original.
    retry_of_attempt_id TEXT NOT NULL DEFAULT '',
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

-- One row per command execution of a validation round. A row is written as
-- `running` before the process starts, so a crash leaves proof that the command
-- may have run: it is reconciled to `unknown`, never inferred passed or retried.
CREATE TABLE pipeline_command_results (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    attempt_id TEXT NOT NULL REFERENCES pipeline_stage_attempts(id) ON DELETE CASCADE,
    round INTEGER NOT NULL CHECK (round >= 1),
    ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
    command_id TEXT NOT NULL,
    command TEXT NOT NULL,
    required INTEGER NOT NULL CHECK (required IN (0, 1)),
    revision TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('running', 'passed', 'failed', 'operational', 'timeout', 'cancelled', 'skipped', 'unknown')),
    exit_code INTEGER NOT NULL DEFAULT -1,
    started_at TIMESTAMP NOT NULL,
    finished_at TIMESTAMP,
    log TEXT NOT NULL DEFAULT '',
    log_truncated INTEGER NOT NULL DEFAULT 0 CHECK (log_truncated IN (0, 1)),
    detail TEXT NOT NULL DEFAULT '',
    UNIQUE (attempt_id, round, ordinal)
);
CREATE INDEX idx_pipeline_command_results_attempt ON pipeline_command_results(attempt_id, round, ordinal);

-- A repair is one automatic return to Build, counted exactly once across the
-- durable run. The unique source attempt makes duplicate feedback, restarts,
-- and concurrent transitions unable to spend the shared budget twice.
CREATE TABLE pipeline_repairs (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES pipeline_runs(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL CHECK (ordinal >= 1),
    source_attempt_id TEXT NOT NULL UNIQUE,
    source_stage_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('production_defect', 'validation_failed', 'review_feedback')),
    target_stage_id TEXT NOT NULL,
    return_stage_id TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL,
    UNIQUE (run_id, ordinal)
);

-- A Review stage attempt is linked to the exact pull request head it evaluates
-- and, once one exists, the AO review run for that head. The link is the
-- durable proof that a later approval or CI result belongs to this revision:
-- it can be filled in but never moved to another head.
CREATE TABLE pipeline_review_links (
    attempt_id TEXT PRIMARY KEY REFERENCES pipeline_stage_attempts(id) ON DELETE CASCADE,
    run_id TEXT NOT NULL REFERENCES pipeline_runs(id) ON DELETE CASCADE,
    pr_url TEXT NOT NULL CHECK (length(pr_url) > 0),
    head_sha TEXT NOT NULL CHECK (length(head_sha) > 0),
    review_run_id TEXT NOT NULL DEFAULT '',
    linked_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL
);

-- Extra automatic repairs need a distinct, persisted human authorization. Each
-- grant is one row, idempotent per (run, request key); the run's repair budget
-- grows in the same transaction, so ordinary resume can never extend it.
CREATE TABLE pipeline_repair_grants (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES pipeline_runs(id) ON DELETE CASCADE,
    amount INTEGER NOT NULL CHECK (amount >= 1),
    authorized_by TEXT NOT NULL CHECK (authorized_by = 'user'),
    request_key TEXT NOT NULL CHECK (length(request_key) > 0),
    note TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP NOT NULL,
    UNIQUE (run_id, request_key)
);

-- A pipeline selected when a task is created (explicitly, or from the project
-- default) cannot start until the worker is provisioned and has a controller.
-- The intent records that selection durably so the daemon starts it exactly once,
-- and records why it did not when it could not.
CREATE TABLE session_pipeline_intents (
    session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    project_id TEXT NOT NULL,
    workflow_id TEXT NOT NULL DEFAULT '',
    source TEXT NOT NULL CHECK (source IN ('explicit', 'default')),
    requested_by TEXT NOT NULL CHECK (requested_by IN ('user', 'orchestrator')),
    state TEXT NOT NULL CHECK (state IN ('pending', 'started', 'skipped', 'failed')),
    detail TEXT NOT NULL DEFAULT '',
    run_id TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL
);
CREATE INDEX idx_session_pipeline_intents_pending ON session_pipeline_intents(state) WHERE state = 'pending';

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

CREATE TRIGGER pipeline_command_results_insert_cdc
AFTER INSERT ON pipeline_command_results
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT r.project_id, r.session_id, 'session_updated', json_object('id', r.session_id), NEW.started_at
    FROM pipeline_stage_attempts a JOIN pipeline_runs r ON r.id = a.run_id WHERE a.id = NEW.attempt_id;
END;

CREATE TRIGGER pipeline_command_results_update_cdc
AFTER UPDATE ON pipeline_command_results
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT r.project_id, r.session_id, 'session_updated', json_object('id', r.session_id), COALESCE(NEW.finished_at, NEW.started_at)
    FROM pipeline_stage_attempts a JOIN pipeline_runs r ON r.id = a.run_id WHERE a.id = NEW.attempt_id;
END;

CREATE TRIGGER pipeline_repairs_insert_cdc
AFTER INSERT ON pipeline_repairs
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT r.project_id, r.session_id, 'session_updated', json_object('id', r.session_id), NEW.created_at
    FROM pipeline_runs r WHERE r.id = NEW.run_id;
END;

CREATE TRIGGER pipeline_review_links_insert_cdc
AFTER INSERT ON pipeline_review_links
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT r.project_id, r.session_id, 'session_updated', json_object('id', r.session_id), NEW.updated_at
    FROM pipeline_runs r WHERE r.id = NEW.run_id;
END;

CREATE TRIGGER pipeline_review_links_update_cdc
AFTER UPDATE ON pipeline_review_links
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT r.project_id, r.session_id, 'session_updated', json_object('id', r.session_id), NEW.updated_at
    FROM pipeline_runs r WHERE r.id = NEW.run_id;
END;

CREATE TRIGGER pipeline_repair_grants_insert_cdc
AFTER INSERT ON pipeline_repair_grants
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT r.project_id, r.session_id, 'session_updated', json_object('id', r.session_id), NEW.created_at
    FROM pipeline_runs r WHERE r.id = NEW.run_id;
END;

CREATE TRIGGER session_pipeline_intents_insert_cdc
AFTER INSERT ON session_pipeline_intents
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (NEW.project_id, NEW.session_id, 'session_updated', json_object('id', NEW.session_id), NEW.updated_at);
END;

CREATE TRIGGER session_pipeline_intents_update_cdc
AFTER UPDATE ON session_pipeline_intents
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (NEW.project_id, NEW.session_id, 'session_updated', json_object('id', NEW.session_id), NEW.updated_at);
END;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS session_pipeline_intents_update_cdc;
DROP TRIGGER IF EXISTS session_pipeline_intents_insert_cdc;
DROP TRIGGER IF EXISTS pipeline_repair_grants_insert_cdc;
DROP TRIGGER IF EXISTS pipeline_review_links_update_cdc;
DROP TRIGGER IF EXISTS pipeline_review_links_insert_cdc;
DROP TRIGGER IF EXISTS pipeline_repairs_insert_cdc;
DROP TRIGGER IF EXISTS pipeline_command_results_update_cdc;
DROP TRIGGER IF EXISTS pipeline_command_results_insert_cdc;
DROP TRIGGER IF EXISTS pipeline_events_insert_cdc;
DROP TRIGGER IF EXISTS pipeline_attempts_update_cdc;
DROP TRIGGER IF EXISTS pipeline_attempts_insert_cdc;
DROP TRIGGER IF EXISTS pipeline_runs_update_cdc;
DROP TRIGGER IF EXISTS pipeline_runs_insert_cdc;
DROP INDEX IF EXISTS idx_session_pipeline_intents_pending;
DROP TABLE IF EXISTS session_pipeline_intents;
DROP TABLE IF EXISTS pipeline_repair_grants;
DROP TABLE IF EXISTS pipeline_review_links;
DROP TABLE IF EXISTS pipeline_repairs;
DROP INDEX IF EXISTS idx_pipeline_command_results_attempt;
DROP TABLE IF EXISTS pipeline_command_results;
DROP INDEX IF EXISTS idx_pipeline_events_run;
DROP TABLE IF EXISTS pipeline_events;
DROP INDEX IF EXISTS idx_pipeline_attempts_run;
DROP TABLE IF EXISTS pipeline_stage_attempts;
DROP INDEX IF EXISTS idx_pipeline_runs_session;
DROP INDEX IF EXISTS idx_pipeline_runs_active_session;
DROP TABLE IF EXISTS pipeline_runs;
DROP INDEX IF EXISTS idx_sessions_attached_for_attempt;
DROP INDEX IF EXISTS idx_sessions_attached_to;
ALTER TABLE sessions DROP COLUMN attached_for_attempt_id;
ALTER TABLE sessions DROP COLUMN attached_to_session_id;
-- +goose StatementEnd
