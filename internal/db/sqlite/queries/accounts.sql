-- name: SearchUsers :many
SELECT u.* FROM users u
WHERE (CAST(sqlc.arg(text) AS TEXT) = '' OR instr(lower(u.username), lower(CAST(sqlc.arg(text) AS TEXT))) > 0
        OR instr(lower(coalesce(u.email, '')), lower(CAST(sqlc.arg(text) AS TEXT))) > 0
        OR instr(lower(coalesce(u.display_name, '')), lower(CAST(sqlc.arg(text) AS TEXT))) > 0)
  AND (CAST(sqlc.arg(enabled) AS INTEGER) < 0 OR u.enabled = CAST(sqlc.arg(enabled) AS INTEGER))
  AND (CAST(sqlc.arg(never_logged_in) AS INTEGER) = 0 OR u.last_login_at IS NULL)
  AND (CAST(sqlc.arg(role_id) AS INTEGER) = 0 OR coalesce(
        (SELECT max(r.role_id) FROM user_roles r WHERE r.user_id = u.id AND r.device_id IS NULL), 10) = CAST(sqlc.arg(role_id) AS INTEGER))
  AND lower(u.username) > sqlc.arg(after)
ORDER BY lower(u.username)
LIMIT sqlc.arg(max_rows);

-- name: DeleteUser :execrows
DELETE FROM users WHERE id = sqlc.arg(id);

-- name: DeleteUserRole :exec
DELETE FROM user_roles
WHERE user_id = sqlc.arg(user_id) AND role_id = CAST(sqlc.arg(role_id) AS INTEGER) AND coalesce(device_id, '') = CAST(sqlc.arg(device_id) AS TEXT);

-- name: ListActiveUserSessions :many
SELECT * FROM sessions
WHERE user_id = sqlc.arg(user_id) AND revoked_at IS NULL
  AND idle_expires_at > sqlc.arg(now) AND absolute_expires_at > sqlc.arg(now)
ORDER BY last_seen_at DESC, created_at DESC;

-- name: InsertInvitation :exec
INSERT INTO invitations (
    id, token_hash, delivery, email, role_id, device_id, created_by, created_at, expires_at,
    redeemed_at, redeemed_user_id, revoked_at
) VALUES (
    sqlc.arg(id), sqlc.arg(token_hash), sqlc.arg(delivery), sqlc.narg(email), CAST(sqlc.arg(role_id) AS INTEGER), sqlc.narg(device_id),
    sqlc.narg(created_by), sqlc.arg(created_at), sqlc.arg(expires_at), sqlc.narg(redeemed_at),
    sqlc.narg(redeemed_user_id), sqlc.narg(revoked_at)
);

-- name: UpdateInvitation :execrows
UPDATE invitations SET
    redeemed_at = sqlc.narg(redeemed_at),
    redeemed_user_id = sqlc.narg(redeemed_user_id),
    revoked_at = sqlc.narg(revoked_at)
WHERE id = sqlc.arg(id) AND redeemed_at IS NULL AND revoked_at IS NULL;

-- name: GetInvitationByID :one
SELECT * FROM invitations WHERE id = sqlc.arg(id);

-- name: GetInvitationByTokenHash :one
SELECT * FROM invitations WHERE token_hash = sqlc.arg(token_hash);

-- name: ListInvitations :many
SELECT * FROM invitations ORDER BY created_at DESC, id DESC LIMIT sqlc.arg(max_rows);

-- name: ClearInvitationEmailOfUser :exec
UPDATE invitations SET email = NULL, delivery = 'link' WHERE redeemed_user_id = sqlc.arg(user_id);

-- name: DeleteEndedInvitations :execrows
DELETE FROM invitations WHERE id IN (
    SELECT i.id FROM invitations i
    WHERE i.expires_at < sqlc.arg(cutoff)
       OR (i.redeemed_at IS NOT NULL AND i.redeemed_at < sqlc.arg(cutoff))
       OR (i.revoked_at IS NOT NULL AND i.revoked_at < sqlc.arg(cutoff))
    LIMIT sqlc.arg(batch)
);

-- name: InsertPasswordResetToken :exec
INSERT INTO password_reset_tokens (id, user_id, token_hash, created_at, expires_at, used_at, requested_ip)
VALUES (sqlc.arg(id), sqlc.arg(user_id), sqlc.arg(token_hash), sqlc.arg(created_at), sqlc.arg(expires_at),
        sqlc.narg(used_at), sqlc.narg(requested_ip));

-- name: InvalidatePasswordResetTokens :exec
UPDATE password_reset_tokens SET used_at = CAST(sqlc.arg(now) AS INTEGER)
WHERE user_id = sqlc.arg(user_id) AND used_at IS NULL;

-- name: UsePasswordResetToken :execrows
UPDATE password_reset_tokens SET used_at = sqlc.arg(used_at) WHERE id = sqlc.arg(id) AND used_at IS NULL;

-- name: GetPasswordResetToken :one
SELECT * FROM password_reset_tokens WHERE token_hash = sqlc.arg(token_hash);

-- name: DeleteEndedPasswordResetTokens :execrows
DELETE FROM password_reset_tokens WHERE id IN (
    SELECT t.id FROM password_reset_tokens t
    WHERE t.expires_at < sqlc.arg(cutoff) OR (t.used_at IS NOT NULL AND t.used_at < sqlc.arg(cutoff))
    LIMIT sqlc.arg(batch)
);

-- name: InsertEmailChangeToken :exec
INSERT INTO email_change_tokens (id, user_id, token_hash, new_email, created_at, expires_at, used_at)
VALUES (sqlc.arg(id), sqlc.arg(user_id), sqlc.arg(token_hash), sqlc.arg(new_email), sqlc.arg(created_at),
        sqlc.arg(expires_at), sqlc.narg(used_at));

-- name: InvalidateEmailChangeTokens :exec
UPDATE email_change_tokens SET used_at = CAST(sqlc.arg(now) AS INTEGER)
WHERE user_id = sqlc.arg(user_id) AND used_at IS NULL;

-- name: UseEmailChangeToken :execrows
UPDATE email_change_tokens SET used_at = sqlc.arg(used_at) WHERE id = sqlc.arg(id) AND used_at IS NULL;

-- name: GetEmailChangeToken :one
SELECT * FROM email_change_tokens WHERE token_hash = sqlc.arg(token_hash);

-- name: DeleteEndedEmailChangeTokens :execrows
DELETE FROM email_change_tokens WHERE id IN (
    SELECT t.id FROM email_change_tokens t
    WHERE t.expires_at < sqlc.arg(cutoff) OR (t.used_at IS NOT NULL AND t.used_at < sqlc.arg(cutoff))
    LIMIT sqlc.arg(batch)
);

-- name: SearchAuditEntries :many
SELECT * FROM audit_log
WHERE (CAST(sqlc.arg(before_id) AS INTEGER) = 0 OR id < CAST(sqlc.arg(before_id) AS INTEGER))
  AND (coalesce(length(CAST(sqlc.arg(actor_user_id) AS BLOB)), 0) = 0 OR actor_user_id = CAST(sqlc.arg(actor_user_id) AS BLOB))
  AND (CAST(sqlc.arg(actor_kind) AS TEXT) = '' OR actor_kind = CAST(sqlc.arg(actor_kind) AS TEXT))
  AND (CAST(sqlc.arg(action_prefix) AS TEXT) = '' OR substr(action, 1, length(CAST(sqlc.arg(action_prefix) AS TEXT))) = CAST(sqlc.arg(action_prefix) AS TEXT))
  AND (CAST(sqlc.arg(target_type) AS TEXT) = '' OR target_type = CAST(sqlc.arg(target_type) AS TEXT))
  AND (CAST(sqlc.arg(target_id) AS TEXT) = '' OR target_id = CAST(sqlc.arg(target_id) AS TEXT))
  AND (CAST(sqlc.arg(from_ms) AS INTEGER) = 0 OR at >= CAST(sqlc.arg(from_ms) AS INTEGER))
  AND (CAST(sqlc.arg(to_ms) AS INTEGER) = 0 OR at < CAST(sqlc.arg(to_ms) AS INTEGER))
ORDER BY id DESC
LIMIT sqlc.arg(max_rows);

-- name: ListUsernames :many
SELECT id, username FROM users WHERE id IN (sqlc.slice(ids));
