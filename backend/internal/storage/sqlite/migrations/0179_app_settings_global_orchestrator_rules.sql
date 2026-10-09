-- +goose Up
-- +goose StatementBegin
-- Per-daemon global orchestrator rules: standing instructions added to every
-- project's orchestrator prompt on this host (unless a project opts out).
-- Empty means no global layer.
ALTER TABLE app_settings
    ADD COLUMN global_orchestrator_rules TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE app_settings DROP COLUMN global_orchestrator_rules;
-- +goose StatementEnd
