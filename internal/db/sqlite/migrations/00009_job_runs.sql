-- Periodic hub jobs (TECHNICAL_SPEC §7.1 auxiliary `job_runs`, §7.3
-- "Retention jobs", ADR 0010): the last run of each job. running_since is
-- set while a run is in progress, so a job never overlaps itself.

-- +goose Up
CREATE TABLE job_runs (
    job              TEXT    NOT NULL PRIMARY KEY CHECK (length(job) BETWEEN 1 AND 64),
    running_since    INTEGER,
    last_started_at  INTEGER,
    last_finished_at INTEGER,
    last_status      TEXT    CHECK (last_status IS NULL OR last_status IN ('ok', 'error')),
    last_error       TEXT    CHECK (last_error IS NULL OR length(last_error) <= 512),
    rows_affected    INTEGER NOT NULL DEFAULT 0 CHECK (rows_affected >= 0)
) STRICT;

-- +goose Down
DROP TABLE job_runs;
