package domain

import (
	"context"
	"errors"
	"time"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Draft is the raw input of a schedule (API), before validation. Nil
// pointers are absent fields.
type Draft struct {
	DeviceID      string
	PresetID      string
	Kind          string // empty: static
	StartMinute   *int
	EndMinute     *int
	DaysOfWeek    *int // nil: every day
	DaylightPhase string
	Priority      *int  // nil: 0
	Enabled       *bool // nil: true
}

// Spec is a validated schedule definition.
type Spec struct {
	device   DeviceID
	preset   shared.UUID
	window   Window
	days     DaysOfWeek
	priority Priority
	enabled  bool
}

// NewSpec validates a draft; every invalid field is reported. Daylight
// windows are refused until SVC-011 (ADR 0020 Q3).
func NewSpec(d Draft) (Spec, error) {
	var (
		s   Spec
		bad []shared.Violation
	)

	collect := func(err error) {
		var de *shared.Error
		if errors.As(err, &de) {
			bad = append(bad, de.Violations()...)
		}
	}

	var err error

	if s.device, err = NewDeviceID(d.DeviceID); err != nil {
		collect(err)
	}

	if s.preset, err = shared.ParseUUID(d.PresetID); err != nil {
		bad = append(bad, shared.NewViolation("preset_id", "invalid_preset_id", "a preset id (UUID)"))
	}

	switch Kind(d.Kind) {
	case "", KindStatic:
		switch {
		case d.StartMinute == nil:
			bad = append(bad, shared.NewViolation("start_minute", "required", "required for a static window"))
		case d.EndMinute == nil:
			bad = append(bad, shared.NewViolation("end_minute", "required", "required for a static window"))
		default:
			if s.window, err = NewStaticWindow(*d.StartMinute, *d.EndMinute); err != nil {
				collect(err)
			}
		}
	case KindDaylight:
		return Spec{}, ErrKindUnsupported
	default:
		bad = append(bad, shared.NewViolation("kind", "invalid_kind", "static or daylight"))
	}

	days := EveryDay
	if d.DaysOfWeek != nil {
		days = *d.DaysOfWeek
	}

	if s.days, err = NewDaysOfWeek(days); err != nil {
		collect(err)
	}

	prio := 0
	if d.Priority != nil {
		prio = *d.Priority
	}

	if s.priority, err = NewPriority(prio); err != nil {
		collect(err)
	}

	s.enabled = d.Enabled == nil || *d.Enabled

	if len(bad) > 0 {
		return Spec{}, ErrInvalidSchedule.WithViolations(bad...)
	}

	return s, nil
}

// Device returns the device of the spec.
func (s Spec) Device() DeviceID { return s.device }

// Preset returns the preset of the spec.
func (s Spec) Preset() shared.UUID { return s.preset }

// Enabled reports whether the spec asks for an enabled schedule.
func (s Spec) Enabled() bool { return s.enabled }

// Schedule is one schedule entry (§7.1 schedules). An enabled schedule is
// evaluated by the hub; the hub disables it with a reason when its device
// goes stale or away, or when its preset no longer fits (GRID-016).
type Schedule struct {
	id         shared.UUID
	spec       Spec
	reason     DisabledReason
	disabledAt time.Time
	createdAt  time.Time
	updatedAt  time.Time
	version    int
}

// NewSchedule creates a schedule.
func NewSchedule(id shared.UUID, spec Spec, now time.Time) (*Schedule, error) {
	if id.IsZero() {
		return nil, ErrInvalidSchedule.WithDetail("invalid schedule id")
	}

	now = ms(now)

	return &Schedule{id: id, spec: spec, createdAt: now, updatedAt: now, version: 1}, nil
}

func ms(t time.Time) time.Time { return t.UTC().Truncate(time.Millisecond) }

// Replace replaces the definition (PUT) when expectedVersion is current.
// Enabling clears the reason the hub disabled it for; a schedule kept
// disabled keeps its reason, so it stays flagged.
func (s *Schedule) Replace(spec Spec, expectedVersion int, now time.Time) error {
	if expectedVersion != s.version {
		return ErrVersionConflict
	}

	s.spec = spec

	if spec.enabled {
		s.reason, s.disabledAt = ReasonNone, time.Time{}
	}

	s.touch(now)

	return nil
}

// Disable disables an enabled schedule for reason (a system change). A
// schedule the hub already disabled takes device_removed over another
// reason; one an admin disabled is left alone. It reports whether the
// schedule changed.
func (s *Schedule) Disable(reason DisabledReason, now time.Time) bool {
	switch {
	case reason == ReasonNone:
		return false
	case !s.spec.enabled && (s.reason == ReasonNone || s.reason == reason || reason != ReasonDeviceRemoved):
		return false
	}

	s.spec.enabled = false
	s.reason, s.disabledAt = reason, ms(now)
	s.touch(now)

	return true
}

func (s *Schedule) touch(now time.Time) {
	s.updatedAt = ms(now)
	s.version++
}

// ID returns the id.
func (s *Schedule) ID() shared.UUID { return s.id }

// Device returns the device.
func (s *Schedule) Device() DeviceID { return s.spec.device }

// Preset returns the preset.
func (s *Schedule) Preset() shared.UUID { return s.spec.preset }

// Window returns the time window.
func (s *Schedule) Window() Window { return s.spec.window }

// Days returns the week days.
func (s *Schedule) Days() DaysOfWeek { return s.spec.days }

// Priority returns the priority.
func (s *Schedule) Priority() Priority { return s.spec.priority }

// Enabled reports whether the schedule is evaluated.
func (s *Schedule) Enabled() bool { return s.spec.enabled }

// DisabledReason returns why the hub disabled the schedule, and when
// (ReasonNone when it did not).
func (s *Schedule) DisabledReason() (DisabledReason, time.Time) { return s.reason, s.disabledAt }

// CreatedAt returns the creation time.
func (s *Schedule) CreatedAt() time.Time { return s.createdAt }

// UpdatedAt returns the time of the last change.
func (s *Schedule) UpdatedAt() time.Time { return s.updatedAt }

// Version returns the optimistic concurrency version.
func (s *Schedule) Version() int { return s.version }

// Snapshot is the persisted form of a schedule.
type Snapshot struct {
	ID                   shared.UUID
	Device               string
	Preset               shared.UUID
	Kind                 Kind
	Start, End           *int
	Phase                Phase
	Days                 int
	Priority             int
	Enabled              bool
	Reason               DisabledReason
	DisabledAt           time.Time
	CreatedAt, UpdatedAt time.Time
	Version              int
}

// Snapshot returns the persisted form.
func (s *Schedule) Snapshot() Snapshot {
	out := Snapshot{
		ID: s.id, Device: s.spec.device.value, Preset: s.spec.preset, Kind: s.spec.window.kind, Phase: s.spec.window.phase,
		Days: s.spec.days.mask, Priority: s.spec.priority.value, Enabled: s.spec.enabled, Reason: s.reason,
		DisabledAt: s.disabledAt, CreatedAt: s.createdAt, UpdatedAt: s.updatedAt, Version: s.version,
	}

	if s.spec.window.kind == KindStatic {
		out.Start, out.End = new(s.spec.window.start), new(s.spec.window.end)
	}

	return out
}

// Rehydrate rebuilds a stored schedule. Daylight rows (written by a later
// version or the migration tool) are kept as they are.
func Rehydrate(s Snapshot) (*Schedule, error) {
	device, err := NewDeviceID(s.Device)
	if err != nil {
		return nil, err
	}

	var w Window

	switch s.Kind {
	case KindStatic:
		if s.Start == nil || s.End == nil {
			return nil, ErrInvalidSchedule.WithDetail("static schedule without minutes")
		}

		w, err = NewStaticWindow(*s.Start, *s.End)
	case KindDaylight:
		w, err = NewDaylightWindow(s.Phase)
	default:
		err = ErrInvalidSchedule.WithDetail("unknown schedule kind")
	}

	if err != nil {
		return nil, err
	}

	days, err := NewDaysOfWeek(s.Days)
	if err != nil {
		return nil, err
	}

	prio, err := NewPriority(s.Priority)
	if err != nil {
		return nil, err
	}

	if s.ID.IsZero() || s.Preset.IsZero() || s.Version < 1 || (s.Reason != ReasonNone && s.Enabled) {
		return nil, ErrInvalidSchedule.WithDetail("invalid stored schedule")
	}

	return &Schedule{
		id:     s.ID,
		spec:   Spec{device: device, preset: s.Preset, window: w, days: days, priority: prio, enabled: s.Enabled},
		reason: s.Reason, disabledAt: s.DisabledAt, createdAt: s.CreatedAt, updatedAt: s.UpdatedAt, version: s.Version,
	}, nil
}

// Repository persists schedules.
type Repository interface {
	Get(ctx context.Context, id shared.UUID) (*Schedule, error)
	// List returns every schedule by device, then start.
	List(ctx context.Context) ([]*Schedule, error)
	ListByDevice(ctx context.Context, device DeviceID) ([]*Schedule, error)
	ListByPreset(ctx context.Context, preset shared.UUID) ([]*Schedule, error)
	Create(ctx context.Context, s *Schedule) error
	// Update writes s when the stored version is expectedVersion
	// (ErrVersionConflict otherwise).
	Update(ctx context.Context, s *Schedule, expectedVersion int) error
	Delete(ctx context.Context, id shared.UUID) error
}
