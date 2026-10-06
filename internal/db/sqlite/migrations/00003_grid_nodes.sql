-- Grid nodes (TECHNICAL_SPEC §7.1 `nodes`, ADR 0008), the hub certificate
-- revocation list and the per-boot event cursor of the control channel.

-- +goose Up
CREATE TABLE nodes (
    id                          TEXT    NOT NULL PRIMARY KEY CHECK (length(id) BETWEEN 2 AND 64),
    name                        TEXT    NOT NULL CHECK (length(name) BETWEEN 1 AND 128),
    url                         TEXT    NOT NULL CHECK (length(url) <= 512),
    enrollment_state            TEXT    NOT NULL CHECK (enrollment_state IN ('pending', 'enrolled', 'revoked')),
    enrollment_token_digest     BLOB             CHECK (enrollment_token_digest IS NULL OR length(enrollment_token_digest) = 32),
    enrollment_token_expires_at INTEGER,
    enrolled_at                 INTEGER,
    cert_fingerprint            BLOB             CHECK (cert_fingerprint IS NULL OR length(cert_fingerprint) = 32),
    cert_serial                 TEXT             CHECK (cert_serial IS NULL OR length(cert_serial) <= 64),
    cert_not_after              INTEGER,
    status                      TEXT    NOT NULL CHECK (status IN ('online', 'degraded', 'offline', 'unreachable', 'incompatible')),
    status_hint                 TEXT             CHECK (status_hint IS NULL OR length(status_hint) <= 64),
    last_heartbeat_at           INTEGER,
    boot_id                     BLOB             CHECK (boot_id IS NULL OR length(boot_id) = 16),
    software_version            TEXT             CHECK (software_version IS NULL OR length(software_version) <= 32),
    protocol_version            TEXT             CHECK (protocol_version IS NULL OR length(protocol_version) <= 32),
    hostname                    TEXT             CHECK (hostname IS NULL OR length(hostname) <= 255),
    cpu_cores                   INTEGER          CHECK (cpu_cores IS NULL OR cpu_cores BETWEEN 0 AND 32767),
    clock_offset_ms             INTEGER          CHECK (clock_offset_ms IS NULL OR clock_offset_ms BETWEEN -2147483648 AND 2147483647),
    labels                      TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(labels)),
    origin                      TEXT    NOT NULL CHECK (origin IN ('config', 'db')),
    locked_fields               TEXT    NOT NULL DEFAULT '[]' CHECK (json_valid(locked_fields)),
    disabled                    INTEGER NOT NULL DEFAULT 0 CHECK (disabled IN (0, 1)),
    created_at                  INTEGER NOT NULL,
    updated_at                  INTEGER NOT NULL,
    version                     INTEGER NOT NULL DEFAULT 1 CHECK (version BETWEEN 1 AND 2147483647)
) STRICT;

CREATE INDEX nodes_status ON nodes (status);

CREATE TABLE revoked_certificates (
    serial     TEXT    NOT NULL PRIMARY KEY CHECK (length(serial) <= 64),
    node_id    TEXT    NOT NULL CHECK (length(node_id) <= 64),
    not_after  INTEGER NOT NULL,
    revoked_at INTEGER NOT NULL,
    reason     TEXT    NOT NULL CHECK (length(reason) <= 32)
) STRICT;

CREATE TABLE node_event_cursor (
    node_id    TEXT    NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    boot_id    BLOB    NOT NULL CHECK (length(boot_id) = 16),
    last_seq   INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (node_id, boot_id)
) STRICT;

-- +goose Down
DROP TABLE node_event_cursor;
DROP TABLE revoked_certificates;
DROP TABLE nodes;
