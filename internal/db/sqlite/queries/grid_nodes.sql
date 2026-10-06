-- name: CreateNode :execrows
INSERT INTO nodes (
    id, name, url, enrollment_state, enrollment_token_digest, enrollment_token_expires_at,
    enrolled_at, cert_fingerprint, cert_serial, cert_not_after, status, status_hint,
    last_heartbeat_at, boot_id, software_version, protocol_version, hostname, cpu_cores,
    clock_offset_ms, origin, locked_fields, disabled, created_at, updated_at, version,
    cert_pending_fingerprint, cert_pending_serial, cert_pending_not_after
) VALUES (
    ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
) ON CONFLICT (id) DO NOTHING;

-- name: GetNode :one
SELECT * FROM nodes WHERE id = ?;

-- name: ListNodes :many
SELECT * FROM nodes ORDER BY id;

-- name: UpdateNode :execrows
UPDATE nodes SET
    name = sqlc.arg(name),
    url = sqlc.arg(url),
    enrollment_state = sqlc.arg(enrollment_state),
    enrollment_token_digest = sqlc.arg(enrollment_token_digest),
    enrollment_token_expires_at = sqlc.arg(enrollment_token_expires_at),
    enrolled_at = sqlc.arg(enrolled_at),
    cert_fingerprint = sqlc.arg(cert_fingerprint),
    cert_serial = sqlc.arg(cert_serial),
    cert_not_after = sqlc.arg(cert_not_after),
    cert_pending_fingerprint = sqlc.arg(cert_pending_fingerprint),
    cert_pending_serial = sqlc.arg(cert_pending_serial),
    cert_pending_not_after = sqlc.arg(cert_pending_not_after),
    origin = sqlc.arg(origin),
    locked_fields = sqlc.arg(locked_fields),
    disabled = sqlc.arg(disabled),
    updated_at = sqlc.arg(updated_at),
    version = sqlc.arg(version)
WHERE id = sqlc.arg(id) AND version = sqlc.arg(expected_version);

-- name: UpdateNodeRuntime :execrows
UPDATE nodes SET
    status = sqlc.arg(status),
    status_hint = sqlc.arg(status_hint),
    last_heartbeat_at = sqlc.arg(last_heartbeat_at),
    boot_id = sqlc.arg(boot_id),
    software_version = sqlc.arg(software_version),
    protocol_version = sqlc.arg(protocol_version),
    hostname = sqlc.arg(hostname),
    cpu_cores = sqlc.arg(cpu_cores),
    clock_offset_ms = sqlc.arg(clock_offset_ms)
WHERE id = sqlc.arg(id);

-- name: DeleteNode :execrows
DELETE FROM nodes WHERE id = ?;

-- name: AddRevokedCertificate :exec
INSERT INTO revoked_certificates (serial, node_id, not_after, revoked_at, reason)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (serial) DO NOTHING;

-- name: ListRevokedCertificates :many
SELECT * FROM revoked_certificates WHERE not_after > ? ORDER BY serial;

-- name: UpdateNodeStatus :execrows
UPDATE nodes SET status = sqlc.arg(status), status_hint = sqlc.arg(status_hint) WHERE id = sqlc.arg(id);
