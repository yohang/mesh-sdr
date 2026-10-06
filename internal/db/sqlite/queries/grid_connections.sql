-- name: InsertConnection :execrows
INSERT INTO connections (
    id, kind, user_id, session_id, role_id, ip, user_agent, node_id, device_id, preset_id, tuned_freq,
    mode, secondary_mode, opened_at, last_heartbeat_at, closed_at, close_reason, bytes_out, bytes_in
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO NOTHING;

-- name: GetConnection :one
SELECT * FROM connections WHERE id = ?;

-- name: UpdateConnection :exec
UPDATE connections SET
    device_id = sqlc.arg(device_id),
    preset_id = sqlc.arg(preset_id),
    tuned_freq = sqlc.arg(tuned_freq),
    mode = sqlc.arg(mode),
    secondary_mode = sqlc.arg(secondary_mode),
    last_heartbeat_at = sqlc.arg(last_heartbeat_at),
    closed_at = sqlc.arg(closed_at),
    close_reason = sqlc.arg(close_reason),
    bytes_out = sqlc.arg(bytes_out),
    bytes_in = sqlc.arg(bytes_in)
WHERE id = sqlc.arg(id);

-- name: HeartbeatConnections :execrows
UPDATE connections SET last_heartbeat_at = sqlc.arg(now)
WHERE closed_at IS NULL AND id IN (sqlc.slice('ids'));

-- name: CloseStaleConnections :execrows
UPDATE connections SET closed_at = last_heartbeat_at, close_reason = 'heartbeat_timeout'
WHERE closed_at IS NULL AND last_heartbeat_at < ?;

-- name: CloseAllConnections :execrows
UPDATE connections SET closed_at = sqlc.arg(now), close_reason = sqlc.arg(reason)
WHERE closed_at IS NULL;

-- name: CloseNodeConnections :execrows
UPDATE connections SET closed_at = sqlc.arg(now), close_reason = sqlc.arg(reason)
WHERE closed_at IS NULL AND node_id = sqlc.arg(node_id);

-- name: ListOpenConnections :many
SELECT * FROM connections WHERE closed_at IS NULL ORDER BY opened_at, id;

-- name: CountOpenConnections :one
SELECT count(*) FROM connections WHERE closed_at IS NULL;

-- name: OpenMediaNodes :many
SELECT DISTINCT node_id FROM connections WHERE closed_at IS NULL AND node_id IS NOT NULL;

-- name: DeleteClosedConnections :execrows
DELETE FROM connections WHERE closed_at IS NOT NULL AND closed_at < ?;

-- name: CountOpenNodeConnections :one
SELECT count(*) FROM connections WHERE closed_at IS NULL AND node_id = ?;

-- name: DeleteClosedUserConnections :execrows
DELETE FROM connections WHERE user_id = sqlc.arg(user_id) AND closed_at IS NOT NULL;

-- name: AnonymizeOpenUserConnections :execrows
UPDATE connections SET user_id = NULL, session_id = NULL, ip = '', user_agent = NULL
WHERE user_id = sqlc.arg(user_id) AND closed_at IS NULL;
