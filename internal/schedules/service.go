// Package schedules models schedules (TECHNICAL_SPEC §7.1 `schedules`,
// §8.5, ADR 0020): (device, preset, time window) entries evaluated by the hub
// into a per-device timeline. It holds the admin CRUD, the guard that
// disables schedules whose device or preset no longer allows them (GRID-016,
// ADM-009), the planner that computes each device's desired state for the
// control channel, and the SQLite repository.
package schedules

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/yohang/mesh-sdr/internal/db"

	"github.com/yohang/mesh-sdr/internal/shared/audit"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Deps are the dependencies of the schedule services. Changed, when set,
// runs after every committed change (the desired state of the nodes is
// pushed again).
type Deps struct {
	Repo    *Schedules
	Tx      *db.DB
	Devices Devices
	Presets Presets
	Audit   audit.Appender
	IDs     *shared.UUIDv7Generator
	Now     Clock
	Changed func(ctx context.Context)
	Logger  *slog.Logger
}

// Service is the admin use cases of schedules (§6.10 /schedules).
type Service struct{ d Deps }

// NewService returns the service.
func NewService(d Deps) *Service { return &Service{d: d} }

// ParseID parses a schedule id from a path (ErrScheduleNotFound when
// invalid).
func ParseID(s string) (shared.UUID, error) {
	id, err := shared.ParseUUID(s)
	if err != nil {
		return shared.UUID{}, ErrScheduleNotFound
	}

	return id, nil
}

// List returns every schedule.
func (s *Service) List(ctx context.Context) ([]*Schedule, error) { return s.d.Repo.List(ctx) }

// ForDevice returns the schedules of a device.
func (s *Service) ForDevice(ctx context.Context, device string) ([]*Schedule, error) {
	id, err := shared.NewDeviceID(device)
	if err != nil {
		return nil, nil //nolint:nilerr // an invalid id has no schedule
	}

	return s.d.Repo.ListByDevice(ctx, id)
}

// SchedulesUsing returns the ids of the schedules that reference a preset.
func (s *Service) SchedulesUsing(ctx context.Context, preset shared.UUID) ([]shared.UUID, error) {
	list, err := s.d.Repo.ListByPreset(ctx, preset)
	if err != nil {
		return nil, err
	}

	out := make([]shared.UUID, len(list))
	for i, sc := range list {
		out[i] = sc.ID()
	}

	return out, nil
}

// Get returns one schedule.
func (s *Service) Get(ctx context.Context, id string) (*Schedule, error) {
	sid, err := ParseID(id)
	if err != nil {
		return nil, err
	}

	return s.d.Repo.Get(ctx, sid)
}

// Create validates and stores a schedule (§6.10 POST /schedules).
func (s *Service) Create(ctx context.Context, d Draft) (*Schedule, error) {
	spec, err := NewSpec(d)
	if err != nil {
		return nil, err
	}

	now := s.d.Now()

	id, err := s.d.IDs.New(now)
	if err != nil {
		return nil, fmt.Errorf("schedule id: %w", err)
	}

	var sc *Schedule

	err = s.d.Tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := s.check(ctx, spec, nil); err != nil {
			return err
		}

		if sc, err = NewSchedule(id, spec, now); err != nil {
			return err
		}

		if err := s.d.Repo.Create(ctx, sc); err != nil {
			return err
		}

		return s.d.Audit.Append(ctx, audit.Record{Action: ActionCreate, TargetType: "schedule", TargetID: id.String(), After: auditFields(sc)})
	})
	if err != nil {
		return nil, err
	}

	s.changed(ctx)

	return sc, nil
}

// Replace replaces a schedule (PUT) when expectedVersion is current.
// Enabling a schedule the hub disabled re-validates it.
func (s *Service) Replace(ctx context.Context, id string, expectedVersion int, d Draft) (*Schedule, error) {
	sid, err := ParseID(id)
	if err != nil {
		return nil, err
	}

	spec, err := NewSpec(d)
	if err != nil {
		return nil, err
	}

	var sc *Schedule

	err = s.d.Tx.WithinTx(ctx, func(ctx context.Context) error {
		if sc, err = s.d.Repo.Get(ctx, sid); err != nil {
			return err
		}

		before := auditFields(sc)

		if err := s.check(ctx, spec, sc); err != nil {
			return err
		}

		if err := sc.Replace(spec, expectedVersion, s.d.Now()); err != nil {
			return err
		}

		if err := s.d.Repo.Update(ctx, sc, expectedVersion); err != nil {
			return err
		}

		return s.d.Audit.Append(ctx, audit.Record{Action: ActionUpdate, TargetType: "schedule", TargetID: sid.String(), Before: before, After: auditFields(sc)})
	})
	if err != nil {
		return nil, err
	}

	s.changed(ctx)

	return sc, nil
}

// Delete deletes a schedule.
func (s *Service) Delete(ctx context.Context, id string) error {
	sid, err := ParseID(id)
	if err != nil {
		return err
	}

	err = s.d.Tx.WithinTx(ctx, func(ctx context.Context) error {
		sc, err := s.d.Repo.Get(ctx, sid)
		if err != nil {
			return err
		}

		if err := s.d.Repo.Delete(ctx, sid); err != nil {
			return err
		}

		return s.d.Audit.Append(ctx, audit.Record{Action: ActionDelete, TargetType: "schedule", TargetID: sid.String(), Before: auditFields(sc)})
	})
	if err != nil {
		return err
	}

	s.changed(ctx)

	return nil
}

// check validates a spec against the registry and the presets: the device
// and the preset exist; an enabled schedule also needs a device its node
// still reports and a preset that fits it (§7.1: "the check also runs on
// write"). A schedule that stays disabled on its own device may be edited
// after the device left the registry (current is the stored schedule, nil
// on creation).
func (s *Service) check(ctx context.Context, spec Spec, current *Schedule) error {
	dev, ok, err := s.d.Devices.Device(ctx, spec.Device().String())
	if err != nil {
		return err
	}

	if !ok {
		if current == nil || spec.Enabled() || spec.Device() != current.Device() {
			return ErrUnknownDevice.WithDetail("no device " + strconv.Quote(spec.Device().String()) + " in the registry")
		}

		dev = Device{ID: spec.Device().String()}
	}

	fit, err := s.d.Presets.Fit(ctx, spec.Preset(), dev)
	if err != nil {
		return err
	}

	switch {
	case !fit.Exists:
		return ErrUnknownPreset
	case !spec.Enabled():
		return nil
	case dev.Stale:
		return ErrDeviceUnavailable
	case fit.Reason != "":
		return ErrPresetIncompatible.WithDetail(fit.Reason)
	}

	return nil
}

func (s *Service) changed(ctx context.Context) {
	if s.d.Changed != nil {
		s.d.Changed(ctx)
	}
}

// auditFields is the audited form of a schedule.
func auditFields(sc *Schedule) map[string]string {
	reason, _ := sc.DisabledReason()

	out := map[string]string{
		"device_id": sc.Device().String(), "preset_id": sc.Preset().String(), "window": sc.Window().String(),
		"days_of_week": strconv.Itoa(sc.Days().Mask()), "priority": strconv.Itoa(sc.Priority().Int()),
		"enabled": strconv.FormatBool(sc.Enabled()), "version": strconv.Itoa(sc.Version()),
	}

	if reason != ReasonNone {
		out["disabled_reason"] = string(reason)
	}

	return out
}
