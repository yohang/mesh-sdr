-- Heartbeat-based presence registry (TECHNICAL_SPEC §7.1 `connections`,
-- §7.3). user_id has no foreign key until users exist (ADR 0008 Q26).

-- +goose Up
CREATE TABLE connections (
    id                BLOB    NOT NULL PRIMARY KEY CHECK (length(id) = 16),
    kind              TEXT    NOT NULL CHECK (kind IN ('events', 'media', 'map')),
    user_id           BLOB             CHECK (user_id IS NULL OR length(user_id) = 16),
    session_id        BLOB             CHECK (session_id IS NULL OR length(session_id) = 16),
    role_id           INTEGER NOT NULL CHECK (role_id BETWEEN 0 AND 32767),
    ip                TEXT    NOT NULL CHECK (length(ip) <= 45),
    user_agent        TEXT             CHECK (user_agent IS NULL OR length(user_agent) <= 512),
    node_id           TEXT             CHECK (node_id IS NULL OR length(node_id) <= 64),
    device_id         TEXT             CHECK (device_id IS NULL OR length(device_id) <= 64),
    preset_id         BLOB             CHECK (preset_id IS NULL OR length(preset_id) = 16),
    tuned_freq        INTEGER,
    mode              TEXT             CHECK (mode IS NULL OR length(mode) <= 24),
    secondary_mode    TEXT             CHECK (secondary_mode IS NULL OR length(secondary_mode) <= 24),
    opened_at         INTEGER NOT NULL,
    last_heartbeat_at INTEGER NOT NULL,
    closed_at         INTEGER,
    close_reason      TEXT             CHECK (close_reason IS NULL OR length(close_reason) <= 32),
    bytes_out         INTEGER NOT NULL DEFAULT 0,
    bytes_in          INTEGER NOT NULL DEFAULT 0
) STRICT;

CREATE INDEX connections_open_heartbeat ON connections (last_heartbeat_at) WHERE closed_at IS NULL;
CREATE INDEX connections_user_id ON connections (user_id);
CREATE INDEX connections_device_id ON connections (device_id);
CREATE INDEX connections_opened_at ON connections (opened_at);

-- +goose Down
DROP TABLE connections;
