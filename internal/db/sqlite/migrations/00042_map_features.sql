-- Map features (TECHNICAL_SPEC §7.1 `map_features`, MAP-002, MAP-012, ADR
-- 0029 "M3a scope"): the hub projects them from the decoded messages at
-- ingest, in the ingest transaction. Times are Unix milliseconds.
--
-- Every device has its own features: feature_key is
-- `<kind>:<subject>@<device_id>` (aprs:F4ABC-9@vhf, locator:DL1ABC@hf,
-- call:DL1ABC>F4ABC@hf), so its track, filters and call cap stay its own;
-- subscribers see the features of the devices they may listen to, and the
-- map merges a subject heard by several devices.
-- lat and lon are null for call lines, whose endpoints live in geometry.
-- details holds plain-text values only.
--
-- expires_at is absolute: the expiry job hard-deletes the expired rows and
-- pushes their removal (no tombstones, ADR 0029); null is permanent.

-- +goose Up
CREATE TABLE map_features (
    feature_key TEXT    NOT NULL PRIMARY KEY CHECK (length(feature_key) BETWEEN 3 AND 160),
    kind        TEXT    NOT NULL CHECK (kind IN ('aircraft', 'vessel', 'aprs', 'sonde', 'meshtastic', 'lora', 'locator', 'call',
                                                 'station', 'repeater', 'receiver', 'static', 'node', 'other')),
    subject     TEXT    NOT NULL CHECK (length(subject) BETWEEN 1 AND 128),
    source      TEXT    NOT NULL CHECK (source IN ('decode', 'mqtt', 'web_cache', 'config', 'gps')),
    device_id   TEXT             CHECK (device_id IS NULL OR length(device_id) BETWEEN 1 AND 64),
    lat         REAL             CHECK (lat IS NULL OR lat BETWEEN -90 AND 90),
    lon         REAL             CHECK (lon IS NULL OR lon BETWEEN -180 AND 180),
    geometry    TEXT             CHECK (geometry IS NULL OR json_valid(geometry)),
    details     TEXT    NOT NULL CHECK (json_valid(details)),
    updated_at  INTEGER NOT NULL,
    expires_at  INTEGER
) STRICT;

CREATE INDEX map_features_expires_at ON map_features (expires_at) WHERE expires_at IS NOT NULL;
CREATE INDEX map_features_device ON map_features (device_id, kind, updated_at DESC);
CREATE INDEX map_features_updated_at ON map_features (updated_at DESC);

-- +goose Down
DROP TABLE map_features;
