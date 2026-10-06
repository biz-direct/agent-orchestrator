-- +goose NO TRANSACTION
-- +goose Up
-- +goose StatementBegin
PRAGMA foreign_keys=OFF;

DROP TRIGGER IF EXISTS pipeline_attempts_insert_cdc;
DROP TRIGGER IF EXISTS pipeline_attempts_update_cdc;

-- Rebuild attempts to admit `validating`: a specialist's result has been
-- verified but AO's own deterministic checks have not finished, so the stage is
-- neither executing nor accepted. Nothing may execute in this state.
CREATE TABLE pipeline_stage_attempts_next (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES pipeline_runs(id) ON DELETE CASCADE,
    stage_id TEXT NOT NULL,
    stage_kind TEXT NOT NULL,
    attempt_no INTEGER NOT NULL CHECK (attempt_no >= 1),
    state TEXT NOT NULL CHECK (state IN ('handoff', 'active', 'validating', 'accepted', 'failed', 'interrupted', 'cancelled')),
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
    UNIQUE (run_id, stage_id, attempt_no)
);
INSERT INTO pipeline_stage_attempts_next SELECT
    id, run_id, stage_id, stage_kind, attempt_no, state, executor_session_id, controller_generation,
    input_commit, output_commit, no_change, outcome, summary, result_key, instruction_delivery,
    started_at, finished_at, predecessor_attempt_id, result_json
FROM pipeline_stage_attempts;
DROP TABLE pipeline_stage_attempts;
ALTER TABLE pipeline_stage_attempts_next RENAME TO pipeline_stage_attempts;
CREATE INDEX idx_pipeline_attempts_run ON pipeline_stage_attempts(run_id, started_at, id);

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

PRAGMA foreign_keys=ON;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS pipeline_command_results;
-- +goose StatementEnd
