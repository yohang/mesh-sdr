package app

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/yohang/mesh-sdr/internal/schedules/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// DevicePlan is the desired state of one device (§4.4 ctl.state.apply
// devices.<id>): the presets that fit it, the preset to start with, and
// its schedule timeline.
type DevicePlan struct {
	Device Device
	// Presets fit the device, by sort order.
	Presets []shared.UUID
	// Start is the device's active preset when it still fits, otherwise
	// the first compatible preset (§7.1); zero when none fits.
	Start    shared.UUID
	Timeline domain.Timeline
}

// Planner computes the desired state of the devices of a node. Schedules
// apply only to devices whose node config sets scheduler_enabled (§7.1)
// and that their node still reports; a schedule whose preset does not fit
// is skipped (the guard disables it).
type Planner struct{ d Deps }

// NewPlanner returns the planner.
func NewPlanner(d Deps) *Planner { return &Planner{d: d} }

// Plan returns the plans of the devices of node for the timeline starting
// at the hour of now.
func (p *Planner) Plan(ctx context.Context, node string, now time.Time) ([]DevicePlan, error) {
	devices, err := p.d.Devices.NodeDevices(ctx, node)
	if err != nil {
		return nil, err
	}

	from := domain.TimelineStart(now)
	until := from.Add(domain.Horizon)
	out := make([]DevicePlan, 0, len(devices))

	for _, dev := range devices {
		plan := DevicePlan{Device: dev, Timeline: domain.Evaluate(nil, from, until)}

		if plan.Presets, err = p.d.Presets.Compatible(ctx, dev); err != nil {
			return nil, err
		}

		switch {
		case !dev.ActivePreset.IsZero() && slices.Contains(plan.Presets, dev.ActivePreset):
			plan.Start = dev.ActivePreset
		case len(plan.Presets) > 0:
			plan.Start = plan.Presets[0]
		}

		if dev.SchedulerEnabled && !dev.Stale {
			if plan.Timeline, err = p.timeline(ctx, dev, plan.Presets, from, until); err != nil {
				return nil, err
			}
		}

		out = append(out, plan)
	}

	return out, nil
}

func (p *Planner) timeline(ctx context.Context, dev Device, fits []shared.UUID, from, until time.Time) (domain.Timeline, error) {
	id, err := shared.NewDeviceID(dev.ID)
	if err != nil {
		return domain.Evaluate(nil, from, until), nil //nolint:nilerr // no schedule names an invalid id
	}

	list, err := p.d.Repo.ListByDevice(ctx, id)
	if err != nil {
		return domain.Timeline{}, err
	}

	usable := list[:0:0]

	for _, sc := range list {
		if !sc.Enabled() {
			continue
		}

		if !slices.Contains(fits, sc.Preset()) {
			p.d.Logger.DebugContext(ctx, "schedule skipped: its preset does not fit the device",
				slog.String("schedule_id", sc.ID().String()), slog.String("device_id", dev.ID))

			continue
		}

		usable = append(usable, sc)
	}

	return domain.Evaluate(usable, from, until), nil
}
