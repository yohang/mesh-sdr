package bookmarks

import (
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Origin is where a bookmark comes from (TECHNICAL_SPEC §7.1 `origin`).
type Origin string

// Origins. M1b writes builtin (the shipped packs) and db (operators).
const (
	OriginConfig  Origin = "config"
	OriginDB      Origin = "db"
	OriginImport  Origin = "import"
	OriginBuiltin Origin = "builtin"
)

// ParseOrigin validates an origin.
func ParseOrigin(s string) (Origin, error) {
	switch o := Origin(s); o {
	case OriginConfig, OriginDB, OriginImport, OriginBuiltin:
		return o, nil
	}

	return "", ErrInvalidBookmark.WithDetail("unknown origin " + s)
}

// Limits of the fields (TECHNICAL_SPEC §7.1 `bookmarks`).
const (
	MaxNameLength        = 128
	MaxDescriptionLength = 1024
	MaxModeLength        = 24
)

// modePattern is the syntax of a mode id (modulation, underlying).
var modePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,23}$`)

// AnalogModes is the analog mode catalogue of the nodes (the demodulator
// registry of internal/radio/infra/engine): the modes listeners can tune
// in M1. A digital mode is kept as is in a pack bookmark (usable in M2).
var AnalogModes = []string{"am", "sam", "nfm", "usb", "lsb", "cw", "wfm"}

// IsAnalog reports whether mode is in the analog catalogue.
func IsAnalog(mode string) bool { return slices.Contains(AnalogModes, mode) }

// TakesUnderlying reports whether a modulation takes an underlying mode:
// the digital modes do (the analog demodulator they decode from, such as
// usb for rtty450), the analog modes do not.
func TakesUnderlying(mode string) bool { return !IsAnalog(mode) }

// ScopeKind is the kind of a bookmark scope (BMK-008).
type ScopeKind string

// Scope kinds.
const (
	ScopeAll    ScopeKind = "all"
	ScopeDevice ScopeKind = "device"
	ScopePreset ScopeKind = "preset"
)

// Scope is where a bookmark is shown: every device, one device or one
// preset. The zero value is all devices.
type Scope struct {
	kind   ScopeKind
	device shared.DeviceID
	preset shared.UUID
}

// AllDevices is the scope of every device.
func AllDevices() Scope { return Scope{kind: ScopeAll} }

// OnDevice is the scope of one device.
func OnDevice(id shared.DeviceID) (Scope, error) {
	if id.IsZero() {
		return Scope{}, ErrInvalidBookmark.WithViolations(shared.NewViolation("device", "required", "choose a device"))
	}

	return Scope{kind: ScopeDevice, device: id}, nil
}

// OnPreset is the scope of one preset.
func OnPreset(id shared.UUID) (Scope, error) {
	if id.IsZero() {
		return Scope{}, ErrInvalidBookmark.WithViolations(shared.NewViolation("preset", "required", "choose a preset"))
	}

	return Scope{kind: ScopePreset, preset: id}, nil
}

// Kind returns the scope kind.
func (s Scope) Kind() ScopeKind {
	if s.kind == "" {
		return ScopeAll
	}

	return s.kind
}

// Device returns the device of a device scope (zero otherwise).
func (s Scope) Device() shared.DeviceID { return s.device }

// Preset returns the preset of a preset scope (zero otherwise).
func (s Scope) Preset() shared.UUID { return s.preset }

// Matches reports whether a bookmark of this scope shows on a device whose
// active preset is preset (zero: none).
func (s Scope) Matches(device shared.DeviceID, preset shared.UUID) bool {
	switch s.Kind() {
	case ScopeDevice:
		return s.device == device
	case ScopePreset:
		return !preset.IsZero() && s.preset == preset
	default:
		return true
	}
}

// Draft is the editable content of a hub bookmark (BMK-003).
type Draft struct {
	Name        string
	Frequency   int64
	Modulation  string
	Underlying  string
	Description string
	Scannable   bool
	Scope       Scope
}

// Bookmark is a hub-wide bookmark (TECHNICAL_SPEC §7.1 `bookmarks`): a pack
// row (builtin, read-only) or a hub row (db, operators).
type Bookmark struct {
	id          shared.UUID
	name        string
	frequency   int64
	modulation  string
	underlying  string
	description string
	scannable   bool
	tags        []string
	origin      Origin
	locked      []string
	scope       Scope
	createdBy   shared.UUID
	createdAt   time.Time
	updatedAt   time.Time
	version     int
}

// clean normalises and checks the fields of a draft; it returns every
// problem at once.
func clean(d Draft) (Draft, error) {
	d.Name = strings.TrimSpace(d.Name)
	d.Modulation = strings.ToLower(strings.TrimSpace(d.Modulation))
	d.Underlying = strings.ToLower(strings.TrimSpace(d.Underlying))
	d.Description = strings.TrimSpace(d.Description)

	var v []shared.Violation

	add := func(path, code, msg string) { v = append(v, shared.NewViolation(path, shared.Code(code), msg)) }

	switch n := utf8.RuneCountInString(d.Name); {
	case n == 0:
		add("name", "required", "required")
	case n > MaxNameLength:
		add("name", "too_long", "at most 128 characters")
	}

	if d.Frequency <= 0 {
		add("frequency", "out_of_range", "enter a frequency in Hz above 0")
	}

	switch {
	case d.Modulation == "":
		add("modulation", "required", "required")
	case !modePattern.MatchString(d.Modulation):
		add("modulation", "invalid", "use a mode id such as nfm, am or usb")
	}

	if d.Underlying != "" && !modePattern.MatchString(d.Underlying) {
		add("underlying", "invalid", "use a mode id such as usb or lsb")
	}

	if utf8.RuneCountInString(d.Description) > MaxDescriptionLength {
		add("description", "too_long", "at most 1024 characters")
	}

	if len(v) > 0 {
		return Draft{}, ErrInvalidBookmark.WithViolations(v...)
	}

	return d, nil
}

// NewBookmark validates the shape of a hub bookmark created by a user (by
// may be zero: the CLI). The modes a device offers are checked by the
// service, which knows the node capabilities.
func NewBookmark(id shared.UUID, d Draft, by shared.UUID, now time.Time) (*Bookmark, error) {
	d, err := clean(d)
	if err != nil {
		return nil, err
	}

	now = now.UTC().Truncate(time.Millisecond)

	return &Bookmark{
		id: id, name: d.Name, frequency: d.Frequency, modulation: d.Modulation, underlying: d.Underlying,
		description: d.Description, scannable: d.Scannable, tags: []string{}, origin: OriginDB, locked: []string{},
		scope: d.Scope, createdBy: by, createdAt: now, updatedAt: now, version: 1,
	}, nil
}

// Update replaces the content of a hub bookmark when expectedVersion is
// current. Pack bookmarks are read-only.
func (b *Bookmark) Update(d Draft, expectedVersion int, now time.Time) error {
	if b.origin == OriginBuiltin {
		return ErrReadOnly
	}

	if b.version != expectedVersion {
		return ErrVersionConflict
	}

	d, err := clean(d)
	if err != nil {
		return err
	}

	b.name, b.frequency, b.modulation, b.underlying = d.Name, d.Frequency, d.Modulation, d.Underlying
	b.description, b.scannable, b.scope = d.Description, d.Scannable, d.Scope
	b.updatedAt = now.UTC().Truncate(time.Millisecond)
	b.version++

	return nil
}

// Snapshot is the stored form of a bookmark (repository rows, packs).
type Snapshot struct {
	ID          shared.UUID
	Name        string
	Frequency   int64
	Modulation  string
	Underlying  string
	Description string
	Scannable   bool
	Tags        []string
	Origin      Origin
	Locked      []string
	Scope       Scope
	CreatedBy   shared.UUID
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Version     int
}

// Rehydrate rebuilds a stored bookmark, checking its fields like a new one.
func Rehydrate(s Snapshot) (*Bookmark, error) {
	d, err := clean(Draft{
		Name: s.Name, Frequency: s.Frequency, Modulation: s.Modulation, Underlying: s.Underlying,
		Description: s.Description, Scannable: s.Scannable, Scope: s.Scope,
	})
	if err != nil {
		return nil, err
	}

	if _, err := ParseOrigin(string(s.Origin)); err != nil {
		return nil, err
	}

	if s.ID.IsZero() || s.Version < 1 {
		return nil, ErrInvalidBookmark.WithDetail("missing id or version")
	}

	return &Bookmark{
		id: s.ID, name: d.Name, frequency: d.Frequency, modulation: d.Modulation, underlying: d.Underlying,
		description: d.Description, scannable: d.Scannable, tags: nonNil(s.Tags), origin: s.Origin,
		locked: nonNil(s.Locked), scope: d.Scope, createdBy: s.CreatedBy, createdAt: s.CreatedAt.UTC(),
		updatedAt: s.UpdatedAt.UTC(), version: s.Version,
	}, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}

	return slices.Clone(s)
}

// Snapshot returns the stored form.
func (b *Bookmark) Snapshot() Snapshot {
	return Snapshot{
		ID: b.id, Name: b.name, Frequency: b.frequency, Modulation: b.modulation, Underlying: b.underlying,
		Description: b.description, Scannable: b.scannable, Tags: slices.Clone(b.tags), Origin: b.origin,
		Locked: slices.Clone(b.locked), Scope: b.scope, CreatedBy: b.createdBy, CreatedAt: b.createdAt,
		UpdatedAt: b.updatedAt, Version: b.version,
	}
}

// ID returns the id.
func (b *Bookmark) ID() shared.UUID { return b.id }

// Name returns the name.
func (b *Bookmark) Name() string { return b.name }

// Frequency returns the frequency in Hz.
func (b *Bookmark) Frequency() int64 { return b.frequency }

// Modulation returns the mode id.
func (b *Bookmark) Modulation() string { return b.modulation }

// Underlying returns the underlying mode ("" when none).
func (b *Bookmark) Underlying() string { return b.underlying }

// Description returns the description ("" when none).
func (b *Bookmark) Description() string { return b.description }

// Scannable reports whether the scanner visits the bookmark (BMK-006).
func (b *Bookmark) Scannable() bool { return b.scannable }

// Tags returns the tags (pack:<name>, region:<r>).
func (b *Bookmark) Tags() []string { return slices.Clone(b.tags) }

// Origin returns the origin.
func (b *Bookmark) Origin() Origin { return b.origin }

// ReadOnly reports whether the bookmark is a pack row.
func (b *Bookmark) ReadOnly() bool { return b.origin == OriginBuiltin }

// Scope returns the scope.
func (b *Bookmark) Scope() Scope { return b.scope }

// CreatedBy returns the creator (zero for pack rows or a deleted user).
func (b *Bookmark) CreatedBy() shared.UUID { return b.createdBy }

// UpdatedAt returns the time of the last change.
func (b *Bookmark) UpdatedAt() time.Time { return b.updatedAt }

// Version returns the version (optimistic concurrency).
func (b *Bookmark) Version() int { return b.version }

// Regions returns the regions of a region pack row (r1, r2, r3); none for
// a general pack row or a hub row.
func (b *Bookmark) Regions() []string {
	var out []string

	for _, t := range b.tags {
		if r, ok := strings.CutPrefix(t, regionTagPrefix); ok {
			out = append(out, r)
		}
	}

	return out
}

// regionTagPrefix starts the tag of a region pack row.
const regionTagPrefix = "region:"
