-- +goose Up
-- +goose StatementBegin
-- The crew write baton: exactly one holder per task. A task with no row is held
-- by its worker (the default), so existing tasks need no backfill. holder_kind is
-- 'worker', 'human', or 'role' (holder_role names the role). One row per owner
-- session makes "exactly one holder" a table invariant; every move also appends
-- a crew_baton_events row in the same transaction.
-- Numbered in the fork's range (see 0250) so a later upstream migration cannot collide.
CREATE TABLE crew_baton (
    owner_session_id TEXT NOT NULL PRIMARY KEY,
    holder_kind      TEXT NOT NULL CHECK (holder_kind IN ('worker', 'human', 'role')),
    holder_role      TEXT NOT NULL DEFAULT '',
    -- The role session the grant was made to; a replacement session for the same
    -- role does not inherit it.
    holder_session_id TEXT NOT NULL DEFAULT '',
    updated_at       TIMESTAMP NOT NULL,
    CHECK ((holder_kind = 'role') = (holder_role <> ''))
);

CREATE TABLE crew_baton_events (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    owner_session_id TEXT NOT NULL,
    from_kind        TEXT NOT NULL,
    from_role        TEXT NOT NULL DEFAULT '',
    to_kind          TEXT NOT NULL,
    to_role          TEXT NOT NULL DEFAULT '',
    actor            TEXT NOT NULL,
    reason           TEXT NOT NULL DEFAULT '',
    created_at       TIMESTAMP NOT NULL
);
CREATE INDEX idx_crew_baton_events_owner ON crew_baton_events (owner_session_id, id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX idx_crew_baton_events_owner;
DROP TABLE crew_baton_events;
DROP TABLE crew_baton;
-- +goose StatementEnd
