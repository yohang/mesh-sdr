-- Hub-wide bookmarks (TECHNICAL_SPEC §7.1 `bookmarks`, BMK-002, BMK-008).
-- `builtin` rows are the shipped packs, written only by the pack sync of
-- `meshsdr hub migrate`; a region pack row carries `region:r1|r2|r3` tags
-- (one row per name, frequency and modulation, tagged with every region
-- whose pack holds it). `db` rows are the operators' bookmarks.
--
-- The scope (BMK-008) is all devices, one device or one preset.
-- device_id and preset_id have no foreign key: like schedules.device_id
-- (ADR 0020), a scope may outlive a forgotten device or a deleted preset;
-- such a bookmark matches no device until it is edited.

-- +goose Up
CREATE TABLE bookmarks (
    id            BLOB    NOT NULL PRIMARY KEY CHECK (length(id) = 16),
    name          TEXT    NOT NULL CHECK (length(name) BETWEEN 1 AND 128),
    frequency     INTEGER NOT NULL CHECK (frequency > 0),
    modulation    TEXT    NOT NULL CHECK (length(modulation) BETWEEN 1 AND 24),
    underlying    TEXT             CHECK (underlying IS NULL OR length(underlying) BETWEEN 1 AND 24),
    description   TEXT             CHECK (description IS NULL OR length(description) <= 1024),
    scannable     INTEGER NOT NULL CHECK (scannable IN (0, 1)),
    tags          TEXT    NOT NULL DEFAULT '[]' CHECK (json_valid(tags) AND json_type(tags) = 'array'),
    origin        TEXT    NOT NULL CHECK (origin IN ('config', 'db', 'import', 'builtin')),
    locked_fields TEXT    NOT NULL DEFAULT '[]' CHECK (json_valid(locked_fields) AND json_type(locked_fields) = 'array'),
    scope         TEXT    NOT NULL DEFAULT 'all' CHECK (scope IN ('all', 'device', 'preset')),
    device_id     TEXT             CHECK (device_id IS NULL OR length(device_id) BETWEEN 1 AND 64),
    preset_id     BLOB             CHECK (preset_id IS NULL OR length(preset_id) = 16),
    created_by    BLOB             REFERENCES users (id) ON DELETE SET NULL,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    version       INTEGER NOT NULL CHECK (version >= 1),
    UNIQUE (name, frequency, modulation),
    CHECK (
        (scope = 'all' AND device_id IS NULL AND preset_id IS NULL)
        OR (scope = 'device' AND device_id IS NOT NULL AND preset_id IS NULL)
        OR (scope = 'preset' AND preset_id IS NOT NULL AND device_id IS NULL)
    )
) STRICT;

CREATE INDEX bookmarks_frequency ON bookmarks (frequency);
CREATE INDEX bookmarks_created_by ON bookmarks (created_by);

-- +goose Down
DROP TABLE bookmarks;
