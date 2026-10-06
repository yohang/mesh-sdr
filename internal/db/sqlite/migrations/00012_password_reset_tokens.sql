-- Password reset tokens (TECHNICAL_SPEC §5.3, §7.1 `password_reset_tokens`,
-- ACC-003): single use, short-lived, only the SHA-256 is stored.

-- +goose Up
CREATE TABLE password_reset_tokens (
    id           BLOB    PRIMARY KEY CHECK (length(id) = 16),
    user_id      BLOB    NOT NULL REFERENCES users (id) ON DELETE CASCADE CHECK (length(user_id) = 16),
    token_hash   BLOB    NOT NULL UNIQUE CHECK (length(token_hash) = 32),
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    used_at      INTEGER,
    requested_ip TEXT    CHECK (requested_ip IS NULL OR length(requested_ip) <= 45)
);

CREATE INDEX ix_password_reset_tokens_user_id ON password_reset_tokens (user_id);

-- +goose Down
DROP TABLE password_reset_tokens;
