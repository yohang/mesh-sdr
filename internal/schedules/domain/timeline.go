package domain

import (
	"slices"
	"time"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Horizon is how far ahead a timeline goes past its start (§8.5: the next
// 24 h). The start is the current hour, so a timeline holds at least 24 h
// and stays the same within the hour (ADR 0020 Q6).
const Horizon = 25 * time.Hour

// TimelineStart is the start of the timeline computed at now: the start of
// the current UTC hour.
func TimelineStart(now time.Time) time.Time { return now.UTC().Truncate(time.Hour) }

// Slot is one interval [From, Until) of a timeline and its preset.
type Slot struct {
	From, Until time.Time
	Preset      shared.UUID
	Schedule    shared.UUID
}

// Timeline is the desired preset of a device per time interval (§8.5).
// Outside its slots no scheduled preset applies.
type Timeline struct {
	from, until time.Time
	slots       []Slot
}

// From returns the start of the timeline.
func (t Timeline) From() time.Time { return t.from }

// Until returns the end of the timeline.
func (t Timeline) Until() time.Time { return t.until }

// Slots returns the slots, in time order, without overlap.
func (t Timeline) Slots() []Slot { return slices.Clone(t.slots) }

// occurrence is one occurrence of a schedule window.
type occurrence struct {
	from, until time.Time
	start       time.Time // unclipped start, for overlaps
	s           *Schedule
}

// Evaluate computes the timeline of the enabled static schedules of one
// device over [from, until) (§8.5). Windows are UTC; days_of_week applies
// to the day a window starts, so an overnight window belongs to its start
// day. Overlaps go to the highest priority, then the earliest start, then
// the lowest schedule id. Daylight entries are not evaluated yet (SVC-011).
func Evaluate(schedules []*Schedule, from, until time.Time) Timeline {
	from, until = from.UTC(), until.UTC()
	t := Timeline{from: from, until: until}

	if !until.After(from) {
		return t
	}

	var occ []occurrence

	first := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)

	for _, s := range schedules {
		if !s.Enabled() || s.Window().Kind() != KindStatic {
			continue
		}

		start, end := s.Window().Minutes()

		for day := first; day.Before(until); day = day.AddDate(0, 0, 1) {
			if !s.Days().Has(day.Weekday()) {
				continue
			}

			a := day.Add(time.Duration(start) * time.Minute)

			b := day.Add(time.Duration(end) * time.Minute)
			if end < start {
				b = b.Add(24 * time.Hour)
			}

			if lo, hi := maxTime(a, from), minTime(b, until); lo.Before(hi) {
				occ = append(occ, occurrence{from: lo, until: hi, start: a, s: s})
			}
		}
	}

	if len(occ) == 0 {
		return t
	}

	cuts := make([]time.Time, 0, 2*len(occ))
	for _, o := range occ {
		cuts = append(cuts, o.from, o.until)
	}

	slices.SortFunc(cuts, func(a, b time.Time) int { return a.Compare(b) })
	cuts = slices.CompactFunc(cuts, func(a, b time.Time) bool { return a.Equal(b) })

	for i := 0; i+1 < len(cuts); i++ {
		a, b := cuts[i], cuts[i+1]

		var win *occurrence

		for j := range occ {
			o := &occ[j]
			if o.from.After(a) || !o.until.After(a) {
				continue
			}

			if win == nil || better(o, win) {
				win = o
			}
		}

		if win == nil {
			continue
		}

		if n := len(t.slots); n > 0 && t.slots[n-1].Until.Equal(a) && t.slots[n-1].Schedule == win.s.ID() {
			t.slots[n-1].Until = b

			continue
		}

		t.slots = append(t.slots, Slot{From: a, Until: b, Preset: win.s.Preset(), Schedule: win.s.ID()})
	}

	return t
}

// better reports whether o wins over w on an overlap.
func better(o, w *occurrence) bool {
	if p, q := o.s.Priority().Int(), w.s.Priority().Int(); p != q {
		return p > q
	}

	if !o.start.Equal(w.start) {
		return o.start.Before(w.start)
	}

	return o.s.ID().Compare(w.s.ID()) < 0
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}

	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}

	return b
}
