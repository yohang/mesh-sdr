-- name: InsertDecodedMessage :one
-- InsertDecodedMessage stores a decoded message unless its dedup_key is
-- already stored (no row then).
INSERT INTO decoded_messages (
    decoded_at, received_at, node_id, device_id, preset_id, decoder_session_id, origin, mode, family,
    frequency, text, payload, payload_schema, dedup_key
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (dedup_key) DO NOTHING
RETURNING id;

-- name: ListDecodedMessages :many
-- ListDecodedMessages returns the messages of the devices of devices_json
-- (a JSON array: sqlc.slice breaks the numbered arguments), newest first,
-- older than before_id, with optional mode and device filters and a decode
-- time range [from_ms, to_ms).
SELECT * FROM decoded_messages
WHERE device_id IN (SELECT value FROM json_each(sqlc.arg(devices_json)))
  AND (sqlc.arg(mode) = '' OR mode = sqlc.arg(mode))
  AND (sqlc.arg(device) = '' OR device_id = sqlc.arg(device))
  AND decoded_at >= sqlc.arg(from_ms) AND decoded_at < sqlc.arg(to_ms)
  AND id < sqlc.arg(before_id)
ORDER BY id DESC
LIMIT sqlc.arg(max_rows);

-- name: DeleteDecodedBefore :execrows
-- DeleteDecodedBefore deletes up to max_rows messages decoded before
-- before_ms (one batch of the retention job).
DELETE FROM decoded_messages WHERE id IN (
    SELECT m.id FROM decoded_messages m WHERE m.decoded_at < sqlc.arg(before_ms) LIMIT sqlc.arg(max_rows)
);

-- name: DeleteDecodedBeyond :execrows
-- DeleteDecodedBeyond deletes up to max_rows of the oldest messages beyond
-- the newest keep ones (one batch of the row cap).
DELETE FROM decoded_messages WHERE id IN (
    SELECT m.id FROM decoded_messages m
    WHERE m.id <= (SELECT d.id FROM decoded_messages d ORDER BY d.id DESC LIMIT 1 OFFSET sqlc.arg(keep))
    ORDER BY m.id LIMIT sqlc.arg(max_rows)
);
