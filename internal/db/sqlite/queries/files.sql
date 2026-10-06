-- name: InsertFile :exec
INSERT INTO files (id, kind, name, mime_type, size_bytes, sha256, blob_state, visibility, width, height, uploaded_by, created_at)
VALUES (sqlc.arg(id), sqlc.arg(kind), sqlc.arg(name), sqlc.arg(mime_type), sqlc.arg(size_bytes), sqlc.arg(sha256),
        sqlc.arg(blob_state), sqlc.arg(visibility), sqlc.narg(width), sqlc.narg(height), sqlc.narg(uploaded_by), sqlc.arg(created_at));

-- name: InsertFileChunk :exec
INSERT INTO file_blobs (file_id, chunk_no, data) VALUES (sqlc.arg(file_id), sqlc.arg(chunk_no), sqlc.arg(data));

-- name: LatestFileOfKind :one
SELECT id, kind, name, mime_type, size_bytes, sha256, blob_state, visibility, width, height, uploaded_by, created_at
FROM files WHERE kind = sqlc.arg(kind) AND deleted_at IS NULL AND blob_state = 'complete'
ORDER BY created_at DESC, id DESC LIMIT 1;

-- name: FileChunks :many
SELECT data FROM file_blobs WHERE file_id = sqlc.arg(file_id) ORDER BY chunk_no;

-- name: DeleteFilesOfKind :execrows
DELETE FROM files WHERE kind = sqlc.arg(kind);
