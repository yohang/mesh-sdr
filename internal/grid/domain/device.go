package domain

import (
	"context"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// RuntimeState is the last device state reported by its node (§7.1).
type RuntimeState string

// Device runtime states.
const (
	StateUnavailable RuntimeState = "unavailable"
	StateDisabled    RuntimeState = "disabled"
	StateStopped     RuntimeState = "stopped"
	StateStarting    RuntimeState = "starting"
	StateRunning     RuntimeState = "running"
	StateRetuning    RuntimeState = "retuning"
	StateStopping    RuntimeState = "stopping"
	StateRetryWait   RuntimeState = "retry_wait"
	StateFailed      RuntimeState = "failed"
)

var runtimeStates = []RuntimeState{StateUnavailable, StateDisabled, StateStopped, StateStarting, StateRunning, StateRetuning, StateStopping, StateRetryWait, StateFailed}

// ParseRuntimeState maps a device.state value (§4.4) to the registry
// state; "offline" maps to unavailable.
func ParseRuntimeState(s string) (RuntimeState, error) {
	if s == "offline" {
		return StateUnavailable, nil
	}

	if st := RuntimeState(s); slices.Contains(runtimeStates, st) {
		return st, nil
	}

	return "", ErrInvalidDevice.WithDetail("unknown device state " + strconv.Quote(s))
}

// Listen policies (§7.4 listen_policy): who may listen to a device,
// globally or as a device's node config override.
const (
	ListenAnonymous  = "anonymous"
	ListenRegistered = "registered"
)

// DeviceSpec is the device definition reported from the node config.
type DeviceSpec struct {
	ID                shared.DeviceID
	Name              string
	Type              string
	Enabled           bool
	FreqMin, FreqMax  int64
	SampleRates       []int64
	ListenPolicy      string
	OperatorCanRetune bool
	AlwaysOn          bool
	SchedulerEnabled  bool
	// Config are the reported driver values; nil when the node does not
	// report them.
	Config *DeviceConfig
	// Position is the reported position of the device (MAP-007); nil when
	// its node config sets none.
	Position *Position
}

// Position is the position of a device (MAP-007), in decimal degrees (WGS
// 84): the device's own (devices.<id>.gps) or its node's (node.gps).
type Position struct {
	lat, lon float64
	own      bool
}

// NewPosition validates a position; own tells whether it is the device's
// own position rather than its node's.
func NewPosition(lat, lon float64, own bool) (Position, error) {
	if math.IsNaN(lat) || math.IsNaN(lon) || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return Position{}, ErrInvalidDevice.WithDetail("invalid position")
	}

	return Position{lat: lat, lon: lon, own: own}, nil
}

// Lat returns the latitude.
func (p Position) Lat() float64 { return p.lat }

// Lon returns the longitude.
func (p Position) Lon() float64 { return p.lon }

// Own reports whether the position is the device's own, not its node's.
func (p Position) Own() bool { return p.own }

// DeviceConfig are the driver values of a device in its node config
// (SRC-022: rf_gain, ppm, bias_tee, direct_sampling, iqswap, lfo_offset),
// mirrored for display only: the hub never changes them.
type DeviceConfig struct {
	// RFGain is "auto" or a value in dB.
	RFGain  string `json:"rf_gain"`
	PPM     int    `json:"ppm"`
	BiasTee bool   `json:"bias_tee"`
	// DirectSampling is off, i or q.
	DirectSampling string `json:"direct_sampling"`
	IQSwap         bool   `json:"iqswap"`
	// LFOOffset is the converter offset in Hz.
	LFOOffset int64 `json:"lfo_offset"`
}

// maxPPM bounds the reported ppm (the node config allows ±1000).
const maxPPM = 1000

func (c DeviceConfig) validate() bool {
	if c.RFGain != "auto" {
		db, err := strconv.ParseFloat(c.RFGain, 64)
		if err != nil || math.IsNaN(db) || math.IsInf(db, 0) {
			return false
		}
	}

	return c.PPM >= -maxPPM && c.PPM <= maxPPM && slices.Contains([]string{"off", "i", "q"}, c.DirectSampling)
}

// cleanText keeps reported text plain (§7.1): valid UTF-8, no C0 controls.
func cleanText(s string, max int) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}

		return r
	}, s)

	for utf8.RuneCountInString(s) > max {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}

	return s
}

func (s DeviceSpec) validate() error {
	if s.ID.String() == "" || strings.TrimSpace(s.Name) == "" || s.Type == "" || len(s.Type) > 48 ||
		s.FreqMin <= 0 || s.FreqMax <= s.FreqMin || len(s.SampleRates) == 0 {
		return ErrInvalidDevice.WithDetail("invalid definition of device " + strconv.Quote(s.ID.String()))
	}

	if s.ListenPolicy != "" && s.ListenPolicy != ListenAnonymous && s.ListenPolicy != ListenRegistered {
		return ErrInvalidDevice.WithDetail("invalid listen_policy of device " + strconv.Quote(s.ID.String()))
	}

	if s.Config != nil && !s.Config.validate() {
		return ErrInvalidDevice.WithDetail("invalid driver config of device " + strconv.Quote(s.ID.String()))
	}

	return nil
}

// Device is the read-only registry entry of a device, mirrored from its
// node (§7.1 devices). It holds no device settings.
type Device struct {
	id          shared.DeviceID
	node        NodeID
	name        string
	typ         string
	freqMin     int64
	freqMax     int64
	sampleRates []int64
	flags       DeviceFlags
	config      *DeviceConfig
	position    *Position
	online      bool
	state       RuntimeState
	stateAt     time.Time
	reason      string
	preset      shared.UUID
	centerFreq  *int64
	sortOrder   int
	reportedAt  time.Time
}

// DeviceFlags mirrors the node-config policy, for display and token
// scoping only (§7.1 devices.capabilities).
type DeviceFlags struct {
	Enabled           bool   `json:"enabled"`
	ListenPolicy      string `json:"listen_policy,omitempty"`
	OperatorCanRetune bool   `json:"operator_can_retune"`
	AlwaysOn          bool   `json:"always_on"`
	SchedulerEnabled  bool   `json:"scheduler_enabled"`
}

// NewReportedDevice registers a device first reported by node.
func NewReportedDevice(node NodeID, spec DeviceSpec, sortOrder int, now time.Time) (*Device, error) {
	d := &Device{id: spec.ID, node: node, state: StateStopped, stateAt: ms(now)}
	if err := d.ApplySpec(node, spec, sortOrder, now); err != nil {
		return nil, err
	}

	return d, nil
}

// ApplySpec updates the device from a new report of node. Another node's
// device, or a type change, is a device_id_conflict (ADR 0008 Q24).
func (d *Device) ApplySpec(node NodeID, spec DeviceSpec, sortOrder int, now time.Time) error {
	if err := spec.validate(); err != nil {
		return err
	}

	if d.node != node {
		return ErrDeviceIDConflict.WithDetail("device " + d.id.String() + " belongs to node " + d.node.String())
	}

	if d.typ != "" && d.typ != spec.Type {
		return ErrDeviceIDConflict.WithDetail("device " + d.id.String() + " is a " + d.typ + ", not a " + spec.Type)
	}

	d.name = cleanText(spec.Name, 128)
	d.typ = spec.Type
	d.freqMin, d.freqMax = spec.FreqMin, spec.FreqMax
	d.sampleRates = slices.Clone(spec.SampleRates)
	d.flags = DeviceFlags{
		Enabled: spec.Enabled, ListenPolicy: spec.ListenPolicy, OperatorCanRetune: spec.OperatorCanRetune,
		AlwaysOn: spec.AlwaysOn, SchedulerEnabled: spec.SchedulerEnabled,
	}
	d.config = nil
	if spec.Config != nil {
		c := *spec.Config
		d.config = &c
	}

	d.position = clonePosition(spec.Position)
	d.sortOrder = sortOrder
	d.reportedAt = ms(now)

	switch {
	case !spec.Enabled:
		d.setState(StateDisabled, "", now)
	case d.state == StateUnavailable || d.state == StateDisabled:
		d.setState(StateStopped, "", now)
	}

	return nil
}

func (d *Device) setState(s RuntimeState, reason string, now time.Time) {
	if d.state != s || d.reason != reason {
		d.stateAt = ms(now)
	}

	d.state, d.reason = s, reason
	d.online = s == StateRunning || s == StateRetuning
}

// ReasonNotReported is the runtime reason of a device that its node no
// longer reports: the device was removed from the node config (ADM-009).
const ReasonNotReported = "not_reported"

// MarkUnavailable records a device no longer reported by its node.
func (d *Device) MarkUnavailable(now time.Time) bool {
	if d.state == StateUnavailable && !d.online {
		return false
	}

	d.setState(StateUnavailable, ReasonNotReported, now)

	return true
}

// Missing reports whether the device is no longer reported by its node
// (its node reported its devices without it), and since when. Only a
// missing device can be forgotten. A device of an offline node is not
// missing.
func (d *Device) Missing() (time.Time, bool) {
	if d.state != StateUnavailable || d.reason != ReasonNotReported {
		return time.Time{}, false
	}

	return d.stateAt, true
}

// ApplyState records a device.state event.
func (d *Device) ApplyState(state RuntimeState, reason string, centerFreq *int64, preset shared.UUID, now time.Time) {
	d.setState(state, cleanText(reason, 64), now)
	d.centerFreq = centerFreq
	d.preset = preset
	d.reportedAt = ms(now)
}

// ID returns the device id.
func (d *Device) ID() shared.DeviceID { return d.id }

// Node returns the owning node.
func (d *Device) Node() NodeID { return d.node }

// Name returns the display name.
func (d *Device) Name() string { return d.name }

// Type returns the driver type.
func (d *Device) Type() string { return d.typ }

// FreqRange returns the tunable range in Hz.
func (d *Device) FreqRange() (int64, int64) { return d.freqMin, d.freqMax }

// SampleRates returns the supported sample rates.
func (d *Device) SampleRates() []int64 { return slices.Clone(d.sampleRates) }

// Flags returns the mirrored policy flags.
func (d *Device) Flags() DeviceFlags { return d.flags }

// Config returns the reported driver values; ok is false when the node
// did not report them.
func (d *Device) Config() (c DeviceConfig, ok bool) {
	if d.config == nil {
		return DeviceConfig{}, false
	}

	return *d.config, true
}

// Position returns the reported position of the device (ok false: none).
func (d *Device) Position() (Position, bool) {
	if d.position == nil {
		return Position{}, false
	}

	return *d.position, true
}

// Online reports whether the device is running on a connected node.
func (d *Device) Online() bool { return d.online }

// State returns the runtime state, its time and reason.
func (d *Device) State() (RuntimeState, time.Time, string) { return d.state, d.stateAt, d.reason }

// ActivePreset returns the last applied preset (zero if none).
func (d *Device) ActivePreset() shared.UUID { return d.preset }

// CenterFreq returns the reported centre frequency.
func (d *Device) CenterFreq() *int64 { return d.centerFreq }

// SortOrder returns the position of the device in its node config.
func (d *Device) SortOrder() int { return d.sortOrder }

// ReportedAt returns the time of the last report.
func (d *Device) ReportedAt() time.Time { return d.reportedAt }

// DeviceStatus summarises a device for admins (Admin › Nodes): ready,
// busy, failed or absent. It is derived from the reported state, the node
// health and the listeners; the state and its reason give the detail.
type DeviceStatus string

// Device statuses.
const (
	DeviceReady  DeviceStatus = "ready"
	DeviceBusy   DeviceStatus = "busy"
	DeviceFailed DeviceStatus = "failed"
	DeviceAbsent DeviceStatus = "absent"
)

// Status summarises the device: absent when its node is not up or the
// device cannot run (unavailable, disabled, no longer reported), failed
// when it failed or waits for a restart, busy while listeners are attached
// (SVC-001), ready otherwise.
func (d *Device) Status(nodeUp bool, listeners int) DeviceStatus {
	switch {
	case !nodeUp || d.state == StateUnavailable || d.state == StateDisabled:
		return DeviceAbsent
	case d.state == StateFailed || d.state == StateRetryWait:
		return DeviceFailed
	case listeners > 0:
		return DeviceBusy
	default:
		return DeviceReady
	}
}

// DeviceSnapshot is the persisted form of a Device.
type DeviceSnapshot struct {
	ID, Node, Name, Type string
	FreqMin, FreqMax     int64
	SampleRates          []int64
	Flags                DeviceFlags
	Config               *DeviceConfig
	Position             *Position
	Online               bool
	State                RuntimeState
	StateAt              time.Time
	Reason               string
	ActivePreset         shared.UUID
	CenterFreq           *int64
	SortOrder            int
	ReportedAt           time.Time
}

// Snapshot returns the persisted form.
func (d *Device) Snapshot() DeviceSnapshot {
	return DeviceSnapshot{
		ID: d.id.String(), Node: d.node.String(), Name: d.name, Type: d.typ, FreqMin: d.freqMin, FreqMax: d.freqMax,
		SampleRates: slices.Clone(d.sampleRates), Flags: d.flags, Config: cloneConfig(d.config), Position: clonePosition(d.position), Online: d.online, State: d.state,
		StateAt: d.stateAt, Reason: d.reason, ActivePreset: d.preset, CenterFreq: d.centerFreq, SortOrder: d.sortOrder, ReportedAt: d.reportedAt,
	}
}

// RehydrateDevice rebuilds a Device from its persisted form.
func RehydrateDevice(s DeviceSnapshot) (*Device, error) {
	id, err := shared.NewDeviceID(s.ID)
	if err != nil {
		return nil, err
	}

	node, err := NewNodeID(s.Node)
	if err != nil {
		return nil, err
	}

	if !slices.Contains(runtimeStates, s.State) {
		return nil, ErrInvalidDevice.WithDetail("invalid stored state of " + s.ID)
	}

	return &Device{
		id: id, node: node, name: s.Name, typ: s.Type, freqMin: s.FreqMin, freqMax: s.FreqMax,
		sampleRates: slices.Clone(s.SampleRates), flags: s.Flags, config: cloneConfig(s.Config), position: clonePosition(s.Position), online: s.Online, state: s.State,
		stateAt: s.StateAt, reason: s.Reason, preset: s.ActivePreset, centerFreq: s.CenterFreq, sortOrder: s.SortOrder, reportedAt: s.ReportedAt,
	}, nil
}

func cloneConfig(c *DeviceConfig) *DeviceConfig {
	if c == nil {
		return nil
	}

	v := *c

	return &v
}

func clonePosition(p *Position) *Position {
	if p == nil {
		return nil
	}

	v := *p

	return &v
}

// DeviceRepository persists the device registry.
type DeviceRepository interface {
	Get(ctx context.Context, id shared.DeviceID) (*Device, error)
	List(ctx context.Context) ([]*Device, error)
	ListByNode(ctx context.Context, node NodeID) ([]*Device, error)
	Save(ctx context.Context, d *Device) error
	// SetNodeOffline marks every device of node offline.
	SetNodeOffline(ctx context.Context, node NodeID) error
	// DeleteMissing deletes the device only while it is missing (see
	// Device.Missing) and reports whether it was deleted.
	DeleteMissing(ctx context.Context, id shared.DeviceID) (bool, error)
}
