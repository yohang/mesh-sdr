package settings

import shared "github.com/yohang/mesh-sdr/internal/shared/domain"

// Errors of the settings store. Codes are part of the public API.
var (
	// ErrInvalidKey is a malformed setting key.
	ErrInvalidKey = shared.NewError(shared.KindInvalid, "invalid_setting_key", "invalid setting key")
	// ErrInvalidValue is a value that is not valid JSON.
	ErrInvalidValue = shared.NewError(shared.KindInvalid, "invalid_setting_value", "setting value must be valid JSON")
	// ErrUnknownSetting is a key that the settings schema does not define.
	ErrUnknownSetting = shared.NewError(shared.KindInvalid, "unknown_setting", "unknown setting")
	// ErrInvalidSetting is a rejected write; its violations name each key.
	ErrInvalidSetting = shared.NewError(shared.KindInvalid, "invalid_setting", "invalid setting values")
	// ErrSettingLocked is a write to a key set in the hub config (file or
	// env); its message names the origin.
	ErrSettingLocked = shared.NewError(shared.KindConflict, "setting_locked", "setting is locked by the hub configuration")
	// ErrVersionConflict is a write based on a stale version of a key.
	ErrVersionConflict = shared.NewError(shared.KindConflict, "version_conflict", "setting was changed meanwhile; reload and try again")
	// ErrSecretsUnavailable is a write to a secret key while the hub has no
	// secrets.master_key to encrypt it (TECHNICAL_SPEC §7.4 "Secrets").
	ErrSecretsUnavailable = shared.NewError(shared.KindConflict, "secrets_unavailable", "secret settings cannot be stored without secrets.master_key")
	// ErrInvalidSettingRow is a persisted row that breaks an invariant.
	ErrInvalidSettingRow = shared.NewError(shared.KindInvalid, "invalid_setting_row", "invalid persisted setting")
)
