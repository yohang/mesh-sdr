package schedules

import shared "github.com/yohang/mesh-sdr/internal/shared/domain"

// Domain errors (stable codes).
var (
	ErrScheduleNotFound = shared.NewError(shared.KindNotFound, "schedule_not_found", "schedule not found")
	ErrInvalidSchedule  = shared.NewError(shared.KindInvalid, "invalid_schedule", "invalid schedule")
	// ErrKindUnsupported refuses daylight windows until SVC-011 (ADR 0020
	// Q3).
	ErrKindUnsupported = shared.NewError(shared.KindInvalid, "schedule_kind_unsupported",
		"daylight schedules are not supported yet: use a static window")
	ErrVersionConflict = shared.NewError(shared.KindConflict, "version_conflict", "the schedule was changed meanwhile")
	// ErrUnknownDevice is a schedule naming a device the registry does not
	// hold.
	ErrUnknownDevice = shared.NewError(shared.KindInvalid, "unknown_device", "no such device in the registry")
	// ErrDeviceUnavailable is an enabled schedule on a device its node no
	// longer reports.
	ErrDeviceUnavailable = shared.NewError(shared.KindInvalid, "device_unavailable", "the device is no longer reported by its node")
	// ErrUnknownPreset is a schedule naming a preset that does not exist.
	ErrUnknownPreset = shared.NewError(shared.KindInvalid, "unknown_preset", "no such preset")
	// ErrPresetIncompatible is an enabled schedule whose preset does not
	// fit the device (same code as the presets module).
	ErrPresetIncompatible = shared.NewError(shared.KindInvalid, "preset_incompatible", "the preset does not fit the device")
)
