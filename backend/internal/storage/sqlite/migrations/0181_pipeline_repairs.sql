-- +goose Up
-- +goose StatementBegin
-- A repair is one automatic return to Build, counted exactly once across the
-- durable run. The unique source attempt makes duplicate feedback, restarts,
-- and concurrent transitions unable to spend the shared budget twice.
ALTER TABLE pipeline_stage_attempts ADD COLUMN repair_source_attempt_id TEXT NOT NULL DEFAULT '';
ALTER TABLE pipeline_stage_attempts ADD COLUMN return_stage_id TEXT NOT NULL DEFAULT '';
ALTER TABLE pipeline_stage_attempts ADD COLUMN feedback_json TEXT NOT NULL DEFAULT '';

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

CREATE TRIGGER pipeline_repairs_insert_cdc
AFTER INSERT ON pipeline_repairs
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    SELECT r.project_id, r.session_id, 'session_updated', json_object('id', r.session_id), NEW.created_at
    FROM pipeline_runs r WHERE r.id = NEW.run_id;
END;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS pipeline_repairs;
-- +goose StatementEnd
