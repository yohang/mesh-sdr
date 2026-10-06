-- Files and their content (TECHNICAL_SPEC §7.1 `files`, `file_blobs`, §7.3
-- "File blobs in the DB", ADR 0010). This part stores only the receiver
-- images (kinds receiver_avatar and receiver_photo); the files epic (FIL)
-- adds the other producers. Content is stored in chunks of at most 1 MiB.

-- +goose Up
CREATE TABLE files (
    id                 BLOB    NOT NULL PRIMARY KEY CHECK (length(id) = 16),
    kind               TEXT    NOT NULL CHECK (kind IN ('sstv', 'fax', 'recording', 'speech', 'text_log', 'satellite',
                                                       'receiver_avatar', 'receiver_photo', 'other')),
    name               TEXT    NOT NULL CHECK (length(name) BETWEEN 1 AND 160),
    mime_type          TEXT    NOT NULL CHECK (mime_type IN ('image/png', 'image/jpeg', 'image/webp', 'audio/mpeg',
                                                             'audio/ogg', 'text/plain', 'application/zip')),
    size_bytes         INTEGER NOT NULL CHECK (size_bytes >= 0),
    sha256             BLOB    NOT NULL CHECK (length(sha256) = 32),
    blob_state         TEXT    NOT NULL CHECK (blob_state IN ('receiving', 'complete', 'failed')),
    node_id            TEXT    CHECK (node_id IS NULL OR length(node_id) <= 64),
    device_id          TEXT    CHECK (device_id IS NULL OR length(device_id) <= 64),
    preset_id          BLOB    CHECK (preset_id IS NULL OR length(preset_id) = 16),
    decoder_session_id BLOB    CHECK (decoder_session_id IS NULL OR length(decoder_session_id) = 16),
    mode               TEXT    CHECK (mode IS NULL OR length(mode) <= 24),
    frequency_hz       INTEGER,
    received_start_utc INTEGER,
    received_end_utc   INTEGER,
    visibility         TEXT    NOT NULL CHECK (visibility IN ('public', 'registered', 'admin')),
    width              INTEGER CHECK (width IS NULL OR width BETWEEN 1 AND 2147483647),
    height             INTEGER CHECK (height IS NULL OR height BETWEEN 1 AND 2147483647),
    duration_ms        INTEGER CHECK (duration_ms IS NULL OR duration_ms BETWEEN 0 AND 2147483647),
    metadata           TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(metadata)),
    uploaded_by        BLOB    REFERENCES users (id) ON DELETE SET NULL CHECK (uploaded_by IS NULL OR length(uploaded_by) = 16),
    created_at         INTEGER NOT NULL,
    deleted_at         INTEGER,
    deleted_by         BLOB    REFERENCES users (id) ON DELETE SET NULL CHECK (deleted_by IS NULL OR length(deleted_by) = 16),
    -- Reception metadata rule (FIL-008): decoder and recording files carry
    -- their reception frequency and start, unless imported without them.
    CHECK (kind IN ('receiver_avatar', 'receiver_photo', 'other')
        OR (frequency_hz IS NOT NULL AND received_start_utc IS NOT NULL)
        OR json_extract(metadata, '$.import_incomplete') = 1)
) STRICT;

CREATE INDEX ix_files_kind_received ON files (kind, received_start_utc DESC);
CREATE INDEX ix_files_frequency_hz ON files (frequency_hz);
CREATE INDEX ix_files_received_start_utc ON files (received_start_utc);
CREATE INDEX ix_files_device_id ON files (device_id);
CREATE INDEX ix_files_sha256 ON files (sha256);

CREATE TABLE file_blobs (
    file_id  BLOB    NOT NULL REFERENCES files (id) ON DELETE CASCADE CHECK (length(file_id) = 16),
    chunk_no INTEGER NOT NULL CHECK (chunk_no >= 0),
    data     BLOB    NOT NULL CHECK (length(data) BETWEEN 1 AND 1048576),
    PRIMARY KEY (file_id, chunk_no)
) STRICT;

-- +goose Down
DROP TABLE file_blobs;
DROP TABLE files;
