-- Map features (TECHNICAL_SPEC §7.1 `map_features`, MAP-002, MAP-012, ADR
-- 0029 "M3a scope"): the hub projects them from the decoded messages at
-- ingest, in the ingest transaction. Times are Unix milliseconds.
--
-- feature_key is `<kind>:<subject>` (aprs:F4ABC-9, locator:DL1ABC,
-- call:F4ABC>DL1ABC). device_id is the device that reported the feature
-- last: subscribers see the features of the devices they may listen to.
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
CREATE INDEX map_features_kind ON map_features (kind, updated_at DESC);

-- +goose Down
DROP TABLE map_features;
