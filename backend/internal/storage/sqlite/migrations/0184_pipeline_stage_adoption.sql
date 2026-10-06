-- +goose Up
-- +goose StatementBegin
-- An attached stage session records the attempt it was created for. After a
-- crash between creating the session and confirming the attempt, restart finds
-- the surviving session by attempt instead of launching a duplicate.
ALTER TABLE sessions ADD COLUMN attached_for_attempt_id TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_sessions_attached_for_attempt ON sessions(attached_for_attempt_id) WHERE attached_for_attempt_id <> '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_sessions_attached_for_attempt;
ALTER TABLE sessions DROP COLUMN attached_for_attempt_id;
-- +goose StatementEnd
