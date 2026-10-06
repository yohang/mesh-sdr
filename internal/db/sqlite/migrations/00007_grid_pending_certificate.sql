-- Renewed node certificate sent but not confirmed yet (ADR 0008): the hub
-- accepts it as well as the current one until the node acknowledges it.

-- +goose Up
ALTER TABLE nodes ADD COLUMN cert_pending_fingerprint BLOB CHECK (cert_pending_fingerprint IS NULL OR length(cert_pending_fingerprint) = 32);
ALTER TABLE nodes ADD COLUMN cert_pending_serial TEXT CHECK (cert_pending_serial IS NULL OR length(cert_pending_serial) <= 64);
ALTER TABLE nodes ADD COLUMN cert_pending_not_after INTEGER;

-- +goose Down
ALTER TABLE nodes DROP COLUMN cert_pending_not_after;
ALTER TABLE nodes DROP COLUMN cert_pending_serial;
ALTER TABLE nodes DROP COLUMN cert_pending_fingerprint;
