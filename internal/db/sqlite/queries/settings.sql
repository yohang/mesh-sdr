-- name: ListSettings :many
SELECT key, value, updated_by, updated_at, version FROM settings WHERE value IS NOT NULL ORDER BY key;

-- name: GetSetting :one
SELECT key, value, updated_by, updated_at, version FROM settings WHERE key = sqlc.arg(key) AND value IS NOT NULL;

-- name: UpsertSetting :exec
INSERT INTO settings (key, value, value_enc, schema_version, updated_by, updated_at, version)
VALUES (sqlc.arg(key), sqlc.arg(value), NULL, sqlc.arg(schema_version), sqlc.narg(updated_by), sqlc.arg(updated_at), sqlc.arg(version))
ON CONFLICT (key) DO UPDATE SET
    value = excluded.value, value_enc = NULL, schema_version = excluded.schema_version,
    updated_by = excluded.updated_by, updated_at = excluded.updated_at, version = excluded.version;

-- name: DeleteSetting :exec
DELETE FROM settings WHERE key = sqlc.arg(key);

-- name: NextSettingsRevision :one
UPDATE settings_revision SET revision = revision + 1 WHERE id = 1 RETURNING revision;

-- name: SettingsRevision :one
SELECT revision FROM settings_revision WHERE id = 1;
