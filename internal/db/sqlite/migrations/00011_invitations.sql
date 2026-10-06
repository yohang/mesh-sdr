-- Invitations (TECHNICAL_SPEC §5.4, §7.1 `invitations`, ACC-002): the only
-- way to create an account from the web UI. Only the SHA-256 of the token is
-- stored. device_id has no foreign key until the devices table exists in
-- every branch (ADR 0011).

-- +goose Up
CREATE TABLE invitations (
    id               BLOB    PRIMARY KEY CHECK (length(id) = 16),
    token_hash       BLOB    NOT NULL UNIQUE CHECK (length(token_hash) = 32),
    delivery         TEXT    NOT NULL CHECK (delivery IN ('email', 'link')),
    email            TEXT    CHECK (email IS NULL OR length(email) <= 254),
    role_id          INTEGER NOT NULL REFERENCES roles (id) CHECK (role_id IN (10, 20, 30)),
    device_id        TEXT    CHECK (device_id IS NULL OR length(device_id) <= 64),
    created_by       BLOB    REFERENCES users (id) ON DELETE SET NULL CHECK (created_by IS NULL OR length(created_by) = 16),
    created_at       INTEGER NOT NULL,
    expires_at       INTEGER NOT NULL,
    redeemed_at      INTEGER,
    redeemed_user_id BLOB    REFERENCES users (id) ON DELETE SET NULL CHECK (redeemed_user_id IS NULL OR length(redeemed_user_id) = 16),
    revoked_at       INTEGER,
    CHECK (delivery = 'link' OR email IS NOT NULL),
    CHECK (role_id <> 30 OR device_id IS NULL)
);

CREATE INDEX ix_invitations_expires_at ON invitations (expires_at);
CREATE INDEX ix_invitations_redeemed_user_id ON invitations (redeemed_user_id);

-- +goose Down
DROP TABLE invitations;
