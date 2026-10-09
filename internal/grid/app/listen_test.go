package app_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// scriptedDevices answers each List call with the next step: its devices
// or error, after its gate (when set) opens.
type scriptedDevices struct {
	mu      sync.Mutex
	steps   []listStep
	entered chan int
}

type listStep struct {
	devices []*domain.Device
	err     error
	gate    chan struct{}
}

func (s *scriptedDevices) List(context.Context) ([]*domain.Device, error) {
	s.mu.Lock()
	step := s.steps[0]
	if len(s.steps) > 1 {
		s.steps = s.steps[1:]
	}
	s.mu.Unlock()

	if s.entered != nil {
		s.entered <- len(step.devices)
	}

	if step.gate != nil {
		<-step.gate
	}

	return step.devices, step.err
}

func listenDevice(t *testing.T, id, policy string) *domain.Device {
	t.Helper()

	d, err := domain.NewReportedDevice(domain.MustNodeID("n1"), domain.DeviceSpec{
		ID: shared.MustDeviceID(id), Name: id, Type: "rtl_sdr", Enabled: true, FreqMin: 1, FreqMax: 2,
		SampleRates: []int64{1}, ListenPolicy: policy,
	}, 0, time.Unix(1_800_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}

	return d
}

// TestListenPoliciesFailedReloadDenies: a reload that fails drops the view,
// so the readers fail closed until a load succeeds again.
func TestListenPoliciesFailedReloadDenies(t *testing.T) {
	ctx := context.Background()
	open := []*domain.Device{listenDevice(t, "open", domain.ListenAnonymous)}
	devices := &scriptedDevices{steps: []listStep{{devices: open}, {err: errors.New("db down")}, {err: errors.New("db down")}, {devices: open}}}
	p := app.NewListenPolicies(devices, fixedPolicy{v: domain.ListenRegistered}, nil)

	if ok, err := p.CanListen(ctx, true, "open"); err != nil || !ok {
		t.Fatalf("CanListen before = %v, %v", ok, err)
	}

	if _, err := p.Refresh(ctx); err == nil {
		t.Fatal("Refresh succeeded on a failing registry")
	}

	if ok, err := p.CanListen(ctx, true, "open"); err == nil || ok {
		t.Errorf("CanListen after a failed reload = %v, %v; want a denial with the error", ok, err)
	}

	if ok, err := p.CanListen(ctx, true, "open"); err != nil || !ok {
		t.Errorf("CanListen once the registry is back = %v, %v", ok, err)
	}
}

// TestListenPoliciesNoStaleReload: an older reload still running when a
// newer one starts never replaces the newer view.
func TestListenPoliciesNoStaleReload(t *testing.T) {
	ctx := context.Background()
	gate := make(chan struct{})
	devices := &scriptedDevices{
		entered: make(chan int, 2),
		steps: []listStep{
			{devices: []*domain.Device{listenDevice(t, "hf", domain.ListenAnonymous)}, gate: gate},
			{devices: []*domain.Device{listenDevice(t, "hf", domain.ListenRegistered)}},
		},
	}
	p := app.NewListenPolicies(devices, fixedPolicy{v: domain.ListenAnonymous}, nil)

	var wg sync.WaitGroup

	wg.Go(func() { _, _ = p.Refresh(ctx) }) // the older load, held at its gate
	<-devices.entered

	wg.Go(func() { _, _ = p.Refresh(ctx) }) // the newer load

	// Give the newer load the time to finish if it could run beside the
	// older one, then let the older one complete.
	select {
	case <-devices.entered:
	case <-time.After(50 * time.Millisecond):
	}

	close(gate)
	wg.Wait()

	v, err := p.View(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if lp, _ := v.Policy("hf"); lp != domain.ListenRegistered {
		t.Errorf("policy of hf = %q, want the newer registered", lp)
	}
}
