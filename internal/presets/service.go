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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"

	"github.com/yohang/mesh-sdr/internal/shared/audit"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Clock returns the current time.
type Clock func() time.Time

// Audit actions.
const (
	ActionCreate = "preset.create"
	ActionUpdate = "preset.update"
	ActionDelete = "preset.delete"
	ActionClone  = "preset.clone"
	ActionMove   = "preset.move"
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
	Repo     *Presets
	Tx       *db.DB
	Audit    audit.Appender
	IDs      *shared.UUIDv7Generator
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

// cloneSuffix starts the suffix of the name of a cloned preset.
const cloneSuffix = " (copy"

// cloneName returns the first free "<name> (copy)", "<name> (copy 2)"…
// within MaxNameLength characters.
func cloneName(name string, taken map[string]bool) string {
	for n := 1; ; n++ {
		suffix := cloneSuffix + ")"
		if n > 1 {
			suffix = cloneSuffix + " " + strconv.Itoa(n) + ")"
		}

		base := []rune(name)
		if room := MaxNameLength - len([]rune(suffix)); len(base) > room {
			base = []rune(strings.TrimSpace(string(base[:room])))
		}

		if c := string(base) + suffix; !taken[c] || n > 10000 {
			return c
		}
	}
}

// Clone stores a copy of a preset at the end of the list: a new id, every
// field copied, the name suffixed " (copy)" (" (copy 2)"… when taken) and a
// slug derived from it (made unique).
func (s *Service) Clone(ctx context.Context, id string) (*Preset, error) {
	pid, err := ParseID(id)
	if err != nil {
		return nil, err
	}

	now := s.d.Now()

	newID, err := s.d.IDs.New(now)
	if err != nil {
		return nil, fmt.Errorf("preset id: %w", err)
	}

	var p *Preset

	err = s.d.Tx.WithinTx(ctx, func(ctx context.Context) error {
		src, err := s.d.Repo.Get(ctx, pid)
		if err != nil {
			return err
		}

		all, err := s.d.Repo.List(ctx)
		if err != nil {
			return err
		}

		taken := make(map[string]bool, len(all))
		for _, o := range all {
			taken[o.Name()] = true
		}

		sn := src.Snapshot()

		spec, err := NewSpec(Draft{
			Name: cloneName(sn.Name, taken), Description: sn.Description, Tags: sn.Tags, CenterFreq: sn.CenterFreq,
			SampRate: sn.SampRate, StartFreq: &sn.StartFreq, StartMod: sn.StartMod, TuningStep: &sn.TuningStep,
			InitialSquelchLevel: sn.Squelch, InitialNRLevel: sn.NR, WaterfallLevels: sn.Waterfall,
		})
		if err != nil {
			return err
		}

		if spec, err = s.uniqueSlug(ctx, spec, shared.UUID{}); err != nil {
			return err
		}

		order, err := s.d.Repo.NextSortOrder(ctx)
		if err != nil {
			return err
		}

		if p, err = NewPreset(newID, spec, order, now); err != nil {
			return err
		}

		if err := s.d.Repo.Create(ctx, p); err != nil {
			return err
		}

		after := auditFields(p)
		after["cloned_from"] = pid.String()

		return s.d.Audit.Append(ctx, audit.Record{Action: ActionClone, TargetType: "preset", TargetID: newID.String(), After: after})
	})
	if err != nil {
		return nil, err
	}

	s.changed(ctx)

	return p, nil
}

// MoveBy moves a preset by delta positions (-1 up, +1 down), within the
// list. It reports whether the order changed. Like MoveTo it only renumbers
// the display positions: no preset is edited, no device is retuned, and the
// desired state of the nodes is not pushed, since the order is read when the
// presets are listed.
func (s *Service) MoveBy(ctx context.Context, id string, delta int) (bool, error) {
	return s.move(ctx, id, func(from, _ int) int { return from + delta })
}

// MoveTo moves a preset to an absolute 0-based position (clamped to the
// list), see MoveBy.
func (s *Service) MoveTo(ctx context.Context, id string, position int) (bool, error) {
	return s.move(ctx, id, func(int, int) int { return position })
}

func (s *Service) move(ctx context.Context, id string, to func(from, count int) int) (bool, error) {
	pid, err := ParseID(id)
	if err != nil {
		return false, err
	}

	moved := false

	err = s.d.Tx.WithinTx(ctx, func(ctx context.Context) error {
		moved = false

		list, err := s.d.Repo.List(ctx)
		if err != nil {
			return err
		}

		from := slices.IndexFunc(list, func(p *Preset) bool { return p.ID() == pid })
		if from < 0 {
			return ErrPresetNotFound
		}

		dest := max(0, min(to(from, len(list)), len(list)-1))

		ordered := slices.Clone(list)
		p := ordered[from]
		ordered = slices.Delete(ordered, from, from+1)
		ordered = slices.Insert(ordered, dest, p)

		changed := 0

		for i, o := range ordered {
			if o.SortOrder() == i {
				continue
			}

			if err := s.d.Repo.SetSortOrder(ctx, o.ID(), i); err != nil {
				return err
			}

			changed++
		}

		if changed == 0 {
			return nil
		}

		moved = true

		return s.d.Audit.Append(ctx, audit.Record{
			Action: ActionMove, TargetType: "preset", TargetID: pid.String(),
			Before: map[string]string{"position": strconv.Itoa(from + 1)}, After: map[string]string{"position": strconv.Itoa(dest + 1)},
		})
	})

	return moved, err
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

// Delete deletes a preset no schedule references (ADM-020) when
// expectedVersion is current. Devices on which it is active keep their
// tuning.
func (s *Service) Delete(ctx context.Context, id string, expectedVersion int) error {
	pid, err := ParseID(id)
	if err != nil {
		return err
	}

	err = s.d.Tx.WithinTx(ctx, func(ctx context.Context) error {
		p, err := s.d.Repo.Get(ctx, pid)
		if err != nil {
			return err
		}

		if p.Version() != expectedVersion {
			return ErrVersionConflict
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
