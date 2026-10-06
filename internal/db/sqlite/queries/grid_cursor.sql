-- name: GetEventCursor :one
SELECT last_seq FROM node_event_cursor WHERE node_id = ? AND boot_id = ?;

-- name: UpsertEventCursor :exec
INSERT INTO node_event_cursor (node_id, boot_id, last_seq, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT (node_id, boot_id) DO UPDATE SET last_seq = excluded.last_seq, updated_at = excluded.updated_at;

-- name: DeleteOldEventCursors :exec
DELETE FROM node_event_cursor WHERE node_id = ? AND boot_id <> ?;
