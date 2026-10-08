-- Node software versions up to 64 characters: a development build stamps a
-- pseudo-version such as `0.0.0-20261008191410-309bcfdfb3d7+dirty`, longer
-- than the former 32. nodes.software_version and the capability report's
-- product_version (the same version) get the new limit; SQLite needs a
-- table rebuild to change a CHECK constraint.
--
-- DROP TABLE nodes with foreign keys on would cascade to node_event_cursor,
-- node_capabilities, node_capability_reports, devices and through devices
-- to bookmarks, so this migration runs outside goose's transaction to turn
-- foreign keys off (the pragma is a no-op inside a transaction) and wraps
-- the rebuild in its own transaction (SQLite's generalized ALTER TABLE
-- procedure). The rebuild copies every id unchanged, so no reference
-- breaks. Goose runs it on one connection, the single writer.

-- +goose NO TRANSACTION
-- +goose Up
PRAGMA foreign_keys = OFF;

BEGIN;

CREATE TABLE nodes_new (
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
    software_version            TEXT             CHECK (software_version IS NULL OR length(software_version) <= 64),
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
    version                     INTEGER NOT NULL DEFAULT 1 CHECK (version BETWEEN 1 AND 2147483647),
    cert_pending_fingerprint    BLOB             CHECK (cert_pending_fingerprint IS NULL OR length(cert_pending_fingerprint) = 32),
    cert_pending_serial         TEXT             CHECK (cert_pending_serial IS NULL OR length(cert_pending_serial) <= 64),
    cert_pending_not_after      INTEGER
) STRICT;

INSERT INTO nodes_new (id, name, url, enrollment_state, enrollment_token_digest, enrollment_token_expires_at, enrolled_at,
                       cert_fingerprint, cert_serial, cert_not_after, status, status_hint, last_heartbeat_at, boot_id,
                       software_version, protocol_version, hostname, cpu_cores, clock_offset_ms, labels, origin,
                       locked_fields, disabled, created_at, updated_at, version, cert_pending_fingerprint,
                       cert_pending_serial, cert_pending_not_after)
SELECT id, name, url, enrollment_state, enrollment_token_digest, enrollment_token_expires_at, enrolled_at,
       cert_fingerprint, cert_serial, cert_not_after, status, status_hint, last_heartbeat_at, boot_id,
       software_version, protocol_version, hostname, cpu_cores, clock_offset_ms, labels, origin,
       locked_fields, disabled, created_at, updated_at, version, cert_pending_fingerprint,
       cert_pending_serial, cert_pending_not_after
FROM nodes;

DROP TABLE nodes;
ALTER TABLE nodes_new RENAME TO nodes;

CREATE INDEX nodes_status ON nodes (status);

CREATE TABLE node_capability_reports_new (
    node_id           TEXT    NOT NULL PRIMARY KEY REFERENCES nodes (id) ON DELETE CASCADE,
    document          TEXT    NOT NULL CHECK (json_valid(document)),
    capabilities_hash TEXT    NOT NULL CHECK (length(capabilities_hash) <= 64),
    product_version   TEXT    NOT NULL CHECK (length(product_version) <= 64),
    protocols         TEXT    NOT NULL CHECK (json_valid(protocols)),
    platform          TEXT    NOT NULL CHECK (json_valid(platform)),
    reported_at       INTEGER NOT NULL
) STRICT;

INSERT INTO node_capability_reports_new (node_id, document, capabilities_hash, product_version, protocols, platform, reported_at)
SELECT node_id, document, capabilities_hash, product_version, protocols, platform, reported_at
FROM node_capability_reports;

DROP TABLE node_capability_reports;
ALTER TABLE node_capability_reports_new RENAME TO node_capability_reports;

COMMIT;

PRAGMA foreign_keys = ON;

-- +goose Down
PRAGMA foreign_keys = OFF;

BEGIN;

CREATE TABLE nodes_old (
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
    version                     INTEGER NOT NULL DEFAULT 1 CHECK (version BETWEEN 1 AND 2147483647),
    cert_pending_fingerprint    BLOB             CHECK (cert_pending_fingerprint IS NULL OR length(cert_pending_fingerprint) = 32),
    cert_pending_serial         TEXT             CHECK (cert_pending_serial IS NULL OR length(cert_pending_serial) <= 64),
    cert_pending_not_after      INTEGER
) STRICT;

INSERT INTO nodes_old (id, name, url, enrollment_state, enrollment_token_digest, enrollment_token_expires_at, enrolled_at,
                       cert_fingerprint, cert_serial, cert_not_after, status, status_hint, last_heartbeat_at, boot_id,
                       software_version, protocol_version, hostname, cpu_cores, clock_offset_ms, labels, origin,
                       locked_fields, disabled, created_at, updated_at, version, cert_pending_fingerprint,
                       cert_pending_serial, cert_pending_not_after)
SELECT id, name, url, enrollment_state, enrollment_token_digest, enrollment_token_expires_at, enrolled_at,
       cert_fingerprint, cert_serial, cert_not_after, status, status_hint, last_heartbeat_at, boot_id,
       substr(software_version, 1, 32), protocol_version, hostname, cpu_cores, clock_offset_ms, labels, origin,
       locked_fields, disabled, created_at, updated_at, version, cert_pending_fingerprint,
       cert_pending_serial, cert_pending_not_after
FROM nodes;

DROP TABLE nodes;
ALTER TABLE nodes_old RENAME TO nodes;

CREATE INDEX nodes_status ON nodes (status);

CREATE TABLE node_capability_reports_old (
    node_id           TEXT    NOT NULL PRIMARY KEY REFERENCES nodes (id) ON DELETE CASCADE,
    document          TEXT    NOT NULL CHECK (json_valid(document)),
    capabilities_hash TEXT    NOT NULL CHECK (length(capabilities_hash) <= 64),
    product_version   TEXT    NOT NULL CHECK (length(product_version) <= 32),
    protocols         TEXT    NOT NULL CHECK (json_valid(protocols)),
    platform          TEXT    NOT NULL CHECK (json_valid(platform)),
    reported_at       INTEGER NOT NULL
) STRICT;

INSERT INTO node_capability_reports_old (node_id, document, capabilities_hash, product_version, protocols, platform, reported_at)
SELECT node_id, document, capabilities_hash, substr(product_version, 1, 32), protocols, platform, reported_at
FROM node_capability_reports;

DROP TABLE node_capability_reports;
ALTER TABLE node_capability_reports_old RENAME TO node_capability_reports;

COMMIT;

PRAGMA foreign_keys = ON;
