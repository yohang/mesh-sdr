-- name: DeleteNodeCapabilities :exec
DELETE FROM node_capabilities WHERE node_id = ?;

-- name: InsertNodeCapability :exec
INSERT INTO node_capabilities (node_id, capability, available, status, version, detail, probed_at, probe_ms, error)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ListNodeCapabilities :many
SELECT * FROM node_capabilities WHERE node_id = ? ORDER BY capability;

-- name: UpsertCapabilityReport :exec
INSERT INTO node_capability_reports (node_id, document, capabilities_hash, product_version, protocols, platform, reported_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (node_id) DO UPDATE SET
    document = excluded.document,
    capabilities_hash = excluded.capabilities_hash,
    product_version = excluded.product_version,
    protocols = excluded.protocols,
    platform = excluded.platform,
    reported_at = excluded.reported_at;

-- name: GetCapabilityReport :one
SELECT * FROM node_capability_reports WHERE node_id = ?;
