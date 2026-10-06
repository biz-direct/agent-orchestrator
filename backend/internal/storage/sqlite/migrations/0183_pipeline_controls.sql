-- +goose Up
-- +goose StatementBegin
-- A resumed stage is a new attempt that records the interrupted attempt it
-- continues, so a retry is never mistaken for a replay of the original.
ALTER TABLE pipeline_stage_attempts ADD COLUMN retry_of_attempt_id TEXT NOT NULL DEFAULT '';

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

CREATE TRIGGER pipeline_repair_grants_insert_cdc
AFTER INSERT ON pipeline_repair_grants
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT r.project_id, r.session_id, 'session_updated', json_object('id', r.session_id), NEW.created_at
    FROM pipeline_runs r WHERE r.id = NEW.run_id;
END;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS pipeline_repair_grants;
-- +goose StatementEnd
