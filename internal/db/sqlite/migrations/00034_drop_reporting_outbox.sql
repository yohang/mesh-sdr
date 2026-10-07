-- The reporting engine is removed (ADR 0022): drop its outbox. RPT-001
-- brings the table back with the first transport.

-- +goose Up
DROP TABLE IF EXISTS reporting_outbox;

-- +goose Down
CREATE TABLE reporting_outbox (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    network            TEXT    NOT NULL CHECK (network IN ('pskreporter', 'wsprnet', 'aprs_is', 'sondehub', 'sondehub_listener', 'ais_udp', 'mqtt')),
    decoded_message_id INTEGER,
    payload            TEXT    NOT NULL CHECK (json_valid(payload)),
    dedup_key          BLOB    NOT NULL CHECK (length(dedup_key) = 16),
    status             TEXT    NOT NULL CHECK (status IN ('pending', 'in_flight', 'sent', 'failed', 'dead')),
    attempts           INTEGER NOT NULL DEFAULT 0 CHECK (attempts BETWEEN 0 AND 32767),
    next_attempt_at    INTEGER NOT NULL,
    lease_owner        TEXT             CHECK (lease_owner IS NULL OR length(lease_owner) <= 64),
    lease_until        INTEGER,
    batch_id           BLOB             CHECK (batch_id IS NULL OR length(batch_id) = 16),
    created_at         INTEGER NOT NULL,
    sent_at            INTEGER,
    last_error         TEXT             CHECK (last_error IS NULL OR length(last_error) <= 512),
    UNIQUE (network, dedup_key)
) STRICT;

CREATE INDEX reporting_outbox_claim ON reporting_outbox (network, status, next_attempt_at);
CREATE INDEX reporting_outbox_status_created ON reporting_outbox (status, created_at);
CREATE INDEX reporting_outbox_network_status_id ON reporting_outbox (network, status, id);
