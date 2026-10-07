-- name: GetSchedule :one
SELECT * FROM schedules WHERE id = ?;

-- name: ListSchedules :many
SELECT * FROM schedules ORDER BY device_id, kind, start_minute, daylight_phase, id;

-- name: ListDeviceSchedules :many
SELECT * FROM schedules WHERE device_id = ? ORDER BY kind, start_minute, daylight_phase, id;

-- name: ListPresetSchedules :many
SELECT * FROM schedules WHERE preset_id = ? ORDER BY device_id, kind, start_minute, daylight_phase, id;

-- name: InsertSchedule :exec
INSERT INTO schedules (
    id, device_id, preset_id, kind, start_minute, end_minute, days_of_week, daylight_phase, priority, enabled,
    disabled_reason, disabled_at, created_at, updated_at, version
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: UpdateSchedule :execrows
UPDATE schedules SET
    device_id = sqlc.arg(device_id), preset_id = sqlc.arg(preset_id), kind = sqlc.arg(kind),
    start_minute = sqlc.arg(start_minute), end_minute = sqlc.arg(end_minute), days_of_week = sqlc.arg(days_of_week),
    daylight_phase = sqlc.arg(daylight_phase), priority = sqlc.arg(priority), enabled = sqlc.arg(enabled),
    disabled_reason = sqlc.arg(disabled_reason), disabled_at = sqlc.arg(disabled_at), updated_at = sqlc.arg(updated_at),
    version = sqlc.arg(version)
WHERE id = sqlc.arg(id) AND version = sqlc.arg(expected_version);

-- name: DeleteSchedule :execrows
DELETE FROM schedules WHERE id = ?;
