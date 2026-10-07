// Package presets models presets (TECHNICAL_SPEC §7.1 `presets`, ADR 0020):
// device-independent tuning data, validated against a device's reported
// limits when it is applied. It holds the admin CRUD with optimistic
// concurrency and audit, the compatibility queries used to apply presets to
// devices, and the SQLite repository.
package presets

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/yohang/mesh-sdr/internal/shared/audit"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Clock returns the current time.
type Clock func() time.Time

// IDs generates preset ids (UUIDv7).
type IDs interface {
	New(now time.Time) (shared.UUID, error)
}

// Transactor runs a unit of work in one write transaction.
type Transactor interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// Audit actions.
const (
	ActionCreate = "preset.create"
	ActionUpdate = "preset.update"
	ActionDelete = "preset.delete"
)

// Usage lists the schedules that reference a preset (ADM-020).
type Usage interface {
	SchedulesUsing(ctx context.Context, preset shared.UUID) ([]shared.UUID, error)
}

// ChangeListener is told, inside the transaction of a preset update, that
// a preset changed; it returns the schedules it disabled because the
// preset no longer fits their device (GRID-016, ADR 0020 Q12).
type ChangeListener interface {
	PresetReplaced(ctx context.Context, preset shared.UUID) ([]shared.UUID, error)
}

// Deps are the dependencies of the service. Usage and Listener are
// optional; Changed, when set, runs after every committed change (the
// desired state of the nodes is pushed again).
type Deps struct {
	Repo     Repository
	Tx       Transactor
	Audit    audit.Appender
	IDs      IDs
	Now      Clock
	Usage    Usage
	Listener ChangeListener
	Changed  func(ctx context.Context)
	Logger   *slog.Logger
}

// Service is the preset use cases.
type Service struct{ d Deps }

// NewService returns the service.
func NewService(d Deps) *Service { return &Service{d: d} }

// ParseID parses a preset id from a path (ErrPresetNotFound when invalid).
func ParseID(s string) (shared.UUID, error) {
	id, err := shared.ParseUUID(s)
	if err != nil {
		return shared.UUID{}, ErrPresetNotFound
	}

	return id, nil
}

// List returns every preset, by sort order.
func (s *Service) List(ctx context.Context) ([]*Preset, error) { return s.d.Repo.List(ctx) }

// Get returns one preset.
func (s *Service) Get(ctx context.Context, id string) (*Preset, error) {
	pid, err := ParseID(id)
	if err != nil {
		return nil, err
	}

	return s.d.Repo.Get(ctx, pid)
}

// Create validates and stores a new preset at the end of the list. A slug
// derived from the name is made unique with a numeric suffix.
func (s *Service) Create(ctx context.Context, d Draft) (*Preset, error) {
	spec, err := NewSpec(d)
	if err != nil {
		return nil, err
	}

	now := s.d.Now()

	id, err := s.d.IDs.New(now)
	if err != nil {
		return nil, fmt.Errorf("preset id: %w", err)
	}

	var p *Preset

	err = s.d.Tx.WithinTx(ctx, func(ctx context.Context) error {
		spec, err := s.uniqueSlug(ctx, spec, shared.UUID{})
		if err != nil {
			return err
		}

		order, err := s.d.Repo.NextSortOrder(ctx)
		if err != nil {
			return err
		}

		if p, err = NewPreset(id, spec, order, now); err != nil {
			return err
		}

		if err := s.d.Repo.Create(ctx, p); err != nil {
			return err
		}

		return s.d.Audit.Append(ctx, audit.Record{Action: ActionCreate, TargetType: "preset", TargetID: id.String(), After: auditFields(p)})
	})
	if err != nil {
		return nil, err
	}

	s.changed(ctx)

	return p, nil
}

// uniqueSlug checks the slug of spec, or finds a free one when it was
// derived from the name.
func (s *Service) uniqueSlug(ctx context.Context, spec Spec, except shared.UUID) (Spec, error) {
	slug, derived := spec.Slug()

	for n := 2; ; n++ {
		taken, err := s.d.Repo.SlugTaken(ctx, slug, except)
		if err != nil {
			return Spec{}, err
		}

		switch {
		case !taken:
			return spec.WithSlug(slug), nil
		case !derived:
			return Spec{}, ErrSlugTaken
		case n > 1000:
			return Spec{}, ErrSlugTaken
		}

		base, _ := spec.Slug()
		slug = SlugWithSuffix(base, n)
	}
}

// Replaced is the outcome of a preset update: the preset, and the schedules
// disabled because it no longer fits their device.
type Replaced struct {
	Preset   *Preset
	Disabled []shared.UUID
}

// Replace replaces a preset (PUT) when expectedVersion is current. The
// schedules it no longer fits are disabled in the same transaction.
func (s *Service) Replace(ctx context.Context, id string, expectedVersion int, d Draft) (Replaced, error) {
	pid, err := ParseID(id)
	if err != nil {
		return Replaced{}, err
	}

	spec, err := NewSpec(d)
	if err != nil {
		return Replaced{}, err
	}

	var out Replaced

	err = s.d.Tx.WithinTx(ctx, func(ctx context.Context) error {
		out = Replaced{}

		p, err := s.d.Repo.Get(ctx, pid)
		if err != nil {
			return err
		}

		before := auditFields(p)

		if _, derived := spec.Slug(); !derived {
			if spec, err = s.uniqueSlug(ctx, spec, pid); err != nil {
				return err
			}
		}

		if err := p.Replace(spec, expectedVersion, s.d.Now()); err != nil {
			return err
		}

		if err := s.d.Repo.Update(ctx, p, expectedVersion); err != nil {
			return err
		}

		if s.d.Listener != nil {
			if out.Disabled, err = s.d.Listener.PresetReplaced(ctx, pid); err != nil {
				return err
			}
		}

		out.Preset = p

		return s.d.Audit.Append(ctx, audit.Record{Action: ActionUpdate, TargetType: "preset", TargetID: pid.String(), Before: before, After: auditFields(p)})
	})
	if err != nil {
		return Replaced{}, err
	}

	s.changed(ctx)

	return out, nil
}

// Delete deletes a preset no schedule references (ADM-020). Devices on
// which it is active keep their tuning.
func (s *Service) Delete(ctx context.Context, id string) error {
	pid, err := ParseID(id)
	if err != nil {
		return err
	}

	err = s.d.Tx.WithinTx(ctx, func(ctx context.Context) error {
		p, err := s.d.Repo.Get(ctx, pid)
		if err != nil {
			return err
		}

		if s.d.Usage != nil {
			used, err := s.d.Usage.SchedulesUsing(ctx, pid)
			if err != nil {
				return err
			}

			if len(used) > 0 {
				ids := make([]string, len(used))
				for i, u := range used {
					ids[i] = u.String()
				}

				return ErrPresetInUse.WithDetail("fix or delete the schedules that use this preset first: " + strings.Join(ids, ", "))
			}
		}

		if err := s.d.Repo.Delete(ctx, pid); err != nil {
			return err
		}

		return s.d.Audit.Append(ctx, audit.Record{Action: ActionDelete, TargetType: "preset", TargetID: pid.String(), Before: auditFields(p)})
	})
	if err != nil {
		return err
	}

	s.changed(ctx)

	return nil
}

func (s *Service) changed(ctx context.Context) {
	if s.d.Changed != nil {
		s.d.Changed(ctx)
	}
}

// Compatible returns the presets that fit a device, by sort order (§6.10
// `GET /presets?device_id=`).
func (s *Service) Compatible(ctx context.Context, limits DeviceLimits) ([]*Preset, error) {
	all, err := s.d.Repo.List(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]*Preset, 0, len(all))

	for _, p := range all {
		if p.Fits(limits) == nil {
			out = append(out, p)
		}
	}

	return out, nil
}

// Check reports whether a preset exists and fits a device: nil,
// ErrPresetNotFound or ErrPresetIncompatible.
func (s *Service) Check(ctx context.Context, id shared.UUID, limits DeviceLimits) error {
	p, err := s.d.Repo.Get(ctx, id)
	if err != nil {
		return err
	}

	return p.Fits(limits)
}

// Exists reports whether a preset exists.
func (s *Service) Exists(ctx context.Context, id shared.UUID) (bool, error) {
	_, err := s.d.Repo.Get(ctx, id)

	switch {
	case errors.Is(err, ErrPresetNotFound):
		return false, nil
	case err != nil:
		return false, err
	}

	return true, nil
}

// auditFields is the audited form of a preset.
func auditFields(p *Preset) map[string]string {
	s := p.Snapshot()
	i64 := func(v int64) string { return strconv.FormatInt(v, 10) }

	out := map[string]string{
		"slug": s.Slug, "name": s.Name, "center_freq": i64(s.CenterFreq), "samp_rate": i64(s.SampRate),
		"start_freq": i64(s.StartFreq), "start_mod": s.StartMod, "tuning_step": i64(s.TuningStep),
		"tags": strings.Join(s.Tags, ","), "version": strconv.Itoa(s.Version),
	}

	if s.Description != "" {
		out["description"] = s.Description
	}

	if s.Squelch != nil {
		out["initial_squelch_level"] = strconv.Itoa(*s.Squelch)
	}

	if s.NR != nil {
		out["initial_nr_level"] = strconv.Itoa(*s.NR)
	}

	if s.Waterfall != nil {
		out["waterfall_levels"] = strconv.Itoa(s.Waterfall[0]) + ".." + strconv.Itoa(s.Waterfall[1])
	}

	return out
}
