-- Presets (TECHNICAL_SPEC §7.1 `presets`, ADR 0020): device-independent
-- tuning data. Compatibility with a device is checked when a preset is
-- applied, never stored.

-- +goose Up
CREATE TABLE presets (
    id                    BLOB    NOT NULL PRIMARY KEY CHECK (length(id) = 16),
    slug                  TEXT    NOT NULL UNIQUE CHECK (length(slug) BETWEEN 1 AND 64),
    name                  TEXT    NOT NULL CHECK (length(name) BETWEEN 1 AND 128),
    description           TEXT             CHECK (description IS NULL OR length(description) <= 1024),
    tags                  TEXT    NOT NULL DEFAULT '[]' CHECK (json_valid(tags) AND json_type(tags) = 'array'),
    center_freq           INTEGER NOT NULL CHECK (center_freq > 0),
    samp_rate             INTEGER NOT NULL CHECK (samp_rate > 0 AND samp_rate <= 2147483647),
    start_freq            INTEGER NOT NULL CHECK (start_freq > 0),
    start_mod             TEXT    NOT NULL CHECK (length(start_mod) BETWEEN 1 AND 24),
    tuning_step           INTEGER NOT NULL CHECK (tuning_step > 0 AND tuning_step <= 2147483647),
    initial_squelch_level INTEGER          CHECK (initial_squelch_level IS NULL OR initial_squelch_level BETWEEN -150 AND 0),
    initial_nr_level      INTEGER          CHECK (initial_nr_level IS NULL OR initial_nr_level BETWEEN -20 AND 20),
    waterfall_levels      TEXT             CHECK (waterfall_levels IS NULL OR json_valid(waterfall_levels)),
    sort_order            INTEGER NOT NULL CHECK (sort_order BETWEEN 0 AND 2147483647),
    created_at            INTEGER NOT NULL,
    updated_at            INTEGER NOT NULL,
    version               INTEGER NOT NULL CHECK (version >= 1)
) STRICT;

CREATE INDEX presets_sort_order ON presets (sort_order);

-- +goose Down
DROP TABLE presets;
