package app

import (
	"context"
	"log/slog"

	"github.com/yohang/mesh-sdr/internal/schedules/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Guard disables the enabled schedules that their device or preset no
// longer allows (GRID-016, ADM-009, ADR 0020 Q10–Q12): device stale,
// device removed, preset no longer fitting the device. Each disable is
// audited (system actor) in the transaction of the caller; an admin
// re-enables the schedule. Its methods join the caller's transaction.
type Guard struct{ d Deps }

// NewGuard returns the guard.
func NewGuard(d Deps) *Guard { return &Guard{d: d} }

// DevicesStale disables the schedules of devices their node no longer
// reports.
func (g *Guard) DevicesStale(ctx context.Context, devices []shared.DeviceID) error {
	return g.disableAll(ctx, devices, domain.ReasonDeviceStale)
}

// DevicesRemoved disables the schedules of devices deleted from the
// registry (forgotten, or removed with their node).
func (g *Guard) DevicesRemoved(ctx context.Context, devices []shared.DeviceID) error {
	return g.disableAll(ctx, devices, domain.ReasonDeviceRemoved)
}

func (g *Guard) disableAll(ctx context.Context, devices []shared.DeviceID, reason domain.DisabledReason) error {
	for _, id := range devices {
		list, err := g.d.Repo.ListByDevice(ctx, id)
		if err != nil {
			return err
		}

		for _, sc := range list {
			if _, err := g.disable(ctx, sc, reason, ""); err != nil {
				return err
			}
		}
	}

	return nil
}

// DeviceReported re-checks the enabled schedules of a device its node
// reported (its limits may have changed): a preset that no longer fits
// disables its schedules.
func (g *Guard) DeviceReported(ctx context.Context, dev Device) error {
	id, err := shared.NewDeviceID(dev.ID)
	if err != nil {
		return nil //nolint:nilerr // no schedule can name an invalid id
	}

	list, err := g.d.Repo.ListByDevice(ctx, id)
	if err != nil {
		return err
	}

	for _, sc := range list {
		if _, err := g.recheck(ctx, sc, dev); err != nil {
			return err
		}
	}

	return nil
}

// PresetReplaced re-checks the enabled schedules of a preset that changed
// and returns those it disabled.
func (g *Guard) PresetReplaced(ctx context.Context, preset shared.UUID) ([]shared.UUID, error) {
	list, err := g.d.Repo.ListByPreset(ctx, preset)
	if err != nil {
		return nil, err
	}

	var out []shared.UUID

	for _, sc := range list {
		if !sc.Enabled() {
			continue
		}

		dev, ok, err := g.d.Devices.Device(ctx, sc.Device().String())
		if err != nil {
			return nil, err
		}

		var changed bool

		if !ok {
			changed, err = g.disable(ctx, sc, domain.ReasonDeviceRemoved, "")
		} else {
			changed, err = g.recheck(ctx, sc, dev)
		}

		if err != nil {
			return nil, err
		}

		if changed {
			out = append(out, sc.ID())
		}
	}

	return out, nil
}

// Reconcile checks every enabled schedule against the registry and the
// presets, as a safety net for changes made outside the hub process (the
// CLI removing a node). It returns the number of schedules disabled.
func (g *Guard) Reconcile(ctx context.Context) (int64, error) {
	var n int64

	err := g.d.Tx.WithinTx(ctx, func(ctx context.Context) error {
		n = 0

		list, err := g.d.Repo.List(ctx)
		if err != nil {
			return err
		}

		for _, sc := range list {
			if !sc.Enabled() {
				continue
			}

			dev, ok, err := g.d.Devices.Device(ctx, sc.Device().String())
			if err != nil {
				return err
			}

			var changed bool

			if !ok {
				changed, err = g.disable(ctx, sc, domain.ReasonDeviceRemoved, "")
			} else {
				changed, err = g.recheck(ctx, sc, dev)
			}

			if err != nil {
				return err
			}

			if changed {
				n++
			}
		}

		return nil
	})

	return n, err
}

// recheck disables an enabled schedule whose device is stale or whose
// preset does not fit the device any more.
func (g *Guard) recheck(ctx context.Context, sc *domain.Schedule, dev Device) (bool, error) {
	if !sc.Enabled() {
		return false, nil
	}

	if dev.Stale {
		return g.disable(ctx, sc, domain.ReasonDeviceStale, "")
	}

	fit, err := g.d.Presets.Fit(ctx, sc.Preset(), dev)
	if err != nil {
		return false, err
	}

	if !fit.Exists || fit.Reason != "" {
		return g.disable(ctx, sc, domain.ReasonPresetIncompatible, fit.Reason)
	}

	return false, nil
}

func (g *Guard) disable(ctx context.Context, sc *domain.Schedule, reason domain.DisabledReason, detail string) (bool, error) {
	v := sc.Version()
	before := auditFields(sc)

	if !sc.Disable(reason, g.d.Now()) {
		return false, nil
	}

	if err := g.d.Repo.Update(ctx, sc, v); err != nil {
		return false, err
	}

	after := auditFields(sc)
	if detail != "" {
		after["reason_detail"] = detail
	}

	if err := g.d.Audit.Record(ctx, AuditRecord{System: true, Action: ActionDisable, Target: sc.ID(), Before: before, After: after}); err != nil {
		return false, err
	}

	g.d.Logger.InfoContext(ctx, "schedule disabled", slog.String("schedule_id", sc.ID().String()),
		slog.String("device_id", sc.Device().String()), slog.String("reason", string(reason)))

	return true, nil
}
