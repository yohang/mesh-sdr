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

-- Files sent by the nodes (FIL-005), the gallery (FIL-001) and the
-- retention (FIL-004). The produced kinds are those of the FIL-008 rule.

-- name: InsertReceivingFile :exec
INSERT INTO files (id, kind, name, mime_type, size_bytes, sha256, blob_state, node_id, device_id, preset_id,
                   decoder_session_id, mode, frequency_hz, received_start_utc, received_end_utc, visibility, metadata,
                   created_at, received_at)
VALUES (sqlc.arg(id), sqlc.arg(kind), sqlc.arg(name), sqlc.arg(mime_type), sqlc.arg(size_bytes), sqlc.arg(sha256),
        'receiving', sqlc.arg(node_id), sqlc.arg(device_id), sqlc.narg(preset_id), sqlc.narg(decoder_session_id),
        sqlc.arg(mode), sqlc.arg(frequency_hz), sqlc.arg(received_start_utc), sqlc.narg(received_end_utc), 'public',
        sqlc.arg(metadata), sqlc.arg(created_at), sqlc.arg(created_at));

-- name: FileExists :one
SELECT count(*) FROM files WHERE id = sqlc.arg(id);

-- name: ReceivingFile :one
SELECT kind, mime_type, size_bytes, sha256, node_id FROM files WHERE id = sqlc.arg(id) AND blob_state = 'receiving';

-- name: FileReceivedBytes :one
SELECT CAST(coalesce(sum(length(data)), 0) AS INTEGER) FROM file_blobs WHERE file_id = sqlc.arg(file_id);

-- name: NextFileChunk :one
SELECT CAST(coalesce(max(chunk_no) + 1, 0) AS INTEGER) FROM file_blobs WHERE file_id = sqlc.arg(file_id);

-- name: FileChunkNumbers :many
SELECT chunk_no FROM file_blobs WHERE file_id = sqlc.arg(file_id) ORDER BY chunk_no;

-- name: FileChunk :one
SELECT data FROM file_blobs WHERE file_id = sqlc.arg(file_id) AND chunk_no = sqlc.arg(chunk_no);

-- name: TouchReceivingFile :exec
UPDATE files SET received_at = sqlc.arg(received_at) WHERE id = sqlc.arg(id);

-- name: ReceivingBytesOfNode :one
SELECT CAST(coalesce(sum(size_bytes), 0) AS INTEGER) FROM files WHERE node_id = sqlc.arg(node_id) AND blob_state = 'receiving';

-- name: DeleteFileChunks :exec
DELETE FROM file_blobs WHERE file_id = sqlc.arg(file_id);

-- name: CompleteFile :execrows
UPDATE files SET blob_state = 'complete', size_bytes = sqlc.arg(size_bytes), sha256 = sqlc.arg(sha256),
                 width = sqlc.narg(width), height = sqlc.narg(height), thumbnail = sqlc.narg(thumbnail)
WHERE id = sqlc.arg(id) AND blob_state = 'receiving';

-- name: DeleteFile :execrows
DELETE FROM files WHERE id = sqlc.arg(id);

-- name: ProducedFile :one
SELECT id, kind, name, mime_type, size_bytes, sha256, node_id, device_id, preset_id, decoder_session_id, mode,
       frequency_hz, received_start_utc, received_end_utc, width, height, metadata,
       CAST(thumbnail IS NOT NULL AS INTEGER) AS has_thumbnail, created_at
FROM files
WHERE id = sqlc.arg(id) AND blob_state = 'complete' AND deleted_at IS NULL
  AND kind IN ('sstv', 'fax', 'recording', 'speech', 'text_log', 'satellite');

-- name: FileThumbnail :one
SELECT thumbnail FROM files WHERE id = sqlc.arg(id) AND blob_state = 'complete' AND thumbnail IS NOT NULL;

-- name: ListProducedFiles :many
SELECT id, kind, name, mime_type, size_bytes, sha256, node_id, device_id, preset_id, decoder_session_id, mode,
       frequency_hz, received_start_utc, received_end_utc, width, height, metadata,
       CAST(thumbnail IS NOT NULL AS INTEGER) AS has_thumbnail, created_at
FROM files
WHERE blob_state = 'complete' AND deleted_at IS NULL
  AND kind IN ('sstv', 'fax', 'recording', 'speech', 'text_log', 'satellite')
  AND (sqlc.narg(mime_like) IS NULL OR mime_type LIKE sqlc.narg(mime_like))
  AND (sqlc.narg(device_id) IS NULL OR device_id = sqlc.narg(device_id))
  AND (sqlc.narg(mode) IS NULL OR mode = sqlc.narg(mode))
  AND (sqlc.narg(from_utc) IS NULL OR received_start_utc >= sqlc.narg(from_utc))
  AND (sqlc.narg(to_utc) IS NULL OR received_start_utc < sqlc.narg(to_utc))
  AND (sqlc.narg(freq_min) IS NULL OR frequency_hz >= sqlc.narg(freq_min))
  AND (sqlc.narg(freq_max) IS NULL OR frequency_hz <= sqlc.narg(freq_max))
  AND (device_id IS NULL OR device_id NOT IN (SELECT value FROM json_each(sqlc.arg(hidden_devices))))
ORDER BY received_start_utc DESC, id DESC
LIMIT sqlc.arg(limit_rows) OFFSET sqlc.arg(offset_rows);

-- name: DeleteProducedFiles :execrows
DELETE FROM files
WHERE kind IN ('sstv', 'fax', 'recording', 'speech', 'text_log', 'satellite')
  AND blob_state = 'complete'
  AND (sqlc.narg(mime_like) IS NULL OR mime_type LIKE sqlc.narg(mime_like))
  AND (sqlc.narg(device_id) IS NULL OR device_id = sqlc.narg(device_id))
  AND (sqlc.narg(mode) IS NULL OR mode = sqlc.narg(mode))
  AND (sqlc.narg(from_utc) IS NULL OR received_start_utc >= sqlc.narg(from_utc))
  AND (sqlc.narg(to_utc) IS NULL OR received_start_utc < sqlc.narg(to_utc))
  AND (sqlc.narg(freq_min) IS NULL OR frequency_hz >= sqlc.narg(freq_min))
  AND (sqlc.narg(freq_max) IS NULL OR frequency_hz <= sqlc.narg(freq_max));

-- name: ProducedFileDevices :many
SELECT DISTINCT device_id FROM files
WHERE blob_state = 'complete' AND device_id IS NOT NULL AND kind IN ('sstv', 'fax', 'recording', 'speech', 'text_log', 'satellite')
ORDER BY device_id;

-- name: ProducedFileModes :many
SELECT DISTINCT mode FROM files
WHERE blob_state = 'complete' AND mode IS NOT NULL AND kind IN ('sstv', 'fax', 'recording', 'speech', 'text_log', 'satellite')
ORDER BY mode;

-- name: PurgeIncompleteFiles :execrows
DELETE FROM files WHERE blob_state <> 'complete' AND coalesce(received_at, created_at) < sqlc.arg(before);

-- name: DeleteProducedFilesBefore :execrows
DELETE FROM files
WHERE kind IN ('sstv', 'fax', 'recording', 'speech', 'text_log', 'satellite') AND created_at < sqlc.arg(before);

-- name: DeleteFilesBeyondCount :execrows
DELETE FROM files WHERE id IN (
    SELECT f.id FROM files AS f WHERE f.kind = sqlc.arg(kind) AND f.blob_state = 'complete'
    ORDER BY f.created_at DESC, f.id DESC LIMIT -1 OFFSET sqlc.arg(keep));

-- name: ProducedFilesStats :one
SELECT count(*) AS files, CAST(coalesce(sum(size_bytes), 0) AS INTEGER) AS bytes
FROM files WHERE kind IN ('sstv', 'fax', 'recording', 'speech', 'text_log', 'satellite') AND blob_state = 'complete';

-- name: OldestProducedFiles :many
SELECT id, size_bytes FROM files
WHERE kind IN ('sstv', 'fax', 'recording', 'speech', 'text_log', 'satellite') AND blob_state = 'complete'
ORDER BY created_at, id LIMIT sqlc.arg(limit_rows);
