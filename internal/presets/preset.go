package presets

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"time"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Draft is the raw input of a preset (API or form), before validation.
// Zero pointers are absent optional fields.
type Draft struct {
	Slug                string // empty: derived from the name on creation
	Name                string
	Description         string
	Tags                []string
	CenterFreq          int64
	SampRate            int64
	StartFreq           *int64 // nil: the centre frequency
	StartMod            string // empty: "nfm" (§9.1 4.6)
	TuningStep          *int64 // nil: 1000 Hz (§9.1 4.6)
	InitialSquelchLevel *int
	InitialNRLevel      *int
	WaterfallLevels     *[2]int // {min, max}
}

// Defaults of the optional preset fields (FEATURE_SPEC §9.1 4.6).
const (
	DefaultStartMod   = "nfm"
	DefaultTuningStep = 1000
)

// Spec is a validated preset definition (every field but the identity).
type Spec struct {
	slug        Slug
	derivedSlug bool
	name        Name
	description string
	tags        Tags
	centerFreq  int64
	sampRate    int64
	startFreq   int64
	startMod    ModeID
	tuningStep  int64
	squelch     *int
	nr          *int
	waterfall   *WaterfallLevels
}

// NewSpec validates a draft. Every invalid field is reported, as one
// ErrInvalidPreset carrying a violation per field.
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

	if d.Slug == "" {
		s.slug, s.derivedSlug = Slugify(d.Name), true
	} else if s.slug, err = NewSlug(d.Slug); err != nil {
		collect(err)
	}

	if s.name, err = NewName(d.Name); err != nil {
		collect(err)
	}

	desc, v := plainText("description", d.Description, MaxDescriptionLength, false)
	if v != nil {
		bad = append(bad, *v)
	}

	s.description = desc

	if s.tags, err = NewTags(d.Tags); err != nil {
		collect(err)
	}

	if d.CenterFreq <= 0 || d.CenterFreq > MaxFrequency {
		bad = append(bad, shared.NewViolation("center_freq", "out_of_range", "a frequency in Hz between 1 and 300 GHz"))
	}

	if d.SampRate <= 0 || d.SampRate > maxInt32 {
		bad = append(bad, shared.NewViolation("samp_rate", "out_of_range", "a sample rate in S/s between 1 and 2147483647"))
	}

	s.centerFreq, s.sampRate = d.CenterFreq, d.SampRate

	s.startFreq = d.CenterFreq
	if d.StartFreq != nil {
		s.startFreq = *d.StartFreq
	}

	if s.startFreq <= 0 || s.startFreq > MaxFrequency {
		bad = append(bad, shared.NewViolation("start_freq", "out_of_range", "a frequency in Hz between 1 and 300 GHz"))
	} else if d.SampRate > 0 && abs(s.startFreq-d.CenterFreq)*2 > d.SampRate {
		bad = append(bad, shared.NewViolation("start_freq", "outside_band",
			"must lie within half the sample rate of the centre frequency"))
	}

	mod := d.StartMod
	if mod == "" {
		mod = DefaultStartMod
	}

	if s.startMod, err = NewModeID(mod); err != nil {
		collect(err)
	}

	s.tuningStep = DefaultTuningStep
	if d.TuningStep != nil {
		s.tuningStep = *d.TuningStep
	}

	if s.tuningStep <= 0 || s.tuningStep > maxInt32 {
		bad = append(bad, shared.NewViolation("tuning_step", "out_of_range", "a step in Hz between 1 and 2147483647"))
	}

	if q := d.InitialSquelchLevel; q != nil {
		if *q < MinSquelch || *q > MaxSquelch {
			bad = append(bad, shared.NewViolation("initial_squelch_level", "out_of_range", "between -150 and 0 dBFS"))
		}

		s.squelch = new(*q)
	}

	if n := d.InitialNRLevel; n != nil {
		if *n < MinNRLevel || *n > MaxNRLevel {
			bad = append(bad, shared.NewViolation("initial_nr_level", "out_of_range", "between -20 and 20 dB"))
		}

		s.nr = new(*n)
	}

	if w := d.WaterfallLevels; w != nil {
		levels, err := NewWaterfallLevels(w[0], w[1])
		if err != nil {
			collect(err)
		} else {
			s.waterfall = &levels
		}
	}

	if len(bad) > 0 {
		return Spec{}, ErrInvalidPreset.WithViolations(bad...)
	}

	return s, nil
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}

	return v
}

// Slug returns the slug, and whether it was derived from the name.
func (s Spec) Slug() (Slug, bool) { return s.slug, s.derivedSlug }

// WithSlug returns the spec with another slug (a derived slug made unique).
func (s Spec) WithSlug(slug Slug) Spec {
	s.slug = slug

	return s
}

// Preset is a device-independent preset (§7.1 presets). It holds no
// hardware setting.
type Preset struct {
	id        shared.UUID
	spec      Spec
	sortOrder int
	createdAt time.Time
	updatedAt time.Time
	version   int
}

// NewPreset creates a preset at the end of the list (sortOrder).
func NewPreset(id shared.UUID, spec Spec, sortOrder int, now time.Time) (*Preset, error) {
	if id.IsZero() || sortOrder < 0 {
		return nil, ErrInvalidPreset.WithDetail("invalid preset identity")
	}

	now = now.UTC().Truncate(time.Millisecond)

	return &Preset{id: id, spec: spec, sortOrder: sortOrder, createdAt: now, updatedAt: now, version: 1}, nil
}

// Replace replaces the definition (PUT) when expectedVersion is current.
// A derived slug keeps the current one: renaming never changes the URL.
func (p *Preset) Replace(spec Spec, expectedVersion int, now time.Time) error {
	if expectedVersion != p.version {
		return ErrVersionConflict
	}

	if spec.derivedSlug {
		spec.slug = p.spec.slug
	}

	p.spec = spec
	p.updatedAt = now.UTC().Truncate(time.Millisecond)
	p.version++

	return nil
}

// DeviceLimits are the reported limits of a device a preset is applied to
// (§7.1 devices.freq_min, freq_max, sample_rates).
type DeviceLimits struct {
	FreqMin, FreqMax int64
	SampleRates      []int64
}

// Fits checks the preset against a device (§7.1 "Capability validation at
// apply time"): the captured band lies within the device range and the
// sample rate is supported. The mode is checked once the mode catalogue
// exists (ADR 0020 Q9).
func (p *Preset) Fits(l DeviceLimits) error {
	half := p.spec.sampRate / 2
	lo, hi := p.spec.centerFreq-half, p.spec.centerFreq+half

	if lo < l.FreqMin || hi > l.FreqMax {
		return ErrPresetIncompatible.WithDetail("frequency_range: " + strconv.FormatInt(lo, 10) + "–" + strconv.FormatInt(hi, 10) +
			" Hz is outside the device range " + strconv.FormatInt(l.FreqMin, 10) + "–" + strconv.FormatInt(l.FreqMax, 10) + " Hz")
	}

	if !slices.Contains(l.SampleRates, p.spec.sampRate) {
		return ErrPresetIncompatible.WithDetail("sample_rate: the device does not support " + strconv.FormatInt(p.spec.sampRate, 10) + " S/s")
	}

	return nil
}

// ID returns the preset id.
func (p *Preset) ID() shared.UUID { return p.id }

// Slug returns the slug.
func (p *Preset) Slug() Slug { return p.spec.slug }

// Name returns the name.
func (p *Preset) Name() Name { return p.spec.name }

// Description returns the description (empty when none).
func (p *Preset) Description() string { return p.spec.description }

// Tags returns the tags.
func (p *Preset) Tags() Tags { return p.spec.tags }

// CenterFreq returns the centre frequency in Hz.
func (p *Preset) CenterFreq() int64 { return p.spec.centerFreq }

// SampRate returns the sample rate in S/s.
func (p *Preset) SampRate() int64 { return p.spec.sampRate }

// StartFreq returns the initial listener frequency in Hz.
func (p *Preset) StartFreq() int64 { return p.spec.startFreq }

// StartMod returns the initial mode.
func (p *Preset) StartMod() ModeID { return p.spec.startMod }

// TuningStep returns the tuning step in Hz.
func (p *Preset) TuningStep() int64 { return p.spec.tuningStep }

// InitialSquelchLevel returns the initial squelch in dBFS, if set.
func (p *Preset) InitialSquelchLevel() (int, bool) { return opt(p.spec.squelch) }

// InitialNRLevel returns the initial noise-reduction level in dB, if set.
func (p *Preset) InitialNRLevel() (int, bool) { return opt(p.spec.nr) }

// WaterfallLevels returns the waterfall levels, if set.
func (p *Preset) WaterfallLevels() (WaterfallLevels, bool) {
	if p.spec.waterfall == nil {
		return WaterfallLevels{}, false
	}

	return *p.spec.waterfall, true
}

func opt(v *int) (int, bool) {
	if v == nil {
		return 0, false
	}

	return *v, true
}

// SortOrder returns the display position.
func (p *Preset) SortOrder() int { return p.sortOrder }

// CreatedAt returns the creation time.
func (p *Preset) CreatedAt() time.Time { return p.createdAt }

// UpdatedAt returns the time of the last change.
func (p *Preset) UpdatedAt() time.Time { return p.updatedAt }

// Version returns the optimistic concurrency version.
func (p *Preset) Version() int { return p.version }

// Snapshot is the persisted form of a preset.
type Snapshot struct {
	ID                   shared.UUID
	Slug, Name           string
	Description          string
	Tags                 []string
	CenterFreq, SampRate int64
	StartFreq            int64
	StartMod             string
	TuningStep           int64
	Squelch, NR          *int
	Waterfall            *[2]int
	SortOrder            int
	CreatedAt, UpdatedAt time.Time
	Version              int
}

// Snapshot returns the persisted form.
func (p *Preset) Snapshot() Snapshot {
	s := Snapshot{
		ID: p.id, Slug: p.spec.slug.value, Name: p.spec.name.value, Description: p.spec.description, Tags: p.spec.tags.Values(),
		CenterFreq: p.spec.centerFreq, SampRate: p.spec.sampRate, StartFreq: p.spec.startFreq, StartMod: p.spec.startMod.value,
		TuningStep: p.spec.tuningStep, Squelch: p.spec.squelch, NR: p.spec.nr, SortOrder: p.sortOrder,
		CreatedAt: p.createdAt, UpdatedAt: p.updatedAt, Version: p.version,
	}

	if w := p.spec.waterfall; w != nil {
		s.Waterfall = &[2]int{w.min, w.max}
	}

	return s
}

// Rehydrate rebuilds a stored preset, validating it like an input.
func Rehydrate(s Snapshot) (*Preset, error) {
	spec, err := NewSpec(Draft{
		Slug: s.Slug, Name: s.Name, Description: s.Description, Tags: s.Tags, CenterFreq: s.CenterFreq, SampRate: s.SampRate,
		StartFreq: &s.StartFreq, StartMod: s.StartMod, TuningStep: &s.TuningStep, InitialSquelchLevel: s.Squelch,
		InitialNRLevel: s.NR, WaterfallLevels: s.Waterfall,
	})
	if err != nil {
		return nil, err
	}

	if s.ID.IsZero() || s.Version < 1 || s.SortOrder < 0 {
		return nil, ErrInvalidPreset.WithDetail("invalid stored preset")
	}

	return &Preset{id: s.ID, spec: spec, sortOrder: s.SortOrder, createdAt: s.CreatedAt, updatedAt: s.UpdatedAt, version: s.Version}, nil
}

// Repository persists presets.
type Repository interface {
	Get(ctx context.Context, id shared.UUID) (*Preset, error)
	// List returns every preset by sort order, then name.
	List(ctx context.Context) ([]*Preset, error)
	// SlugTaken reports whether another preset than except has slug.
	SlugTaken(ctx context.Context, slug Slug, except shared.UUID) (bool, error)
	// NextSortOrder returns the position after the last preset.
	NextSortOrder(ctx context.Context) (int, error)
	// Create inserts a preset (ErrSlugTaken on a duplicate slug).
	Create(ctx context.Context, p *Preset) error
	// Update writes p when the stored version is expectedVersion
	// (ErrVersionConflict otherwise, ErrSlugTaken on a duplicate slug).
	Update(ctx context.Context, p *Preset, expectedVersion int) error
	// Delete deletes a preset (ErrPresetNotFound, ErrPresetInUse while
	// schedules reference it).
	Delete(ctx context.Context, id shared.UUID) error
}
