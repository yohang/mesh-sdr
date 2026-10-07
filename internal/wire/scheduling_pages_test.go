package wire_test

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	griddomain "github.com/yohang/mesh-sdr/internal/grid/domain"
	gridsqlite "github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
	"github.com/yohang/mesh-sdr/internal/presets"
	"github.com/yohang/mesh-sdr/internal/schedules"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// TestDeviceSchedules: the device page lists the schedules of the device
// and flags those the hub disabled; the overview counts them; forgetting
// a device disables its schedules (ADM-009, ADR 0020).
func TestDeviceSchedules(t *testing.T) {
	h := newAdminHub(t, nil)
	seedDevices(t, h)

	ctx := context.Background()
	now := time.Now()
	ids := shared.NewUUIDv7Generator()

	pid, _ := ids.New(now)
	spec, _ := presets.NewSpec(presets.Draft{Name: "20 m FT8", CenterFreq: 14_074_000, SampRate: 2_048_000})
	p, _ := presets.NewPreset(pid, spec, 0, now)

	if err := presets.NewPresets(h.db).Create(ctx, p); err != nil {
		t.Fatal(err)
	}

	repo := schedules.NewSchedules(h.db)

	add := func(device string, start int) shared.UUID {
		t.Helper()

		id, _ := ids.New(now)

		s, err := schedules.NewSpec(schedules.Draft{
			DeviceID: device, PresetID: pid.String(), StartMinute: &start, EndMinute: new(start + 60), DaysOfWeek: new(3),
		})
		if err != nil {
			t.Fatal(err)
		}

		sc, _ := schedules.NewSchedule(id, s, now)
		if err := repo.Create(ctx, sc); err != nil {
			t.Fatal(err)
		}

		return id
	}

	hf := add("hf", 1320)
	vhf := add("vhf", 0)

	sc, _ := repo.Get(ctx, hf)
	sc.Disable(schedules.ReasonPresetIncompatible, now)

	if err := repo.Update(ctx, sc, 1); err != nil {
		t.Fatal(err)
	}

	// The node reports its active preset (device.state).
	devices := gridsqlite.NewDeviceRepository(h.db)

	dev, err := devices.Get(ctx, shared.MustDeviceID("hf"))
	if err != nil {
		t.Fatal(err)
	}

	dev.ApplyState(griddomain.StateRunning, "", nil, pid, now)

	if err := devices.Save(ctx, dev); err != nil {
		t.Fatal(err)
	}

	op := h.browser("op")

	if _, body := op.do(http.MethodGet, "/admin/devices/hf", "", "", nil); !regexp.MustCompile(`Active preset</dt>\s*<dd>\s*20 m FT8`).Match(body) {
		t.Errorf("hf page lacks its active preset: %s", body)
	}

	_, body := op.do(http.MethodGet, "/admin/devices/hf", "", "", nil)
	for _, want := range []string{"Schedules", "2200-2300 UTC", "Mon, Tue", "20 m FT8", "its preset no longer fits this device"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("hf page lacks %q: %s", want, body)
		}
	}

	admin := h.browser("root")

	if _, body := admin.do(http.MethodGet, "/admin", "", "", nil); !strings.Contains(string(body), "Schedules disabled by the hub (device gone or preset no longer fitting): ") {
		t.Errorf("overview = %s", body)
	}

	if res, _ := admin.do(http.MethodPost, "/admin/devices/vhf/forget", "", "", map[string]string{"HX-Request": "true"}); res.StatusCode != http.StatusNoContent {
		t.Fatalf("forget = %d", res.StatusCode)
	}

	got, err := repo.Get(ctx, vhf)
	if reason, _ := got.DisabledReason(); err != nil || got.Enabled() || reason != schedules.ReasonDeviceRemoved {
		t.Errorf("schedule of a forgotten device = %+v, %v", got.Snapshot(), err)
	}
}
