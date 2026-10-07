package schedules

import (
	"context"
	"time"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Clock returns the current time.
type Clock func() time.Time

// Device is what schedules need of a device of the registry.
type Device struct {
	ID, Node         string
	Name             string
	FreqMin, FreqMax int64
	SampleRates      []int64
	SchedulerEnabled bool
	// Stale is set when the node reports its devices without this one.
	Stale bool
	// ActivePreset is the last preset the node applied (zero if none).
	ActivePreset shared.UUID
}

// Devices reads the device registry.
type Devices interface {
	// Device returns a device; ok is false when the registry lacks it.
	Device(ctx context.Context, id string) (d Device, ok bool, err error)
	// NodeDevices returns the devices of a node, in node-config order.
	NodeDevices(ctx context.Context, node string) ([]Device, error)
	// All returns every device of the registry.
	All(ctx context.Context) ([]Device, error)
}

// Fit is the answer of the preset catalogue about a preset and a device.
type Fit struct {
	Exists bool
	// Reason is empty when the preset fits the device, otherwise the
	// failed check (preset_incompatible detail).
	Reason string
}

// Presets is the preset catalogue.
type Presets interface {
	Fit(ctx context.Context, preset shared.UUID, d Device) (Fit, error)
	// Compatible returns the presets that fit d, by sort order.
	Compatible(ctx context.Context, d Device) ([]shared.UUID, error)
}

// Audit actions.
const (
	ActionCreate  = "schedule.create"
	ActionUpdate  = "schedule.update"
	ActionDelete  = "schedule.delete"
	ActionDisable = "schedule.disable"
)
