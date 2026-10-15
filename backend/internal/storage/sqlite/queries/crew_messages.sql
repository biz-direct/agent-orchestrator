-- name: InsertCrewMessage :one
INSERT INTO crew_messages (
    id, owner_session_id, sender_role, sender_session_id, target_role, target_session_id,
    idempotency_key, body, mode, commit_sha, baton_holder, state, state_reason, created_at, updated_at
) VALUES (
    sqlc.arg(id), sqlc.arg(owner_session_id), sqlc.arg(sender_role), sqlc.arg(sender_session_id),
    sqlc.arg(target_role), sqlc.arg(target_session_id), sqlc.arg(idempotency_key), sqlc.arg(body),
    sqlc.arg(mode), sqlc.arg(commit_sha), sqlc.arg(baton_holder), sqlc.arg(state), sqlc.arg(state_reason),
    sqlc.arg(created_at), sqlc.arg(created_at)
)
RETURNING *;

-- name: GetCrewMessage :one
SELECT * FROM crew_messages WHERE id = sqlc.arg(id);

-- name: GetCrewMessageByKey :one
SELECT * FROM crew_messages
WHERE owner_session_id = sqlc.arg(owner_session_id)
  AND sender_role = sqlc.arg(sender_role)
  AND idempotency_key = sqlc.arg(idempotency_key);

-- name: ListCrewMessages :many
SELECT * FROM crew_messages WHERE owner_session_id = sqlc.arg(owner_session_id) ORDER BY seq;

-- name: CountCrewMessagesForPairSince :one
SELECT COUNT(*) FROM crew_messages
WHERE owner_session_id = sqlc.arg(owner_session_id)
  AND sender_role = sqlc.arg(sender_role)
  AND target_role = sqlc.arg(target_role)
  AND state != 'dropped'
  AND created_at >= sqlc.arg(since);

-- name: ListCrewMessageTargetsWithPending :many
SELECT DISTINCT target_session_id FROM crew_messages WHERE state = 'queued';

-- name: ListQueuedCrewMessagesForTarget :many
SELECT * FROM crew_messages
WHERE target_session_id = sqlc.arg(target_session_id) AND state = 'queued'
ORDER BY seq;

-- name: ClaimCrewMessage :execrows
UPDATE crew_messages
SET state = 'delivering', attempts = attempts + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND state = 'queued';

-- name: MarkCrewMessageDelivered :execrows
UPDATE crew_messages
SET state = 'delivered', state_reason = '', delivered_at = sqlc.arg(now), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND state = 'delivering';

-- name: RequeueCrewMessage :execrows
UPDATE crew_messages
SET state = 'queued', state_reason = sqlc.arg(state_reason), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND state = 'delivering';

-- name: FailCrewMessage :execrows
UPDATE crew_messages
SET state = 'failed', state_reason = sqlc.arg(state_reason), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND state IN ('queued', 'delivering');

-- name: RequeueStaleCrewMessages :execrows
UPDATE crew_messages
SET state = 'queued', state_reason = 'delivery interrupted; retrying', updated_at = sqlc.arg(now)
WHERE state = 'delivering' AND updated_at < sqlc.arg(before);

-- name: SetCrewMessageWaitReason :exec
UPDATE crew_messages SET state_reason = sqlc.arg(state_reason)
WHERE id = sqlc.arg(id) AND state = 'queued' AND state_reason != sqlc.arg(state_reason);

-- name: EditCrewMessage :execrows
UPDATE crew_messages SET body = sqlc.arg(body), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND state IN ('queued', 'held');

-- name: DropCrewMessage :execrows
UPDATE crew_messages SET state = 'dropped', state_reason = 'dropped by a person', updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND state IN ('queued', 'held');

-- name: ReleaseCrewMessage :execrows
UPDATE crew_messages SET state = 'queued', state_reason = '', updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND state = 'held';
