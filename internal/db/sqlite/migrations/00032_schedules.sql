-- Schedules (TECHNICAL_SPEC §7.1 `schedules`, ADR 0020): (device, preset,
-- time window) rows evaluated by the hub. device_id has no foreign key: a
-- schedule outlives its device (forgotten, or removed with its node) and is
-- disabled with a reason instead (ADM-009, GRID-016). preset_id restricts
-- the deletion of a preset in use (ADM-020).

-- +goose Up
CREATE TABLE schedules (
    id              BLOB    NOT NULL PRIMARY KEY CHECK (length(id) = 16),
    device_id       TEXT    NOT NULL CHECK (length(device_id) BETWEEN 1 AND 64),
    preset_id       BLOB    NOT NULL REFERENCES presets (id) ON DELETE RESTRICT CHECK (length(preset_id) = 16),
    kind            TEXT    NOT NULL CHECK (kind IN ('static', 'daylight')),
    start_minute    INTEGER          CHECK (start_minute IS NULL OR start_minute BETWEEN 0 AND 1439),
    end_minute      INTEGER          CHECK (end_minute IS NULL OR end_minute BETWEEN 0 AND 1439),
    days_of_week    INTEGER NOT NULL DEFAULT 127 CHECK (days_of_week BETWEEN 1 AND 127),
    daylight_phase  TEXT             CHECK (daylight_phase IS NULL OR daylight_phase IN ('day', 'night', 'greyline')),
    priority        INTEGER NOT NULL DEFAULT 0 CHECK (priority BETWEEN -32768 AND 32767),
    enabled         INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    disabled_reason TEXT             CHECK (disabled_reason IS NULL OR disabled_reason IN ('device_stale', 'device_removed', 'preset_incompatible')),
    disabled_at     INTEGER,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    version         INTEGER NOT NULL CHECK (version >= 1),
    CHECK (kind <> 'static' OR (start_minute IS NOT NULL AND end_minute IS NOT NULL)),
    CHECK (kind <> 'daylight' OR daylight_phase IS NOT NULL),
    CHECK (disabled_reason IS NULL OR enabled = 0),
    CHECK ((disabled_reason IS NULL) = (disabled_at IS NULL))
) STRICT;

CREATE INDEX schedules_device_id ON schedules (device_id);
CREATE INDEX schedules_preset_id ON schedules (preset_id);

-- +goose Down
DROP TABLE schedules;
