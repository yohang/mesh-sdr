package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
)

func device(id, typ string) ctl.Device {
	return ctl.Device{ID: id, Name: id + " name", Type: typ, Enabled: true, FreqMin: 1_000, FreqMax: 30_000_000, SampleRates: []int64{2_000_000}}
}

func TestDeviceRegistrySync(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	repo := sqlite.NewDeviceRepository(e.db)
	s := app.NewDevices(repo, e.audit, discard)

	attic := enrolledNode(t, e)

	garden := domain.NewNode(domain.MustNodeID("garden"), domain.MustNodeName("g"), domain.MustNodeURL("https://g:1"), e.clock.now())
	if err := e.nodes.Create(ctx, garden); err != nil {
		t.Fatal(err)
	}

	report := func(n *domain.Node, devices ...ctl.Device) {
		t.Helper()

		err := e.db.WithinTx(ctx, func(ctx context.Context) error {
			return s.Sync(ctx, n, ctl.Capabilities{Devices: devices}, e.clock.now())
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	report(attic, device("hf", "soapy:sdrplay"), device("vhf", "rtl_sdr"))

	// garden claims hf: refused, its own device registers.
	report(garden, device("hf", "rtl_sdr"), device("uhf", "rtl_sdr"))

	hf, err := s.Get(ctx, "hf")
	if err != nil || hf.Node() != attic.ID() || hf.Type() != "soapy:sdrplay" || hf.SortOrder() != 0 {
		t.Fatalf("hf = %+v, %v", hf, err)
	}

	if uhf, err := s.Get(ctx, "uhf"); err != nil || uhf.Node() != garden.ID() {
		t.Fatalf("uhf = %+v, %v", uhf, err)
	}

	if got := e.audit.actions(); len(got) != 1 || got[0] != "device.register:hf" {
		t.Errorf("audit = %v", got)
	}

	// A type change of an existing id is refused too; vhf disappears.
	report(attic, device("hf", "rtl_sdr"))

	if hf, _ := s.Get(ctx, "hf"); hf.Type() != "soapy:sdrplay" {
		t.Errorf("type changed to %s", hf.Type())
	}

	vhf, _ := s.Get(ctx, "vhf")
	if st, _, reason := vhf.State(); st != domain.StateUnavailable || reason != "not_reported" || vhf.Online() {
		t.Errorf("vhf = %s %s", st, reason)
	}

	// device.state updates the runtime columns; offline nodes take their devices offline.
	c, _ := newControl(e, "1.0.0")
	c.Handle(rxv1.TypeDeviceState, s.StateHandler())
	boot := welcome(t, c, attic.ID(), "1.0.0")

	payload, _ := json.Marshal(ctl.DeviceState{Seq: 1, DeviceID: "hf", State: "running", CenterFreq: ptr(int64(14_074_000))})
	if _, err := c.Apply(ctx, attic.ID(), boot, false, []app.Event{{Seq: 1, Type: rxv1.TypeDeviceState, Payload: payload}}); err != nil {
		t.Fatal(err)
	}

	hf, _ = s.Get(ctx, "hf")
	if st, _, _ := hf.State(); st != domain.StateRunning || !hf.Online() || *hf.CenterFreq() != 14_074_000 {
		t.Errorf("hf after state = %+v", hf.Snapshot())
	}

	s.NodeStatusChanged(ctx, attic.ID(), domain.StatusOffline, "")

	if hf, _ := s.Get(ctx, "hf"); hf.Online() {
		t.Error("device of an offline node still online")
	}

	if _, err := s.Get(ctx, "nope"); !errors.Is(err, domain.ErrDeviceNotFound) {
		t.Errorf("unknown device = %v", err)
	}

	list, _ := s.List(ctx)
	if len(list) != 3 {
		t.Errorf("registry = %d devices", len(list))
	}

	// Forget (ADM-009): only a device its node no longer reports.
	if _, missing := hf.Missing(); missing {
		t.Error("hf of an offline node reported missing")
	}

	if err := s.Forget(ctx, app.ActorUser, "hf"); !errors.Is(err, domain.ErrDeviceStillReported) {
		t.Errorf("forget a reported device = %v", err)
	}

	if err := s.Forget(ctx, app.ActorUser, "vhf"); err != nil {
		t.Fatalf("forget vhf: %v", err)
	}

	if _, err := s.Get(ctx, "vhf"); !errors.Is(err, domain.ErrDeviceNotFound) {
		t.Errorf("vhf after forget = %v", err)
	}

	if err := s.Forget(ctx, app.ActorUser, "nope"); !errors.Is(err, domain.ErrDeviceNotFound) {
		t.Errorf("forget an unknown device = %v", err)
	}

	if got := e.audit.actions(); got[len(got)-1] != "device.forget:vhf" {
		t.Errorf("audit = %v", got)
	}

	// Devices go away with their node.
	if err := e.nodes.Delete(ctx, garden.ID()); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Get(ctx, "uhf"); !errors.Is(err, domain.ErrDeviceNotFound) {
		t.Errorf("uhf after node delete = %v", err)
	}

}

func ptr[T any](v T) *T { return &v }
