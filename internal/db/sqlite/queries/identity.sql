-- name: InsertUser :exec
INSERT INTO users (
    id, username, email, email_verified_at, display_name, password_hash, must_change_password,
    enabled, failed_login_count, locked_until, last_login_at, origin, created_at, updated_at, version
) VALUES (
    sqlc.arg(id), sqlc.arg(username), sqlc.narg(email), sqlc.narg(email_verified_at), sqlc.narg(display_name),
    sqlc.narg(password_hash), sqlc.arg(must_change_password), sqlc.arg(enabled), sqlc.arg(failed_login_count),
    sqlc.narg(locked_until), sqlc.narg(last_login_at), sqlc.arg(origin), sqlc.arg(created_at), sqlc.arg(updated_at),
    sqlc.arg(version)
);

-- name: UpdateUser :execrows
UPDATE users SET
    email = sqlc.narg(email),
    email_verified_at = sqlc.narg(email_verified_at),
    display_name = sqlc.narg(display_name),
    password_hash = sqlc.narg(password_hash),
    must_change_password = sqlc.arg(must_change_password),
    enabled = sqlc.arg(enabled),
    failed_login_count = sqlc.arg(failed_login_count),
    locked_until = sqlc.narg(locked_until),
    last_login_at = sqlc.narg(last_login_at),
    updated_at = sqlc.arg(updated_at),
    version = version + 1
WHERE id = sqlc.arg(id) AND version = sqlc.arg(version);

-- name: GetUserByID :one
SELECT * FROM users WHERE id = sqlc.arg(id);

-- name: GetUserByUsername :one
SELECT * FROM users WHERE lower(username) = lower(sqlc.arg(username));

-- name: GetUserByEmail :one
SELECT * FROM users WHERE email IS NOT NULL AND lower(email) = lower(sqlc.arg(email));

-- name: GetUserByIdentity :one
SELECT u.* FROM users u
JOIN user_identities i ON i.user_id = u.id
WHERE i.provider = sqlc.arg(provider) AND i.subject = sqlc.arg(subject);

-- name: InsertUserIdentity :exec
INSERT INTO user_identities (user_id, provider, subject, created_at)
VALUES (sqlc.arg(user_id), sqlc.arg(provider), sqlc.arg(subject), sqlc.arg(created_at));

-- name: ListUserIdentities :many
SELECT provider, subject FROM user_identities WHERE user_id = sqlc.arg(user_id) ORDER BY provider, subject;

-- name: InsertUserRole :exec
INSERT INTO user_roles (id, user_id, role_id, device_id, granted_by, granted_at)
VALUES (sqlc.arg(id), sqlc.arg(user_id), sqlc.arg(role_id), sqlc.narg(device_id), sqlc.narg(granted_by), sqlc.arg(granted_at));

-- name: ListUserRoles :many
SELECT role_id, device_id FROM user_roles WHERE user_id = sqlc.arg(user_id) ORDER BY role_id, device_id;

-- name: InsertSession :exec
INSERT INTO sessions (
    id, token_hash, csrf_secret, user_id, auth_provider, created_at, last_seen_at, idle_expires_at,
    absolute_expires_at, ip, user_agent, revoked_at, revoke_reason
) VALUES (
    sqlc.arg(id), sqlc.arg(token_hash), sqlc.arg(csrf_secret), sqlc.arg(user_id), sqlc.arg(auth_provider),
    sqlc.arg(created_at), sqlc.arg(last_seen_at), sqlc.arg(idle_expires_at), sqlc.arg(absolute_expires_at),
    sqlc.narg(ip), sqlc.narg(user_agent), sqlc.narg(revoked_at), sqlc.narg(revoke_reason)
);

-- name: TouchSession :exec
UPDATE sessions SET
    last_seen_at = sqlc.arg(last_seen_at),
    idle_expires_at = sqlc.arg(idle_expires_at)
WHERE id = sqlc.arg(id) AND revoked_at IS NULL;

-- name: RevokeSession :exec
UPDATE sessions SET
    revoked_at = COALESCE(revoked_at, sqlc.arg(revoked_at)),
    revoke_reason = COALESCE(revoke_reason, sqlc.arg(revoke_reason))
WHERE id = sqlc.arg(id);

-- name: GetSessionByTokenHash :one
SELECT * FROM sessions WHERE token_hash = sqlc.arg(token_hash);

-- name: RevokeUserSessions :execrows
UPDATE sessions SET
    revoked_at = COALESCE(revoked_at, sqlc.arg(revoked_at)),
    revoke_reason = COALESCE(revoke_reason, sqlc.arg(revoke_reason))
WHERE user_id = sqlc.arg(user_id) AND revoked_at IS NULL;

-- name: DeleteEndedSessions :execrows
DELETE FROM sessions WHERE id IN (
    SELECT s.id FROM sessions s
    WHERE (s.revoked_at IS NOT NULL AND s.revoked_at < sqlc.arg(cutoff))
       OR s.idle_expires_at < sqlc.arg(cutoff)
       OR s.absolute_expires_at < sqlc.arg(cutoff)
    LIMIT sqlc.arg(batch)
);

-- name: InsertAuditEntry :exec
INSERT INTO audit_log (at, actor_kind, actor_user_id, actor_ip, action, target_type, target_id, result, before, after, request_id)
VALUES (
    sqlc.arg(at), sqlc.arg(actor_kind), sqlc.narg(actor_user_id), sqlc.narg(actor_ip), sqlc.arg(action),
    sqlc.narg(target_type), sqlc.narg(target_id), sqlc.arg(result), sqlc.narg(before), sqlc.narg(after),
    sqlc.narg(request_id)
);

-- name: ListRecentAuditEntries :many
SELECT * FROM audit_log ORDER BY id DESC LIMIT sqlc.arg(max_rows);
