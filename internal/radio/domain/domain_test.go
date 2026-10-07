package domain

import (
	"errors"
	"testing"
)

func params(t *testing.T) DeviceParams {
	t.Helper()

	typ, err := NewDeviceType(TypeRTLSDR)
	if err != nil {
		t.Fatal(err)
	}

	drv, err := NewDriver(typ, "", 1, AutoGain(), false)
	if err != nil {
		t.Fatal(err)
	}

	r, err := NewFreqRange(MustFrequency(24_000_000), MustFrequency(1_766_000_000))
	if err != nil {
		t.Fatal(err)
	}

	return DeviceParams{
		ID: MustDeviceID("vhf"), Name: "VHF", Type: typ, Enabled: true, Range: r,
		Rates: []SampleRate{MustSampleRate(2_400_000), MustSampleRate(1_024_000)}, Driver: drv,
	}
}

func TestValueObjects(t *testing.T) {
	for _, s := range []string{"", "A", "-x", "x y"} {
		if _, err := NewDeviceID(s); !errors.Is(err, ErrInvalidDeviceID) {
			t.Fatalf("device id %q: %v", s, err)
		}
	}

	if _, err := NewFrequency(0); !errors.Is(err, ErrInvalidFrequency) {
		t.Fatal(err)
	}

	if _, err := NewSampleRate(-1); !errors.Is(err, ErrInvalidSampleRate) {
		t.Fatal(err)
	}

	if _, err := NewFreqRange(MustFrequency(10), MustFrequency(10)); !errors.Is(err, ErrOutOfRange) {
		t.Fatal(err)
	}

	if _, err := NewDeviceType("soapy:sdr play"); !errors.Is(err, ErrInvalidDeviceType) {
		t.Fatal(err)
	}

	soapy, _ := NewDeviceType("soapy:sdrplay")
	tcp, _ := NewDeviceType(TypeRTLTCP)

	if soapy.Supported() || !tcp.Supported() {
		t.Fatal("supported types")
	}

	if _, err := NewDriver(tcp, "localhost", 0, AutoGain(), false); !errors.Is(err, ErrInvalidDriver) {
		t.Fatal("rtl_tcp without port accepted")
	}

	if _, err := NewDriver(tcp, "a\nb:1", 0, AutoGain(), false); !errors.Is(err, ErrInvalidDriver) {
		t.Fatal("control character accepted")
	}

	if _, err := NewDriver(tcp, "sdr.lan:1234", 2000, AutoGain(), false); !errors.Is(err, ErrInvalidDriver) {
		t.Fatal("ppm out of range accepted")
	}

	if _, err := NewGain(-3); !errors.Is(err, ErrInvalidDriver) {
		t.Fatal(err)
	}

	g, _ := NewGain(29.7)
	if g.String() != "29.7" || AutoGain().String() != "auto" {
		t.Fatal(g.String())
	}
}

func TestDeviceLifecycleAndDemand(t *testing.T) {
	d, err := NewDevice(params(t))
	if err != nil {
		t.Fatal(err)
	}

	tu := d.Tuning()
	if tu.Center().Hz() != 24_000_000+1_200_000 || tu.Rate().PerSecond() != 2_400_000 || tu.Start() != 24_000_000 {
		t.Fatalf("default tuning %+v", tu)
	}

	if !tu.ContainsOffset(1_200_000) || tu.ContainsOffset(1_200_001) {
		t.Fatal("offset bounds")
	}

	if d.Wanted() {
		t.Fatal("wanted without demand")
	}

	if err := d.AddListener(); err != nil || !d.Wanted() {
		t.Fatal(err)
	}

	for _, s := range []State{StateStarting, StateRetryWait, StateStarting, StateRunning, StateStopping, StateStopped} {
		if err := d.Transition(s, "r", 2); err != nil {
			t.Fatal(err)
		}
	}

	if err := d.Transition(StateRunning, "", 0); !errors.Is(err, ErrInvalidTransition) {
		t.Fatal("stopped → running accepted")
	}

	d.RemoveListener()
	d.RemoveListener()

	if d.Listeners() != 0 || d.Wanted() {
		t.Fatal("demand")
	}

	if err := d.Retune(MustFrequency(10)); !errors.Is(err, ErrOutOfRange) {
		t.Fatal(err)
	}

	if err := d.Retune(MustFrequency(145_000_000)); err != nil || d.Snapshot().CenterHz != 145_000_000 {
		t.Fatal(err)
	}

	if err := d.SetRate(MustSampleRate(250_000)); !errors.Is(err, ErrOutOfRange) {
		t.Fatal(err)
	}

	p := params(t)
	p.Enabled = false
	off, _ := NewDevice(p)

	if st, _ := off.State(); st != StateDisabled || off.AddListener() == nil {
		t.Fatal("disabled device")
	}

	p = params(t)
	p.Type, _ = NewDeviceType("soapy:sdrplay")
	un, _ := NewDevice(p)

	if st, reason := un.State(); st != StateUnavailable || reason != "driver_unsupported" {
		t.Fatal(st, reason)
	}

	p = params(t)
	p.Rates = nil

	if _, err := NewDevice(p); !errors.Is(err, ErrInvalidDevice) {
		t.Fatal(err)
	}

	// A range narrower than the rate centres the band.
	p = params(t)
	p.Range, _ = NewFreqRange(MustFrequency(100_000_000), MustFrequency(101_000_000))
	narrow, _ := NewDevice(p)

	if narrow.Tuning().Center().Hz() != 100_500_000 {
		t.Fatal(narrow.Tuning().Center().Hz())
	}

	p = params(t)
	p.AlwaysOn = true
	on, _ := NewDevice(p)

	if !on.Wanted() {
		t.Fatal("always_on not wanted")
	}
}
