-- Device registry (TECHNICAL_SPEC §7.1 `devices`), mirrored from node
-- reports. active_preset_id has no foreign key until presets exist
-- (ADR 0008 Q26).

-- +goose Up
CREATE TABLE devices (
    id               TEXT    NOT NULL PRIMARY KEY CHECK (length(id) BETWEEN 1 AND 64),
    node_id          TEXT    NOT NULL REFERENCES nodes (id) ON DELETE CASCADE,
    name             TEXT    NOT NULL CHECK (length(name) <= 128),
    type             TEXT    NOT NULL CHECK (length(type) BETWEEN 1 AND 48),
    freq_min         INTEGER NOT NULL,
    freq_max         INTEGER NOT NULL,
    sample_rates     TEXT    NOT NULL CHECK (json_valid(sample_rates)),
    capabilities     TEXT    NOT NULL CHECK (json_valid(capabilities)),
    online           INTEGER NOT NULL CHECK (online IN (0, 1)),
    runtime_state    TEXT    NOT NULL CHECK (runtime_state IN ('unavailable', 'disabled', 'stopped', 'starting', 'running', 'retuning', 'stopping', 'retry_wait', 'failed')),
    runtime_state_at INTEGER NOT NULL,
    runtime_reason   TEXT             CHECK (runtime_reason IS NULL OR length(runtime_reason) <= 64),
    active_preset_id BLOB             CHECK (active_preset_id IS NULL OR length(active_preset_id) = 16),
    center_freq      INTEGER,
    sort_order       INTEGER NOT NULL CHECK (sort_order BETWEEN 0 AND 2147483647),
    reported_at      INTEGER NOT NULL
) STRICT;

CREATE INDEX devices_node_id ON devices (node_id);
CREATE INDEX devices_node_sort ON devices (node_id, sort_order);

-- +goose Down
DROP TABLE devices;
