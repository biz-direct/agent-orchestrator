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
    revision = revision + 1
WHERE id = sqlc.arg(id) AND revision = sqlc.arg(expected_revision)
RETURNING *;

-- name: CreatePipelineStageAttempt :one
INSERT INTO pipeline_stage_attempts (
    id, run_id, stage_id, stage_kind, attempt_no, state, executor_session_id,
    controller_generation, input_commit, instruction_delivery, started_at, predecessor_attempt_id
)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)
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
    finished_at = sqlc.arg(finished_at)
WHERE id = sqlc.arg(id) AND state IN ('active', 'handoff')
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
