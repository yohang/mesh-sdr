package domain

import (
	"slices"
	"strconv"
	"unicode/utf8"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// State is the runtime state of a device (§8.2 "Lifecycle"; the wire and DB
// names).
type State string

// Device states.
const (
	StateUnavailable State = "unavailable"
	StateDisabled    State = "disabled"
	StateStopped     State = "stopped"
	StateStarting    State = "starting"
	StateRunning     State = "running"
	StateRetuning    State = "retuning"
	StateStopping    State = "stopping"
	StateRetryWait   State = "retry_wait"
	StateFailed      State = "failed"
)

// transitions is the §8.2 state diagram, plus the shortcuts a supervised
// process takes (a start that ends without retry goes straight to failed
// or unavailable, a stop during a start goes to stopped).
var transitions = map[State][]State{
	StateUnavailable: {StateStopped},
	StateDisabled:    {StateStopped},
	StateStopped:     {StateStarting, StateDisabled, StateUnavailable},
	StateStarting:    {StateRunning, StateRetryWait, StateStopping, StateStopped, StateFailed, StateUnavailable},
	StateRunning:     {StateRetryWait, StateRetuning, StateStopping, StateStopped, StateFailed, StateUnavailable},
	StateRetuning:    {StateRunning, StateRetryWait, StateStopping, StateStopped},
	StateRetryWait:   {StateStarting, StateStopping, StateStopped, StateFailed, StateUnavailable},
	StateStopping:    {StateStopped, StateRetryWait, StateFailed},
	StateFailed:      {StateStarting, StateStopped, StateUnavailable},
}

// DeviceParams are the configured properties of a device (node.toml
// [devices.<id>], §7.4).
type DeviceParams struct {
	ID                shared.DeviceID
	Name              string
	Type              DeviceType
	Enabled           bool
	Range             FreqRange
	Rates             []SampleRate
	AlwaysOn          bool
	OperatorCanRetune bool
	AutoRecover       bool
	Driver            Driver
	// MaxDemods caps the demodulators of the device (0: DefaultMaxDemods).
	MaxDemods int
}

// DefaultMaxDemods is the interim per-device demodulator cap (ADR 0019).
const DefaultMaxDemods = 16

// Device is one SDR device of the node: its configuration (immutable for
// the life of the process) and its runtime state.
type Device struct {
	p DeviceParams

	state     State
	reason    string
	attempt   int
	tuning    Tuning
	listeners int
}

// NewDevice checks the invariants of p. The device starts disabled when
// not enabled, unavailable when its type has no driver on this node, and
// stopped otherwise.
func NewDevice(p DeviceParams) (*Device, error) {
	switch {
	case p.Name == "" || utf8.RuneCountInString(p.Name) > 128:
		return nil, ErrInvalidDevice.WithDetail("device " + p.ID.String() + ": name must be 1 to 128 characters")
	case len(p.Rates) == 0:
		return nil, ErrInvalidDevice.WithDetail("device " + p.ID.String() + ": at least one sample rate")
	case p.ID == shared.DeviceID{} || p.Type == DeviceType{}:
		return nil, ErrInvalidDevice.WithDetail("device id and type are required")
	}

	if p.MaxDemods <= 0 {
		p.MaxDemods = DefaultMaxDemods
	}

	d := &Device{p: p, state: StateStopped}
	d.p.Rates = slices.Clone(p.Rates)
	d.tuning = defaultTuning(p.Range, d.p.Rates)

	switch {
	case !p.Enabled:
		d.state = StateDisabled
	case !p.Type.Supported():
		d.state, d.reason = StateUnavailable, "driver_unsupported"
	}

	return d, nil
}

// ID returns the device id.
func (d *Device) ID() shared.DeviceID { return d.p.ID }

// Params returns the configuration.
func (d *Device) Params() DeviceParams {
	p := d.p
	p.Rates = slices.Clone(d.p.Rates)

	return p
}

// State returns the runtime state and its reason.
func (d *Device) State() (State, string) { return d.state, d.reason }

// Tuning returns the current capture band.
func (d *Device) Tuning() Tuning { return d.tuning }

// Listeners returns the USER demand (attached media sessions).
func (d *Device) Listeners() int { return d.listeners }

// Usable reports whether the device can run (enabled, with a driver).
func (d *Device) Usable() bool { return d.state != StateDisabled && d.state != StateUnavailable }

// Wanted reports whether the device has demand (§8.2: USER or ALWAYS_ON;
// the linger after the last listener is the manager's timer).
func (d *Device) Wanted() bool { return d.Usable() && (d.p.AlwaysOn || d.listeners > 0) }

// AddListener records a media session attached to the device.
func (d *Device) AddListener() error {
	if !d.Usable() {
		return ErrDeviceUnavailable.WithDetail("device " + d.p.ID.String() + " is " + string(d.state))
	}

	d.listeners++

	return nil
}

// RemoveListener records a detached media session.
func (d *Device) RemoveListener() {
	if d.listeners > 0 {
		d.listeners--
	}
}

// Transition moves the device to state with a reason and the retry
// attempt (retry_wait). Same-state transitions update the reason.
func (d *Device) Transition(to State, reason string, attempt int) error {
	if to != d.state && !slices.Contains(transitions[d.state], to) {
		return ErrInvalidTransition.WithDetail("device " + d.p.ID.String() + ": " + string(d.state) + " → " + string(to))
	}

	d.state, d.reason = to, reason

	switch to {
	case StateRetryWait:
		d.attempt = attempt
	case StateRunning, StateStopped:
		d.attempt = 0
	}

	return nil
}

// Retune moves the centre frequency, keeping the sample rate (§6.4
// device.retune: center_hz within the device range).
func (d *Device) Retune(center Frequency) error {
	if !d.p.Range.Contains(center) {
		return ErrOutOfRange.WithDetail("center " + fmtHz(center.hz) + " outside the device range " +
			fmtHz(d.p.Range.min.hz) + ".." + fmtHz(d.p.Range.max.hz))
	}

	d.tuning = Tuning{center: center, rate: d.tuning.rate}

	return nil
}

// SetRate changes the sample rate to one the device supports.
func (d *Device) SetRate(r SampleRate) error {
	if !containsRate(d.p.Rates, r) {
		return ErrOutOfRange.WithDetail("sample rate " + strconv.Itoa(r.v) + " not supported by the device")
	}

	d.tuning = Tuning{center: d.tuning.center, rate: r}

	return nil
}

// Snapshot is the reportable status of a device (device.state, §8.2).
type Snapshot struct {
	ID        string
	Name      string
	State     State
	Reason    string
	Attempt   int
	CenterHz  int64
	RateHz    int
	Listeners int
	// MinHz and MaxHz are the device range.
	MinHz, MaxHz      int64
	OperatorCanRetune bool
}

// Snapshot returns the status.
func (d *Device) Snapshot() Snapshot {
	return Snapshot{
		ID: d.p.ID.String(), Name: d.p.Name, State: d.state, Reason: d.reason, Attempt: d.attempt,
		CenterHz: d.tuning.center.hz, RateHz: d.tuning.rate.v, Listeners: d.listeners,
		MinHz: d.p.Range.min.hz, MaxHz: d.p.Range.max.hz, OperatorCanRetune: d.p.OperatorCanRetune,
	}
}
