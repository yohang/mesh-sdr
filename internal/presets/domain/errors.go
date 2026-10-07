package domain

import shared "github.com/yohang/mesh-sdr/internal/shared/domain"

// Domain errors (stable codes).
var (
	ErrPresetNotFound = shared.NewError(shared.KindNotFound, "preset_not_found", "preset not found")
	ErrInvalidPreset  = shared.NewError(shared.KindInvalid, "invalid_preset", "invalid preset")
	ErrSlugTaken      = shared.NewError(shared.KindConflict, "preset_slug_taken", "another preset has this slug")
	// ErrPresetInUse refuses to delete a preset that schedules reference
	// (ADM-020): they are fixed first.
	ErrPresetInUse     = shared.NewError(shared.KindConflict, "preset_in_use", "schedules reference this preset")
	ErrVersionConflict = shared.NewError(shared.KindConflict, "version_conflict", "the preset was changed meanwhile")
	// ErrPresetIncompatible is a preset that does not fit a device
	// (§7.1 "Capability validation at apply time").
	ErrPresetIncompatible = shared.NewError(shared.KindInvalid, "preset_incompatible", "the preset does not fit the device")
	// ErrUnknownDevice is a device the registry does not hold.
	ErrUnknownDevice = shared.NewError(shared.KindInvalid, "unknown_device", "no such device in the registry")
)
