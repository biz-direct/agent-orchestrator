-- +goose Up
-- +goose StatementBegin
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
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS pipeline_review_links;
-- +goose StatementEnd
