-- Node capabilities (TECHNICAL_SPEC §7.1 `node_capabilities`, §4.7) and
-- the full report each row set comes from (ADR 0008 Q23).

-- +goose Up
CREATE TABLE node_capabilities (
    node_id    TEXT    NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    capability TEXT    NOT NULL CHECK (length(capability) BETWEEN 3 AND 64),
    available  INTEGER NOT NULL CHECK (available IN (0, 1)),
    status     TEXT    NOT NULL CHECK (status IN ('ok', 'missing', 'too_old', 'untested', 'error')),
    version    TEXT             CHECK (version IS NULL OR length(version) <= 64),
    detail     TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(detail)),
    probed_at  INTEGER NOT NULL,
    probe_ms   INTEGER NOT NULL DEFAULT 0 CHECK (probe_ms BETWEEN 0 AND 2147483647),
    error      TEXT             CHECK (error IS NULL OR length(error) <= 512),
    PRIMARY KEY (node_id, capability)
) STRICT;

CREATE TABLE node_capability_reports (
    node_id           TEXT    NOT NULL PRIMARY KEY REFERENCES nodes (id) ON DELETE CASCADE,
    document          TEXT    NOT NULL CHECK (json_valid(document)),
    capabilities_hash TEXT    NOT NULL CHECK (length(capabilities_hash) <= 64),
    product_version   TEXT    NOT NULL CHECK (length(product_version) <= 32),
    protocols         TEXT    NOT NULL CHECK (json_valid(protocols)),
    platform          TEXT    NOT NULL CHECK (json_valid(platform)),
    reported_at       INTEGER NOT NULL
) STRICT;

-- +goose Down
DROP TABLE node_capability_reports;
DROP TABLE node_capabilities;
