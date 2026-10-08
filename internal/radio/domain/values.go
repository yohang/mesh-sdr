// Package domain models the SDR devices of a node (TECHNICAL_SPEC §8.2):
// their configuration, tuning and runtime lifecycle. It is pure: no I/O,
// no logging.
package domain

import (
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Radio domain errors. Codes are part of the public API.
var (
	ErrInvalidFrequency  = shared.NewError(shared.KindInvalid, "invalid_frequency", "invalid frequency")
	ErrInvalidSampleRate = shared.NewError(shared.KindInvalid, "invalid_sample_rate", "invalid sample rate")
	ErrInvalidDeviceType = shared.NewError(shared.KindInvalid, "invalid_device_type", "invalid device type")
	ErrInvalidDriver     = shared.NewError(shared.KindInvalid, "invalid_driver", "invalid driver settings")
	ErrInvalidDevice     = shared.NewError(shared.KindInvalid, "invalid_device", "invalid device")
	ErrOutOfRange        = shared.NewError(shared.KindInvalid, "out_of_range", "value out of range")
	ErrDeviceNotFound    = shared.NewError(shared.KindNotFound, "device_not_found", "device not found")
	ErrDeviceUnavailable = shared.NewError(shared.KindUnavailable, "device_unavailable", "the device is not available")
	ErrInvalidTransition = shared.NewError(shared.KindConflict, "invalid_device_transition", "invalid device state transition")
	ErrUnsupportedMode   = shared.NewError(shared.KindInvalid, "unsupported_mode", "demodulation mode not supported by this node")
	ErrCapacityExceeded  = shared.NewError(shared.KindConflict, "capacity_exceeded", "demodulator capacity exceeded")
)

// MaxFrequency bounds frequencies (100 GHz).
const MaxFrequency = 100_000_000_000

// Frequency is a frequency in Hz.
type Frequency struct{ hz int64 }

// NewFrequency validates hz (0 < hz ≤ 100 GHz).
func NewFrequency(hz int64) (Frequency, error) {
	if hz <= 0 || hz > MaxFrequency {
		return Frequency{}, ErrInvalidFrequency.WithDetail("invalid frequency " + strconv.FormatInt(hz, 10) + " Hz")
	}

	return Frequency{hz: hz}, nil
}

// MustFrequency is NewFrequency that panics. Tests and constants only.
func MustFrequency(hz int64) Frequency {
	f, err := NewFrequency(hz)
	if err != nil {
		panic(err)
	}

	return f
}

// Hz returns the value.
func (f Frequency) Hz() int64 { return f.hz }

// MaxSampleRate bounds device sample rates (§2.3 reference hardware B
// stops at 10 MS/s; 100 MS/s leaves room).
const MaxSampleRate = 100_000_000

// SampleRate is a device sample rate in samples per second.
type SampleRate struct{ v int }

// NewSampleRate validates v.
func NewSampleRate(v int64) (SampleRate, error) {
	if v <= 0 || v > MaxSampleRate {
		return SampleRate{}, ErrInvalidSampleRate.WithDetail("invalid sample rate " + strconv.FormatInt(v, 10))
	}

	return SampleRate{v: int(v)}, nil
}

// MustSampleRate is NewSampleRate that panics. Tests and constants only.
func MustSampleRate(v int64) SampleRate {
	r, err := NewSampleRate(v)
	if err != nil {
		panic(err)
	}

	return r
}

// PerSecond returns the value.
func (r SampleRate) PerSecond() int { return r.v }

// FreqRange is a tunable range.
type FreqRange struct{ min, max Frequency }

// NewFreqRange validates min < max.
func NewFreqRange(lo, hi Frequency) (FreqRange, error) {
	if lo.hz >= hi.hz {
		return FreqRange{}, ErrOutOfRange.WithDetail("frequency range: want min < max")
	}

	return FreqRange{min: lo, max: hi}, nil
}

// Min returns the lowest frequency.
func (r FreqRange) Min() Frequency { return r.min }

// Max returns the highest frequency.
func (r FreqRange) Max() Frequency { return r.max }

// Contains reports whether f lies in the range.
func (r FreqRange) Contains(f Frequency) bool { return f.hz >= r.min.hz && f.hz <= r.max.hz }

// DeviceType is the driver type of a device (§8.2 "Driver strategy").
type DeviceType struct{ value string }

// Device types this node can run.
const (
	TypeRTLSDR = "rtl_sdr"
	TypeRTLTCP = "rtl_tcp"
)

// deviceTypes is the SDR type registry (SRC-001): every type this node
// runs, with the owrx_connector tool that runs it (§8.2). Other types are
// valid in the config but unavailable on this node.
var deviceTypes = map[string]string{
	TypeRTLSDR: "rtl_connector",
	TypeRTLTCP: "rtl_tcp_connector",
}

// SupportedTypes returns the registered device types, sorted.
func SupportedTypes() []DeviceType {
	out := make([]DeviceType, 0, len(deviceTypes))
	for _, t := range slices.Sorted(maps.Keys(deviceTypes)) {
		out = append(out, DeviceType{value: t})
	}

	return out
}

var deviceTypePattern = regexp.MustCompile(`^[a-z0-9_]{1,24}(:[a-z0-9_]{1,23})?$`)

// NewDeviceType validates the syntax of s. Supported tells whether this
// node has a driver for it.
func NewDeviceType(s string) (DeviceType, error) {
	if !deviceTypePattern.MatchString(s) {
		return DeviceType{}, ErrInvalidDeviceType.WithDetail("invalid device type " + strconv.Quote(s))
	}

	return DeviceType{value: s}, nil
}

// String returns the type.
func (t DeviceType) String() string { return t.value }

// Supported reports whether the type is in the registry.
func (t DeviceType) Supported() bool {
	_, ok := deviceTypes[t.value]

	return ok
}

// Tool returns the connector tool of a supported type ("" otherwise).
func (t DeviceType) Tool() string { return deviceTypes[t.value] }

// driverDevicePattern bounds the connector device string (§8.2 rule 2): a
// serial, an index or host:port, no control characters.
var driverDevicePattern = regexp.MustCompile(`^[A-Za-z0-9._:=-]{1,64}$`)

// Gain is the RF gain: automatic or a value in dB.
type Gain struct {
	auto bool
	db   float64
}

// AutoGain is the automatic gain.
func AutoGain() Gain { return Gain{auto: true} }

// NewGain validates a manual gain (0..100 dB).
func NewGain(db float64) (Gain, error) {
	if math.IsNaN(db) || db < 0 || db > 100 {
		return Gain{}, ErrInvalidDriver.WithDetail("rf_gain: want auto or 0..100 dB")
	}

	return Gain{db: db}, nil
}

// Auto reports whether the gain is automatic.
func (g Gain) Auto() bool { return g.auto }

// String returns the connector form: "auto" or the dB value.
func (g Gain) String() string {
	if g.auto {
		return "auto"
	}

	return strconv.FormatFloat(g.db, 'f', -1, 64)
}

// DirectSampling is the direct sampling input of an RTL-SDR (off, I or Q
// branch), for HF reception without an upconverter.
type DirectSampling int

// Direct sampling inputs, in the connector numbering (-e 0|1|2).
const (
	DirectSamplingOff DirectSampling = iota
	DirectSamplingI
	DirectSamplingQ
)

// ParseDirectSampling parses "off" (or ""), "i" or "q".
func ParseDirectSampling(s string) (DirectSampling, error) {
	switch s {
	case "", "off":
		return DirectSamplingOff, nil
	case "i":
		return DirectSamplingI, nil
	case "q":
		return DirectSamplingQ, nil
	default:
		return 0, ErrInvalidDriver.WithDetail("driver.direct_sampling: want off, i or q")
	}
}

// DriverSettings are the raw driver keys of a device (devices.<id>.driver).
type DriverSettings struct {
	// Device is the connector device (rtl_sdr: index or serial, default 0;
	// rtl_tcp: host:port).
	Device         string
	PPM            int
	Gain           Gain
	IQSwap         bool
	BiasTee        bool
	DirectSampling DirectSampling
	// LFOOffset is added to the centre frequency to tune the hardware (Hz,
	// signed): the local oscillator of an up- or downconverter.
	LFOOffset int64
}

// Driver is the validated driver table of a device (devices.<id>.driver).
type Driver struct{ s DriverSettings }

// NewDriver validates the driver settings of a device of type t.
func NewDriver(t DeviceType, s DriverSettings) (Driver, error) {
	if s.Device == "" && t.value == TypeRTLSDR {
		s.Device = "0"
	}

	if !driverDevicePattern.MatchString(s.Device) {
		return Driver{}, ErrInvalidDriver.WithDetail("driver.device: want 1..64 characters of [A-Za-z0-9._:=-]")
	}

	if t.value == TypeRTLTCP {
		host, port, ok := strings.Cut(s.Device, ":")
		if p, err := strconv.Atoi(port); !ok || host == "" || err != nil || p < 1 || p > 65535 {
			return Driver{}, ErrInvalidDriver.WithDetail("driver.device: rtl_tcp needs host:port")
		}
	}

	if s.PPM < -1000 || s.PPM > 1000 {
		return Driver{}, ErrInvalidDriver.WithDetail("driver.ppm: want -1000..1000")
	}

	if s.DirectSampling < DirectSamplingOff || s.DirectSampling > DirectSamplingQ {
		return Driver{}, ErrInvalidDriver.WithDetail("driver.direct_sampling: want off, i or q")
	}

	if s.LFOOffset < -MaxFrequency || s.LFOOffset > MaxFrequency {
		return Driver{}, ErrInvalidDriver.WithDetail("driver.lfo_offset: want -100 GHz..100 GHz")
	}

	return Driver{s: s}, nil
}

// Device returns the connector device string.
func (d Driver) Device() string { return d.s.Device }

// PPM returns the frequency correction.
func (d Driver) PPM() int { return d.s.PPM }

// Gain returns the RF gain.
func (d Driver) Gain() Gain { return d.s.Gain }

// IQSwap reports whether I and Q are swapped.
func (d Driver) IQSwap() bool { return d.s.IQSwap }

// BiasTee reports whether the bias-tee is powered.
func (d Driver) BiasTee() bool { return d.s.BiasTee }

// DirectSampling returns the direct sampling input.
func (d Driver) DirectSampling() DirectSampling { return d.s.DirectSampling }

// LFOOffset returns the converter local oscillator offset in Hz.
func (d Driver) LFOOffset() int64 { return d.s.LFOOffset }

// HardwareHz returns the frequency the hardware tunes to for a centre
// frequency: centre + lfo_offset.
func (d Driver) HardwareHz(center int64) int64 { return center + d.s.LFOOffset }

// Tuning is the capture band of a running device.
type Tuning struct {
	center Frequency
	rate   SampleRate
}

// NewTuning returns a tuning.
func NewTuning(center Frequency, rate SampleRate) Tuning { return Tuning{center: center, rate: rate} }

// Center returns the centre frequency.
func (t Tuning) Center() Frequency { return t.center }

// Rate returns the sample rate.
func (t Tuning) Rate() SampleRate { return t.rate }

// Start returns the lowest frequency of the capture band.
func (t Tuning) Start() int64 { return t.center.hz - int64(t.rate.v)/2 }

// ContainsOffset reports whether offsetHz from the centre is inside the
// capture band (§6.9: ±sample_rate/2).
func (t Tuning) ContainsOffset(offsetHz int64) bool {
	half := int64(t.rate.v) / 2

	return offsetHz >= -half && offsetHz <= half
}

// initialTuning is the tuning of a device at start, until presets come
// from the hub: the configured centre and sample rate, otherwise the first
// sample rate with the capture band at the bottom of the range (centred
// when the range is narrower, ADR 0019).
func initialTuning(r FreqRange, rates []SampleRate, center Frequency, rate SampleRate) Tuning {
	if rate.v == 0 {
		rate = rates[0]
	}

	if center.hz != 0 {
		return Tuning{center: center, rate: rate}
	}

	half := int64(rate.v) / 2
	hz := r.min.hz + half

	if r.max.hz-r.min.hz < 2*half || hz > r.max.hz {
		hz = r.min.hz + (r.max.hz-r.min.hz)/2
	}

	return Tuning{center: Frequency{hz: hz}, rate: rate}
}

func containsRate(rates []SampleRate, r SampleRate) bool {
	return slices.Contains(rates, r)
}

func fmtHz(hz int64) string { return fmt.Sprintf("%d Hz", hz) }
