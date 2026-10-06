package domain

import shared "github.com/yohang/mesh-sdr/internal/shared/domain"

// Grid domain errors. Codes are part of the public API.
var (
	ErrNodeNotFound            = shared.NewError(shared.KindNotFound, "node_not_found", "node not found")
	ErrNodeExists              = shared.NewError(shared.KindConflict, "node_exists", "a node with this id already exists")
	ErrEntityLocked            = shared.NewError(shared.KindConflict, "entity_locked", "the node is declared in the hub config and cannot be deleted")
	ErrSettingLocked           = shared.NewError(shared.KindConflict, "setting_locked", "the field is set in the hub config")
	ErrVersionConflict         = shared.NewError(shared.KindConflict, "version_conflict", "the node was changed concurrently")
	ErrInvalidNodeName         = shared.NewError(shared.KindInvalid, "invalid_node_name", "node name must be 1 to 128 printable characters")
	ErrInvalidNodeURL          = shared.NewError(shared.KindInvalid, "invalid_node_url", "node url must be https://host:port")
	ErrInvalidEnrollmentToken  = shared.NewError(shared.KindInvalid, "enrollment_token_invalid", "enrollment token must be 32 random bytes in unpadded base64url (43 characters), for example `openssl rand -base64 32 | tr +/ -_ | tr -d =`")
	ErrEnrollmentTokenExpired  = shared.NewError(shared.KindConflict, "enrollment_token_expired", "the enrollment token has expired")
	ErrEnrollmentSuperseded    = shared.NewError(shared.KindConflict, "enrollment_superseded", "the enrollment token or node URL changed during the exchange")
	ErrNodeNotPending          = shared.NewError(shared.KindConflict, "node_not_pending", "the node is not waiting for enrollment")
	ErrNodeNotEnrolled         = shared.NewError(shared.KindConflict, "node_not_enrolled", "the node is not enrolled")
	ErrNodeRevoked             = shared.NewError(shared.KindConflict, "node_revoked", "the node is revoked")
	ErrNodeUnavailable         = shared.NewError(shared.KindUnavailable, "node_unavailable", "the node is not connected")
	ErrInvalidCertInfo         = shared.NewError(shared.KindInvalid, "invalid_certificate", "invalid certificate information")
	ErrInvalidDeviceID         = shared.NewError(shared.KindInvalid, "invalid_device_id", "device id must match ^[a-z0-9][a-z0-9_-]{0,62}$")
	ErrDeviceIDConflict        = shared.NewError(shared.KindConflict, "device_id_conflict", "the device id is owned by another node or has another type")
	ErrDeviceNotFound          = shared.NewError(shared.KindNotFound, "device_not_found", "device not found")
	ErrDeviceStillReported     = shared.NewError(shared.KindConflict, "device_reported", "the device is still reported by its node; remove it from the node config first")
	ErrInvalidDevice           = shared.NewError(shared.KindInvalid, "invalid_device", "invalid device report")
	ErrInvalidVersion          = shared.NewError(shared.KindInvalid, "invalid_version", "invalid product version")
	ErrInvalidCapabilityReport = shared.NewError(shared.KindInvalid, "invalid_capability_report", "invalid capability report")
	ErrCapabilitiesNotReported = shared.NewError(shared.KindNotFound, "capabilities_not_reported", "the node has not reported its capabilities yet")
	ErrInvalidConnection       = shared.NewError(shared.KindInvalid, "invalid_connection", "invalid connection")
	ErrConnectionNotFound      = shared.NewError(shared.KindNotFound, "connection_not_found", "connection not found")
)
