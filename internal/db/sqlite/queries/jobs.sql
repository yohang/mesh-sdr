-- name: GetJobRun :one
SELECT * FROM job_runs WHERE job = sqlc.arg(job);

-- name: UpsertJobRun :exec
INSERT INTO job_runs (job, running_since, last_started_at, last_finished_at, last_status, last_error, rows_affected)
VALUES (sqlc.arg(job), sqlc.narg(running_since), sqlc.narg(last_started_at), sqlc.narg(last_finished_at),
        sqlc.narg(last_status), sqlc.narg(last_error), sqlc.arg(rows_affected))
ON CONFLICT (job) DO UPDATE SET
    running_since = excluded.running_since, last_started_at = excluded.last_started_at,
    last_finished_at = excluded.last_finished_at, last_status = excluded.last_status,
    last_error = excluded.last_error, rows_affected = excluded.rows_affected;
