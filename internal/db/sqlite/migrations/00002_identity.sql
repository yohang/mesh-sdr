-- Identity core (TECHNICAL_SPEC §5, §7.1): roles, users, user_identities,
-- user_roles, sessions and the append-only audit_log. Generic types per
-- §7.2: UUID = BLOB(16), TIMESTAMP = INTEGER Unix epoch milliseconds (UTC),
-- BOOL = INTEGER 0/1, ENUM = TEXT with CHECK, JSON = TEXT with json_valid.

-- +goose Up
CREATE TABLE roles (
    id          INTEGER PRIMARY KEY CHECK (id BETWEEN -32768 AND 32767),
    name        TEXT    NOT NULL UNIQUE CHECK (length(name) <= 32),
    rank        INTEGER NOT NULL CHECK (rank BETWEEN -32768 AND 32767),
    description TEXT    NOT NULL
);

INSERT INTO roles (id, name, rank, description) VALUES
    (0,  'anonymous', 0,  'Visitor without a session. Never granted.'),
    (10, 'listener',  10, 'Registered user. Implicit for every account.'),
    (20, 'operator',  20, 'Switches shared presets and manages hub-wide bookmarks, globally or on scoped devices.'),
    (30, 'admin',     30, 'Full administration. Always global.');

CREATE TABLE users (
    id                   BLOB    PRIMARY KEY CHECK (length(id) = 16),
    username             TEXT    NOT NULL CHECK (length(username) BETWEEN 2 AND 64),
    email                TEXT    CHECK (email IS NULL OR length(email) <= 254),
    email_verified_at    INTEGER,
    display_name         TEXT    CHECK (display_name IS NULL OR length(display_name) <= 64),
    password_hash        TEXT,
    must_change_password INTEGER NOT NULL DEFAULT 0 CHECK (must_change_password IN (0, 1)),
    enabled              INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    failed_login_count   INTEGER NOT NULL DEFAULT 0 CHECK (failed_login_count BETWEEN 0 AND 32767),
    locked_until         INTEGER,
    last_login_at        INTEGER,
    origin               TEXT    NOT NULL CHECK (origin IN ('config', 'db', 'import')),
    created_at           INTEGER NOT NULL,
    updated_at           INTEGER NOT NULL,
    version              INTEGER NOT NULL DEFAULT 1 CHECK (version BETWEEN 1 AND 2147483647)
);

CREATE UNIQUE INDEX ux_users_username_lower ON users (lower(username));
CREATE UNIQUE INDEX ux_users_email_lower ON users (lower(email)) WHERE email IS NOT NULL;

CREATE TABLE user_identities (
    user_id    BLOB    NOT NULL REFERENCES users (id) ON DELETE CASCADE CHECK (length(user_id) = 16),
    provider   TEXT    NOT NULL CHECK (length(provider) BETWEEN 1 AND 32),
    subject    TEXT    NOT NULL CHECK (length(subject) BETWEEN 1 AND 255),
    created_at INTEGER NOT NULL,
    PRIMARY KEY (provider, subject)
);

CREATE INDEX ix_user_identities_user_id ON user_identities (user_id);

-- device_id has no foreign key yet: the devices table comes with the grid
-- epic, whose migration adds it (ADR 0009).
CREATE TABLE user_roles (
    id         BLOB    PRIMARY KEY CHECK (length(id) = 16),
    user_id    BLOB    NOT NULL REFERENCES users (id) ON DELETE CASCADE CHECK (length(user_id) = 16),
    role_id    INTEGER NOT NULL REFERENCES roles (id) CHECK (role_id <> 0),
    device_id  TEXT    CHECK (device_id IS NULL OR length(device_id) <= 64),
    granted_by BLOB    REFERENCES users (id) ON DELETE SET NULL CHECK (granted_by IS NULL OR length(granted_by) = 16),
    granted_at INTEGER NOT NULL
);

CREATE UNIQUE INDEX ux_user_roles_global ON user_roles (user_id, role_id) WHERE device_id IS NULL;
CREATE UNIQUE INDEX ux_user_roles_device ON user_roles (user_id, role_id, device_id) WHERE device_id IS NOT NULL;
CREATE INDEX ix_user_roles_device_id ON user_roles (device_id);

CREATE TABLE sessions (
    id                  BLOB    PRIMARY KEY CHECK (length(id) = 16),
    token_hash          BLOB    NOT NULL UNIQUE CHECK (length(token_hash) = 32),
    csrf_secret         BLOB    NOT NULL CHECK (length(csrf_secret) = 32),
    user_id             BLOB    NOT NULL REFERENCES users (id) ON DELETE CASCADE CHECK (length(user_id) = 16),
    auth_provider       TEXT    NOT NULL CHECK (length(auth_provider) BETWEEN 1 AND 32),
    created_at          INTEGER NOT NULL,
    last_seen_at        INTEGER NOT NULL,
    idle_expires_at     INTEGER NOT NULL,
    absolute_expires_at INTEGER NOT NULL,
    ip                  TEXT    CHECK (ip IS NULL OR length(ip) <= 45),
    user_agent          TEXT    CHECK (user_agent IS NULL OR length(user_agent) <= 512),
    revoked_at          INTEGER,
    revoke_reason       TEXT    CHECK (revoke_reason IS NULL OR revoke_reason IN
        ('logout', 'admin', 'password_change', 'password_reset', 'user_disabled', 'expired', 'rotated'))
);

CREATE INDEX ix_sessions_user_id ON sessions (user_id);
CREATE INDEX ix_sessions_idle_expires_at ON sessions (idle_expires_at);

CREATE TABLE audit_log (
    id            INTEGER PRIMARY KEY,
    at            INTEGER NOT NULL,
    actor_kind    TEXT    NOT NULL CHECK (actor_kind IN ('user', 'anonymous', 'system', 'node', 'cli')),
    actor_user_id BLOB    CHECK (actor_user_id IS NULL OR length(actor_user_id) = 16),
    actor_ip      TEXT    CHECK (actor_ip IS NULL OR length(actor_ip) <= 45),
    action        TEXT    NOT NULL CHECK (length(action) BETWEEN 1 AND 64),
    target_type   TEXT    CHECK (target_type IS NULL OR length(target_type) <= 32),
    target_id     TEXT    CHECK (target_id IS NULL OR length(target_id) <= 128),
    result        TEXT    NOT NULL CHECK (result IN ('ok', 'denied', 'error')),
    before        TEXT    CHECK (before IS NULL OR json_valid(before)),
    after         TEXT    CHECK (after IS NULL OR json_valid(after)),
    request_id    TEXT    CHECK (request_id IS NULL OR length(request_id) <= 64)
);

CREATE INDEX ix_audit_log_at ON audit_log (at DESC);
CREATE INDEX ix_audit_log_actor_at ON audit_log (actor_user_id, at);
CREATE INDEX ix_audit_log_action_at ON audit_log (action, at);

-- The audit log is append-only: only the retention job deletes rows.
-- +goose StatementBegin
CREATE TRIGGER audit_log_append_only BEFORE UPDATE ON audit_log
BEGIN
    SELECT RAISE(ABORT, 'audit_log is append-only');
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER audit_log_append_only;
DROP TABLE audit_log;
DROP TABLE sessions;
DROP TABLE user_roles;
DROP TABLE user_identities;
DROP TABLE users;
DROP TABLE roles;
