package wire

import (
	"context"
	"testing"
	"time"

	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/presets"
	"github.com/yohang/mesh-sdr/internal/schedules"
	"github.com/yohang/mesh-sdr/internal/shared/audit"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// TestDesiredStateEndToEnd pushes the desired state of a real node over
// the control channel (ADR 0020): the node answers every revision, a new
// preset and schedule change the revision, and removing the node disables
// its schedules (GRID-016).
func TestDesiredStateEndToEnd(t *testing.T) {
	e := newGridEnv(t, fastTimings())

	devices := DevicesOf(e.nodeCfg)
	for i := range devices {
		devices[i].SchedulerEnabled = true
	}

	e.enrollNode(t, fakeProber{devices: devices})

	ctx := context.Background()
	id := domain.MustNodeID("attic")

	applied := func() (gridapp.LinkState, bool) {
		link, ok := e.g.tracker.State(id)

		return link, ok && link.StateRevision != 0 && link.AppliedRevision == link.StateRevision && len(link.StateErrors) == 0
	}

	eventually(t, "device registry", 15*time.Second, func() bool {
		_, err := e.g.devices.Get(ctx, "hf")

		return err == nil
	})
	eventually(t, "first desired state applied", 10*time.Second, func() bool { _, ok := applied(); return ok })

	first, _ := applied()

	// A preset that fits hf and a schedule using it.
	now := time.Now()
	ids := shared.NewUUIDv7Generator()
	pid, _ := ids.New(now)

	spec, err := presets.NewSpec(presets.Draft{Name: "20 m", CenterFreq: 14_074_000, SampRate: 2_048_000})
	if err != nil {
		t.Fatal(err)
	}

	p, _ := presets.NewPreset(pid, spec, 0, now)
	if err := presets.NewPresets(e.adapter).Create(ctx, p); err != nil {
		t.Fatal(err)
	}

	sid, _ := ids.New(now)

	sspec, err := schedules.NewSpec(schedules.Draft{
		DeviceID: "hf", PresetID: pid.String(), StartMinute: new(0), EndMinute: new(720),
	})
	if err != nil {
		t.Fatal(err)
	}

	sc, _ := schedules.NewSchedule(sid, sspec, now)
	repo := schedules.NewSchedules(e.adapter)

	if err := repo.Create(ctx, sc); err != nil {
		t.Fatal(err)
	}

	if n := e.g.states.PublishAll(ctx); n != 1 {
		t.Errorf("pushed to %d nodes", n)
	}

	eventually(t, "second desired state applied", 10*time.Second, func() bool {
		link, ok := applied()

		return ok && link.StateRevision != first.StateRevision
	})

	// Removing the node removes hf: its schedule is disabled, not deleted.
	if err := e.g.nodes.Delete(ctx, audit.CLI, "attic"); err != nil {
		t.Fatal(err)
	}

	got, err := repo.Get(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}

	if reason, _ := got.DisabledReason(); got.Enabled() || reason != schedules.ReasonDeviceRemoved {
		t.Errorf("schedule after the node removal = %+v", got.Snapshot())
	}
}
