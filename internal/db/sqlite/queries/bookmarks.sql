-- name: GetBookmark :one
SELECT * FROM bookmarks WHERE id = ?;

-- name: ListBookmarksInRange :many
-- ListBookmarksInRange returns the bookmarks between two frequencies that
-- apply in a region: every hub row, the builtin rows without a region tag
-- (general pack) and the builtin rows tagged with the region (region_tag is
-- the JSON-encoded tag, such as '%"region:r1"%'). Hub rows come before pack
-- rows on the same frequency (BMK-007).
SELECT * FROM bookmarks
WHERE frequency BETWEEN sqlc.arg(from_hz) AND sqlc.arg(to_hz)
  AND (origin <> 'builtin' OR tags NOT LIKE '%"region:%' OR tags LIKE sqlc.arg(region_tag))
ORDER BY frequency, CASE origin WHEN 'builtin' THEN 1 ELSE 0 END, name, id;

-- name: ListBookmarksByOrigin :many
SELECT * FROM bookmarks WHERE origin = ? ORDER BY frequency, name, id;

-- name: InsertBookmark :exec
INSERT INTO bookmarks (
    id, name, frequency, modulation, underlying, description, scannable, tags, origin, locked_fields,
    scope, device_id, preset_id, created_by, created_at, updated_at, version
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: UpdateBookmark :execrows
UPDATE bookmarks SET
    name = sqlc.arg(name), frequency = sqlc.arg(frequency), modulation = sqlc.arg(modulation),
    underlying = sqlc.arg(underlying), description = sqlc.arg(description), scannable = sqlc.arg(scannable),
    tags = sqlc.arg(tags), scope = sqlc.arg(scope), device_id = sqlc.arg(device_id), preset_id = sqlc.arg(preset_id),
    updated_at = sqlc.arg(updated_at), version = sqlc.arg(version)
WHERE id = sqlc.arg(id) AND origin = sqlc.arg(origin) AND version = sqlc.arg(expected_version);

-- name: DeleteBookmark :execrows
DELETE FROM bookmarks WHERE id = ? AND origin = ?;

-- name: BookmarkKeyTaken :one
SELECT EXISTS (
    SELECT 1 FROM bookmarks WHERE name = ? AND frequency = ? AND modulation = ? AND id <> sqlc.arg(except_id)
);
