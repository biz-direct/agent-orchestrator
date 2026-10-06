-- +goose Up
-- +goose StatementBegin
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
DROP TABLE IF EXISTS session_pipeline_intents;
-- +goose StatementEnd
