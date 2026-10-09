-- name: GetMapFeature :one
SELECT * FROM map_features WHERE feature_key = ?;

-- name: UpsertMapFeature :exec
-- UpsertMapFeature stores a feature, replacing the one of the same key.
INSERT INTO map_features (feature_key, kind, source, device_id, lat, lon, geometry, details, updated_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (feature_key) DO UPDATE SET
    kind = excluded.kind, source = excluded.source, device_id = excluded.device_id, lat = excluded.lat,
    lon = excluded.lon, geometry = excluded.geometry, details = excluded.details, updated_at = excluded.updated_at,
    expires_at = excluded.expires_at;

-- name: DeleteMapFeature :execrows
DELETE FROM map_features WHERE feature_key = ?;

-- name: ListMapFeatures :many
-- ListMapFeatures returns the features not expired at now_ms of the
-- devices of devices_json (a JSON array), oldest first.
SELECT * FROM map_features
WHERE device_id IN (SELECT value FROM json_each(sqlc.arg(devices_json)))
  AND (expires_at IS NULL OR expires_at > sqlc.arg(now_ms))
ORDER BY updated_at, feature_key;

-- name: ListMapFeaturesOfKind :many
-- ListMapFeaturesOfKind returns the features of a kind, newest first.
SELECT * FROM map_features WHERE kind = ? ORDER BY updated_at DESC, feature_key;

-- name: DeleteExpiredMapFeatures :many
-- DeleteExpiredMapFeatures deletes up to max_rows features expired at
-- now_ms and returns their key and device (one batch of the expiry job).
DELETE FROM map_features WHERE feature_key IN (
    SELECT f.feature_key FROM map_features f
    WHERE f.expires_at IS NOT NULL AND f.expires_at <= sqlc.arg(now_ms)
    ORDER BY f.expires_at LIMIT sqlc.arg(max_rows)
)
RETURNING feature_key, device_id;
