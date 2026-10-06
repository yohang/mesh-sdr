-- E-mail change tokens (ACC-004, auxiliary table): a changed address is used
-- once its owner opens the single-use link sent to it. Only the SHA-256 of
-- the token is stored.

-- +goose Up
CREATE TABLE email_change_tokens (
    id         BLOB    PRIMARY KEY CHECK (length(id) = 16),
    user_id    BLOB    NOT NULL REFERENCES users (id) ON DELETE CASCADE CHECK (length(user_id) = 16),
    token_hash BLOB    NOT NULL UNIQUE CHECK (length(token_hash) = 32),
    new_email  TEXT    NOT NULL CHECK (length(new_email) <= 254),
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    used_at    INTEGER
);

CREATE INDEX ix_email_change_tokens_user_id ON email_change_tokens (user_id);

-- +goose Down
DROP TABLE email_change_tokens;
