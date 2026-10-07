package schedules

import (
	"strconv"
	"strings"
	"time"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Kind is the kind of window of a schedule.
type Kind string

// Window kinds.
const (
	KindStatic   Kind = "static"
	KindDaylight Kind = "daylight"
)

// Phase is a daylight period (SVC-011).
type Phase string

// Daylight phases.
const (
	PhaseDay      Phase = "day"
	PhaseNight    Phase = "night"
	PhaseGreyline Phase = "greyline"
)

// MinutesPerDay is the number of minutes of a UTC day.
const MinutesPerDay = 1440

// Window is the time window of a schedule: a static UTC slot of minutes of
// the day (end before start wraps over midnight), or a daylight phase.
type Window struct {
	kind       Kind
	start, end int
	phase      Phase
}

// NewStaticWindow validates a UTC slot [start, end) in minutes of the day.
// end < start wraps over midnight; start = end is the whole day, from
// start to the same minute of the next day.
func NewStaticWindow(start, end int) (Window, error) {
	switch {
	case start < 0 || start >= MinutesPerDay:
		return Window{}, ErrInvalidSchedule.WithViolations(shared.NewViolation("start_minute", "out_of_range", "a minute of the day, 0 to 1439 (UTC)"))
	case end < 0 || end >= MinutesPerDay:
		return Window{}, ErrInvalidSchedule.WithViolations(shared.NewViolation("end_minute", "out_of_range", "a minute of the day, 0 to 1439 (UTC)"))
	}

	return Window{kind: KindStatic, start: start, end: end}, nil
}

// NewDaylightWindow validates a daylight phase.
func NewDaylightWindow(p Phase) (Window, error) {
	switch p {
	case PhaseDay, PhaseNight, PhaseGreyline:
		return Window{kind: KindDaylight, phase: p}, nil
	}

	return Window{}, ErrInvalidSchedule.WithViolations(shared.NewViolation("daylight_phase", "invalid_phase", "day, night or greyline"))
}

// Kind returns the kind of window.
func (w Window) Kind() Kind { return w.kind }

// Minutes returns the static slot (start, end) in minutes of the UTC day.
func (w Window) Minutes() (int, int) { return w.start, w.end }

// Phase returns the daylight phase.
func (w Window) Phase() Phase { return w.phase }

// Wraps reports whether a static slot wraps over midnight (a whole-day
// slot included).
func (w Window) Wraps() bool { return w.kind == KindStatic && w.end <= w.start }

// WholeDay reports whether a static slot lasts 24 h (start = end).
func (w Window) WholeDay() bool { return w.kind == KindStatic && w.end == w.start }

// String formats a static slot as HHMM-HHMM, or the daylight phase.
func (w Window) String() string {
	if w.kind == KindDaylight {
		return string(w.phase)
	}

	hhmm := func(m int) string {
		s := strconv.Itoa(m/60*100 + m%60)
		for len(s) < 4 {
			s = "0" + s
		}

		return s
	}

	if w.WholeDay() {
		return "24 h from " + hhmm(w.start) + " UTC"
	}

	return hhmm(w.start) + "-" + hhmm(w.end) + " UTC"
}

// DaysOfWeek is a bitmask of week days, Monday = bit 0 (§7.1).
type DaysOfWeek struct{ mask int }

// EveryDay is the mask of the seven days.
const EveryDay = 127

// NewDaysOfWeek validates a non-empty mask.
func NewDaysOfWeek(mask int) (DaysOfWeek, error) {
	if mask < 1 || mask > EveryDay {
		return DaysOfWeek{}, ErrInvalidSchedule.WithViolations(shared.NewViolation("days_of_week", "out_of_range",
			"a bitmask of the week days, Monday = 1, 1 to 127"))
	}

	return DaysOfWeek{mask: mask}, nil
}

// Mask returns the bitmask.
func (d DaysOfWeek) Mask() int { return d.mask }

// String formats the days for people ("Every day", "Mon, Tue").
func (d DaysOfWeek) String() string {
	if d.mask == EveryDay {
		return "Every day"
	}

	names := []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}

	var out []string

	for i, n := range names {
		if d.mask&(1<<i) != 0 {
			out = append(out, n)
		}
	}

	return strings.Join(out, ", ")
}

// Has reports whether day is set.
func (d DaysOfWeek) Has(day time.Weekday) bool {
	return d.mask&(1<<((int(day)+6)%7)) != 0
}

// Priority resolves overlapping windows: the highest wins (§7.1).
type Priority struct{ value int }

// NewPriority validates a priority in the 16-bit range.
func NewPriority(v int) (Priority, error) {
	if v < -32768 || v > 32767 {
		return Priority{}, ErrInvalidSchedule.WithViolations(shared.NewViolation("priority", "out_of_range", "-32768 to 32767"))
	}

	return Priority{value: v}, nil
}

// Int returns the priority.
func (p Priority) Int() int { return p.value }

// DisabledReason tells why the hub disabled a schedule (GRID-016,
// ADM-009). An admin re-enables it.
type DisabledReason string

// Disabled reasons.
const (
	ReasonNone               DisabledReason = ""
	ReasonDeviceStale        DisabledReason = "device_stale"
	ReasonDeviceRemoved      DisabledReason = "device_removed"
	ReasonPresetIncompatible DisabledReason = "preset_incompatible"
)

// ParseDisabledReason validates a stored reason.
func ParseDisabledReason(s string) (DisabledReason, error) {
	switch r := DisabledReason(s); r {
	case ReasonNone, ReasonDeviceStale, ReasonDeviceRemoved, ReasonPresetIncompatible:
		return r, nil
	}

	return "", ErrInvalidSchedule.WithDetail("unknown disabled reason " + strconv.Quote(s))
}
