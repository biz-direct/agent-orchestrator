-- +goose Up
-- +goose StatementBegin
-- Per-daemon global worker rules: standing instructions added to every worker
-- prompt on this host (unless a project opts out). Empty means no global layer.
-- Numbered well above upstream's range on purpose: the fork owns this file and
-- goose runs with WithAllowMissing, so a later upstream migration cannot collide.
ALTER TABLE app_settings
    ADD COLUMN global_worker_rules TEXT NOT NULL DEFAULT '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE app_settings DROP COLUMN global_worker_rules;
-- +goose StatementEnd
