-- +goose NO TRANSACTION
-- +goose Up
-- +goose StatementBegin
PRAGMA foreign_keys=OFF;

-- Attached specialist stage sessions are hidden `sessions` rows that execute a
-- pipeline stage inside the owner worker's worktree. The column is the single
-- marker: store-level session listings exclude attached rows, so the board,
-- reaper, SCM observer and startup reconcile never see (or restart) them.
ALTER TABLE sessions ADD COLUMN attached_to_session_id TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_sessions_attached_to ON sessions(attached_to_session_id) WHERE attached_to_session_id <> '';

DROP TRIGGER IF EXISTS pipeline_attempts_insert_cdc;
DROP TRIGGER IF EXISTS pipeline_attempts_update_cdc;

-- Rebuild attempts to admit the `handoff` state (a successor created but not
-- yet confirmed started), and add result/predecessor facts.
CREATE TABLE pipeline_stage_attempts_next (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES pipeline_runs(id) ON DELETE CASCADE,
    stage_id TEXT NOT NULL,
    stage_kind TEXT NOT NULL,
    attempt_no INTEGER NOT NULL CHECK (attempt_no >= 1),
    state TEXT NOT NULL CHECK (state IN ('handoff', 'active', 'accepted', 'failed', 'interrupted', 'cancelled')),
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
INSERT INTO pipeline_stage_attempts_next (
    id, run_id, stage_id, stage_kind, attempt_no, state, executor_session_id, controller_generation,
    input_commit, output_commit, no_change, outcome, summary, result_key, instruction_delivery,
    started_at, finished_at
) SELECT
    id, run_id, stage_id, stage_kind, attempt_no, state, executor_session_id, controller_generation,
    input_commit, output_commit, no_change, outcome, summary, result_key, instruction_delivery,
    started_at, finished_at
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

PRAGMA foreign_keys=ON;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_sessions_attached_to;
ALTER TABLE sessions DROP COLUMN attached_to_session_id;
-- +goose StatementEnd
