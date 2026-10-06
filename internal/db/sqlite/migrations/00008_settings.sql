-- Settings store (TECHNICAL_SPEC §7.1 `settings`, ADR 0010): admin-set DB
-- settings, the "DB" layer of config (locked) > DB > default. Generic types
-- per §7.2: UUID = BLOB(16), TIMESTAMP = INTEGER Unix epoch milliseconds,
-- JSON = TEXT with json_valid. A reset deletes the row.
--
-- version is the settings revision of the last write, taken from the
-- one-row settings_revision counter, so a version is never reused after a
-- reset (optimistic concurrency without ABA).

-- +goose Up
CREATE TABLE settings (
    key            TEXT    NOT NULL PRIMARY KEY CHECK (length(key) BETWEEN 1 AND 160),
    value          TEXT    CHECK (value IS NULL OR json_valid(value)),
    value_enc      BLOB,
    schema_version INTEGER NOT NULL CHECK (schema_version BETWEEN 1 AND 32767),
    updated_by     BLOB    REFERENCES users (id) ON DELETE SET NULL CHECK (updated_by IS NULL OR length(updated_by) = 16),
    updated_at     INTEGER NOT NULL,
    version        INTEGER NOT NULL CHECK (version >= 1),
    CHECK ((value IS NULL) <> (value_enc IS NULL))
) STRICT;

CREATE TABLE settings_revision (
    id       INTEGER NOT NULL PRIMARY KEY CHECK (id = 1),
    revision INTEGER NOT NULL CHECK (revision >= 0)
) STRICT;

INSERT INTO settings_revision (id, revision) VALUES (1, 0);

-- +goose Down
DROP TABLE settings_revision;
DROP TABLE settings;
