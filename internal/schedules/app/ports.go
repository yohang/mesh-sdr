// Package app holds the schedule use cases (ADR 0020): admin CRUD, the
// guard that disables schedules whose device or preset no longer allows
// them (GRID-016, ADM-009), and the planner that computes each device's
// desired state for the control channel. Ports are declared here.
package app

import (
	"context"
	"time"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Clock returns the current time.
type Clock func() time.Time

// IDs generates schedule ids (UUIDv7).
type IDs interface {
	New(now time.Time) (shared.UUID, error)
}

// Transactor runs a unit of work in one write transaction.
type Transactor interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context) error) error
}

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
