-- name: CreatePipelineRun :one
INSERT INTO pipeline_runs (
    id, session_id, project_id, workflow_id, state, pause_reason, pause_detail,
    current_stage_id, requested_by, expected_branch, repair_budget, repairs_used,
    snapshot, snapshot_sha256, revision, created_at, updated_at
)
VALUES (?, ?, ?, ?, ?, '', '', ?, ?, ?, ?, 0, ?, ?, 1, ?, ?)
RETURNING *;

-- name: GetPipelineRun :one
SELECT * FROM pipeline_runs WHERE id = ?;

-- name: GetActivePipelineRunBySession :one
SELECT * FROM pipeline_runs
WHERE session_id = ? AND state IN ('running', 'paused');

-- name: GetLatestPipelineRunBySession :one
SELECT * FROM pipeline_runs
WHERE session_id = ?
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: ListUnfinishedPipelineRuns :many
SELECT * FROM pipeline_runs WHERE state IN ('running', 'paused') ORDER BY created_at, id;

-- name: SetPipelineRunState :one
UPDATE pipeline_runs
SET state = sqlc.arg(state),
    pause_reason = sqlc.arg(pause_reason),
    pause_detail = sqlc.arg(pause_detail),
    current_stage_id = sqlc.arg(current_stage_id),
    updated_at = sqlc.arg(updated_at),
    completed_at = sqlc.arg(completed_at),
    repairs_used = repairs_used + sqlc.arg(repairs_delta),
    repair_budget = repair_budget + sqlc.arg(budget_delta),
    revision = revision + 1
WHERE id = sqlc.arg(id) AND revision = sqlc.arg(expected_revision)
RETURNING *;

-- name: CreatePipelineStageAttempt :one
INSERT INTO pipeline_stage_attempts (
    id, run_id, stage_id, stage_kind, attempt_no, state, executor_session_id,
    controller_generation, input_commit, instruction_delivery, started_at, predecessor_attempt_id,
    repair_source_attempt_id, return_stage_id, feedback_json, retry_of_attempt_id
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: ActivatePipelineStageAttempt :one
UPDATE pipeline_stage_attempts
SET state = 'active',
    executor_session_id = sqlc.arg(executor_session_id),
    controller_generation = sqlc.arg(controller_generation),
    started_at = sqlc.arg(started_at)
WHERE id = sqlc.arg(id) AND state = 'handoff'
RETURNING *;

-- name: GetPipelineStageAttempt :one
SELECT * FROM pipeline_stage_attempts WHERE id = ?;

-- name: ListPipelineStageAttempts :many
SELECT * FROM pipeline_stage_attempts WHERE run_id = ? ORDER BY started_at, attempt_no, id;

-- name: FinishPipelineStageAttempt :one
UPDATE pipeline_stage_attempts
SET state = sqlc.arg(state),
    output_commit = sqlc.arg(output_commit),
    no_change = sqlc.arg(no_change),
    outcome = sqlc.arg(outcome),
    summary = sqlc.arg(summary),
    result_key = sqlc.arg(result_key),
    result_json = sqlc.arg(result_json),
    feedback_json = CASE WHEN sqlc.arg(feedback_json) <> '' THEN sqlc.arg(feedback_json) ELSE feedback_json END,
    finished_at = sqlc.arg(finished_at)
WHERE id = sqlc.arg(id) AND state IN ('active', 'handoff', 'validating')
RETURNING *;

-- name: SetPipelineAttemptInstructionDelivery :exec
UPDATE pipeline_stage_attempts SET instruction_delivery = ? WHERE id = ?;

-- name: CreatePipelineEvent :exec
INSERT INTO pipeline_events (run_id, attempt_id, kind, detail, created_at)
VALUES (?, ?, ?, ?, ?);

-- name: ListPipelineEvents :many
SELECT * FROM pipeline_events WHERE run_id = ? ORDER BY id DESC LIMIT ?;

-- name: HasUnfinishedPipelineRun :one
SELECT EXISTS (
    SELECT 1 FROM pipeline_runs WHERE session_id = ? AND state IN ('running', 'paused')
);

-- name: StartPipelineAttemptValidation :one
UPDATE pipeline_stage_attempts
SET state = 'validating',
    output_commit = sqlc.arg(output_commit),
    no_change = sqlc.arg(no_change),
    outcome = sqlc.arg(outcome),
    summary = sqlc.arg(summary),
    result_key = sqlc.arg(result_key),
    result_json = sqlc.arg(result_json)
WHERE id = sqlc.arg(id) AND state = 'active'
RETURNING *;

-- name: CreatePipelineCommandResult :one
INSERT INTO pipeline_command_results (
    attempt_id, round, ordinal, command_id, command, required, revision, status, started_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: FinishPipelineCommandResult :exec
UPDATE pipeline_command_results
SET status = sqlc.arg(status),
    exit_code = sqlc.arg(exit_code),
    finished_at = sqlc.arg(finished_at),
    log = sqlc.arg(log),
    log_truncated = sqlc.arg(log_truncated),
    detail = sqlc.arg(detail)
WHERE id = sqlc.arg(id);

-- name: ListPipelineCommandResults :many
SELECT * FROM pipeline_command_results WHERE attempt_id = ? ORDER BY round, ordinal;

-- name: ListRunningPipelineCommandResults :many
SELECT c.* FROM pipeline_command_results c
JOIN pipeline_stage_attempts a ON a.id = c.attempt_id
WHERE c.status = 'running' AND a.run_id = ?;

-- name: MarkPipelineCommandResultUnknown :exec
UPDATE pipeline_command_results
SET status = 'unknown', finished_at = ?, detail = ?
WHERE id = ? AND status = 'running';

-- name: CreatePipelineRepair :exec
INSERT INTO pipeline_repairs (
    id, run_id, ordinal, source_attempt_id, source_stage_id, kind, target_stage_id, return_stage_id, created_at
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListPipelineRepairs :many
SELECT * FROM pipeline_repairs WHERE run_id = ? ORDER BY ordinal;

-- name: UpsertPipelineReviewLink :exec
INSERT INTO pipeline_review_links (attempt_id, run_id, pr_url, head_sha, review_run_id, linked_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (attempt_id) DO UPDATE SET
    review_run_id = CASE WHEN excluded.review_run_id <> '' THEN excluded.review_run_id ELSE pipeline_review_links.review_run_id END,
    updated_at = excluded.updated_at
WHERE pipeline_review_links.pr_url = excluded.pr_url
  AND pipeline_review_links.head_sha = excluded.head_sha
  AND pipeline_review_links.review_run_id IS NOT excluded.review_run_id
  AND excluded.review_run_id <> '';

-- name: GetPipelineReviewLink :one
SELECT * FROM pipeline_review_links WHERE attempt_id = ?;

-- name: ListPipelineReviewLinks :many
SELECT * FROM pipeline_review_links WHERE run_id = ? ORDER BY linked_at;

-- name: CreatePipelineRepairGrant :exec
INSERT INTO pipeline_repair_grants (id, run_id, amount, authorized_by, request_key, note, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: ListPipelineRepairGrants :many
SELECT * FROM pipeline_repair_grants WHERE run_id = ? ORDER BY created_at, id;
