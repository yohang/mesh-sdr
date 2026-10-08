-- Decoded messages (TECHNICAL_SPEC §7.1 `decoded_messages`, DEC-047, ADR
-- 0028): the hub is the only writer, from the node decode.batch events.
-- Times are Unix milliseconds: decoded_at is the node's decode time (the
-- slot start for slot modes), received_at the hub's.
--
-- No foreign key: a message outlives its node, device, preset and session.
-- The call, locator and position stay in the JSON payload until the map
-- (M3) needs columns.
--
-- dedup_key is the first 16 bytes of SHA-256(device, mode, second of
-- decoded_at, frequency, text): two listeners decoding the same signal on
-- one device store it once, and a replayed event is skipped.

-- +goose Up
CREATE TABLE decoded_messages (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    decoded_at         INTEGER NOT NULL,
    received_at        INTEGER NOT NULL,
    node_id            TEXT             CHECK (node_id IS NULL OR length(node_id) BETWEEN 1 AND 64),
    device_id          TEXT             CHECK (device_id IS NULL OR length(device_id) BETWEEN 1 AND 64),
    preset_id          BLOB             CHECK (preset_id IS NULL OR length(preset_id) = 16),
    decoder_session_id BLOB             CHECK (decoder_session_id IS NULL OR length(decoder_session_id) = 16),
    origin             TEXT    NOT NULL CHECK (origin IN ('service', 'listener', 'mqtt')),
    mode               TEXT    NOT NULL CHECK (length(mode) BETWEEN 1 AND 24),
    family             TEXT    NOT NULL CHECK (length(family) BETWEEN 1 AND 16),
    frequency          INTEGER          CHECK (frequency IS NULL OR frequency > 0),
    text               TEXT             CHECK (text IS NULL OR length(CAST(text AS BLOB)) <= 4096),
    payload            TEXT    NOT NULL CHECK (json_valid(payload)),
    payload_schema     TEXT    NOT NULL CHECK (length(payload_schema) BETWEEN 1 AND 32),
    dedup_key          BLOB    NOT NULL UNIQUE CHECK (length(dedup_key) = 16)
);

CREATE INDEX decoded_messages_decoded_at ON decoded_messages (decoded_at);
CREATE INDEX decoded_messages_mode ON decoded_messages (mode, decoded_at DESC);
CREATE INDEX decoded_messages_device ON decoded_messages (device_id, decoded_at DESC);

-- +goose Down
DROP TABLE decoded_messages;
