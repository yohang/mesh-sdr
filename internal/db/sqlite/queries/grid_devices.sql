-- name: GetDevice :one
SELECT * FROM devices WHERE id = ?;

-- name: ListDevices :many
SELECT * FROM devices ORDER BY node_id, sort_order, id;

-- name: ListNodeDevices :many
SELECT * FROM devices WHERE node_id = ? ORDER BY sort_order, id;

-- name: UpsertDevice :exec
-- A preset id the hub does not know is stored as NULL (ADR 0020).
INSERT INTO devices (
    id, node_id, name, type, freq_min, freq_max, sample_rates, capabilities, online, runtime_state,
    runtime_state_at, runtime_reason, active_preset_id, center_freq, sort_order, reported_at
) VALUES (
    sqlc.arg(id),
    sqlc.arg(node_id),
    sqlc.arg(name),
    sqlc.arg(type),
    sqlc.arg(freq_min),
    sqlc.arg(freq_max),
    sqlc.arg(sample_rates),
    sqlc.arg(capabilities),
    sqlc.arg(online),
    sqlc.arg(runtime_state),
    sqlc.arg(runtime_state_at),
    sqlc.arg(runtime_reason),
    (SELECT p.id FROM presets p WHERE p.id = sqlc.narg(active_preset_id)),
    sqlc.arg(center_freq),
    sqlc.arg(sort_order),
    sqlc.arg(reported_at)
)
ON CONFLICT (id) DO UPDATE SET
    name = excluded.name,
    type = excluded.type,
    freq_min = excluded.freq_min,
    freq_max = excluded.freq_max,
    sample_rates = excluded.sample_rates,
    capabilities = excluded.capabilities,
    online = excluded.online,
    runtime_state = excluded.runtime_state,
    runtime_state_at = excluded.runtime_state_at,
    runtime_reason = excluded.runtime_reason,
    active_preset_id = excluded.active_preset_id,
    center_freq = excluded.center_freq,
    sort_order = excluded.sort_order,
    reported_at = excluded.reported_at
WHERE devices.node_id = excluded.node_id;

-- name: SetNodeDevicesOffline :exec
UPDATE devices SET online = 0 WHERE node_id = ? AND online = 1;

-- name: DeleteMissingDevice :execrows
DELETE FROM devices WHERE id = ? AND runtime_state = 'unavailable' AND runtime_reason = 'not_reported';
