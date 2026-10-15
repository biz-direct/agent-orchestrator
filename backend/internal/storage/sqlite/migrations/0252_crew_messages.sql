-- +goose Up
-- +goose StatementBegin
-- Durable crew messages: one row per role-to-role message on a task crew. The
-- row is the outbox entry, the delivery receipt, and the crew thread the task
-- view shows. Its id is the chat layer's client message id, so a delivery that
-- is retried after a crash or concurrent dispatch lands exactly once.
-- Numbered in the fork's range (see 0250) so upstream migrations cannot collide.
CREATE TABLE crew_messages (
    seq               INTEGER PRIMARY KEY AUTOINCREMENT,
    id                TEXT    NOT NULL UNIQUE,
    owner_session_id  TEXT    NOT NULL,
    sender_role       TEXT    NOT NULL,
    sender_session_id TEXT    NOT NULL,
    target_role       TEXT    NOT NULL,
    target_session_id TEXT    NOT NULL,
    idempotency_key   TEXT    NOT NULL,
    body              TEXT    NOT NULL,
    mode              TEXT    NOT NULL DEFAULT 'queue' CHECK (mode IN ('queue', 'steer')),
    commit_sha        TEXT    NOT NULL DEFAULT '',
    baton_holder      TEXT    NOT NULL DEFAULT '',
    state             TEXT    NOT NULL DEFAULT 'queued'
        CHECK (state IN ('queued', 'held', 'delivering', 'delivered', 'dropped', 'failed')),
    state_reason      TEXT    NOT NULL DEFAULT '',
    attempts          INTEGER NOT NULL DEFAULT 0,
    created_at        TIMESTAMP    NOT NULL,
    updated_at        TIMESTAMP    NOT NULL,
    delivered_at      TIMESTAMP,
    UNIQUE (owner_session_id, sender_role, idempotency_key)
);

CREATE INDEX idx_crew_messages_owner ON crew_messages (owner_session_id, seq);
CREATE INDEX idx_crew_messages_pair ON crew_messages (owner_session_id, sender_role, target_role, created_at);
CREATE INDEX idx_crew_messages_pending ON crew_messages (state, seq) WHERE state IN ('queued', 'delivering');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE crew_messages;
-- +goose StatementEnd
