// Package bookmarks is the hub-wide bookmarks module (TECHNICAL_SPEC §7.1
// `bookmarks`, BMK-001…008): the SQLite repository, the bookmark packs and
// band plans shipped with the hub (imported from OpenWebRX+, embedded), the
// pack sync run by `meshsdr hub migrate`, the JSON API (GET /bookmarks and
// GET /bandplan) and the Bookmarks › Manage pages of operators and admins.
//
// Pack rows (origin builtin) are read-only; hub rows (origin db) are
// created, edited and deleted by operators and admins, audited and
// announced on the hub events (`bookmark.changed`).
package bookmarks

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/shared/audit"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
	"github.com/yohang/mesh-sdr/internal/web/render"
)

// Audit actions.
const (
	ActionCreate = "bookmark.create"
	ActionUpdate = "bookmark.update"
	ActionDelete = "bookmark.delete"
)

// Device is an enabled device of the registry, as the bookmarks see it.
type Device struct {
	ID   shared.DeviceID
	Name string
	// ActivePreset is the preset the device runs (zero: none).
	ActivePreset shared.UUID
	// Modes are the modes the device's node reports available.
	Modes []string
}

// Devices lists the enabled devices of the registry.
type Devices interface {
	Devices(ctx context.Context) ([]Device, error)
}

// Preset is a preset a bookmark can be scoped to.
type Preset struct {
	ID   shared.UUID
	Name string
}

// Presets lists the presets.
type Presets interface {
	Presets(ctx context.Context) ([]Preset, error)
}

// Change ops of a `bookmark.changed` event.
const (
	OpUpsert = "upsert"
	OpDelete = "delete"
)

// Change is a committed change of a hub bookmark.
type Change struct {
	Op       string
	Bookmark *Bookmark
}

// Deps are the dependencies of the module (the pack sync of `meshsdr hub
// migrate` is Sync, which needs none of them).
type Deps struct {
	DB      *db.DB
	Audit   audit.Appender
	Devices Devices
	Presets Presets
	// Region returns the bandplan.region setting (r1, r2 or r3).
	Region func() string
	// CanListen reports whether the caller of ctx may listen to a device
	// (the listen policy).
	CanListen func(ctx context.Context, device shared.DeviceID) (bool, error)
	// User returns the signed-in user of ctx (zero when anonymous).
	User func(ctx context.Context) shared.UUID
	// Changed, when set, is told every committed change of a hub bookmark.
	Changed func(ctx context.Context, c Change)
	Render  *render.Renderer
	// Guard admits the operators and admins (identity).
	Guard  func(http.Handler) http.Handler
	IDs    *shared.UUIDv7Generator
	Now    func() time.Time
	Logger *slog.Logger
}

// Module is the bookmarks module: use cases, JSON API handlers and pages
// (an internal/http.Module).
type Module struct {
	d         Deps
	repo      *Repository
	bandplans map[string]*Bandplan
}

// New parses the embedded band plans and returns the module. Every
// dependency is required except Changed.
func New(d Deps) (*Module, error) {
	plans, err := LoadBandplans()
	if err != nil {
		return nil, fmt.Errorf("band plans: %w", err)
	}

	if d.IDs == nil {
		d.IDs = shared.NewUUIDv7Generator()
	}

	return &Module{d: d, repo: NewRepository(d.DB), bandplans: plans}, nil
}

// Region returns the current band plan region (r1 when invalid).
func (m *Module) Region() string {
	if r := m.d.Region(); ValidRegion(r) {
		return r
	}

	return "r1"
}

// Bandplan returns the band plan of the current region.
func (m *Module) Bandplan() *Bandplan { return m.bandplans[m.Region()] }

// Range is a frequency range in Hz; the zero value is every frequency.
type Range struct {
	from, to int64
	bounded  bool
}

// NewRange validates a range; nil bounds are open.
func NewRange(from, to *int64) (Range, error) {
	r := Range{from: 0, to: math.MaxInt64, bounded: true}
	if from != nil {
		r.from = *from
	}

	if to != nil {
		r.to = *to
	}

	if r.from < 0 || r.to < r.from {
		return Range{}, ErrInvalidRange
	}

	return r, nil
}

// Bounds returns the lowest and highest frequencies of the range.
func (r Range) Bounds() (int64, int64) {
	if !r.bounded {
		return 0, math.MaxInt64
	}

	return r.from, r.to
}

// device returns the enabled device id from the registry.
func (m *Module) device(ctx context.Context, id string) (Device, bool, error) {
	all, err := m.d.Devices.Devices(ctx)
	if err != nil {
		return Device{}, false, fmt.Errorf("list devices: %w", err)
	}

	for _, d := range all {
		if d.ID.String() == id {
			return d, true, nil
		}
	}

	return Device{}, false, nil
}

// ForDevice returns the bookmarks shown on a device in a range (BMK-001,
// GET /bookmarks): the general and current region pack rows, and the hub
// rows whose scope matches the device and its active preset. A device the
// caller may not listen to answers ErrDeviceNotFound.
func (m *Module) ForDevice(ctx context.Context, deviceID string, rg Range) ([]*Bookmark, error) {
	d, ok, err := m.device(ctx, deviceID)
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, ErrDeviceNotFound
	}

	allowed, err := m.d.CanListen(ctx, d.ID)
	if err != nil {
		return nil, fmt.Errorf("listen policy of %s: %w", d.ID, err)
	}

	if !allowed {
		return nil, ErrDeviceNotFound
	}

	from, to := rg.Bounds()

	all, err := m.repo.InRange(ctx, m.Region(), from, to)
	if err != nil {
		return nil, err
	}

	out := all[:0]

	for _, b := range all {
		if b.Scope().Matches(d.ID, d.ActivePreset) {
			out = append(out, b)
		}
	}

	return out, nil
}

// Filter selects the bookmarks of the management table (BMK-005).
type Filter struct {
	// Origin is db, builtin or "" for both.
	Origin Origin
	// Device keeps the bookmarks shown on this device ("" for any).
	Device string
	// Scope keeps one scope kind ("" for any).
	Scope ScopeKind
	Range Range
}

// List returns the bookmarks of the management table: the hub rows and
// the pack rows of the current region, by frequency.
func (m *Module) List(ctx context.Context, f Filter) ([]*Bookmark, error) {
	from, to := f.Range.Bounds()

	all, err := m.repo.InRange(ctx, m.Region(), from, to)
	if err != nil {
		return nil, err
	}

	var dev *Device

	if f.Device != "" {
		d, ok, err := m.device(ctx, f.Device)
		if err != nil {
			return nil, err
		}

		if !ok {
			return []*Bookmark{}, nil
		}

		dev = &d
	}

	out := make([]*Bookmark, 0, len(all))

	for _, b := range all {
		switch {
		case f.Origin != "" && b.Origin() != f.Origin:
		case f.Scope != "" && b.Scope().Kind() != f.Scope:
		case dev != nil && !b.Scope().Matches(dev.ID, dev.ActivePreset):
		default:
			out = append(out, b)
		}
	}

	return out, nil
}

// ParseID parses a bookmark id (ErrBookmarkNotFound when invalid).
func ParseID(s string) (shared.UUID, error) {
	id, err := shared.ParseUUID(s)
	if err != nil {
		return shared.UUID{}, ErrBookmarkNotFound
	}

	return id, nil
}

// Get returns one bookmark.
func (m *Module) Get(ctx context.Context, id string) (*Bookmark, error) {
	bid, err := ParseID(id)
	if err != nil {
		return nil, err
	}

	return m.repo.Get(ctx, bid)
}

// check validates a draft against the hub (BMK-003): the shape of every
// field, the scope's device or preset, the modulation known to the
// capabilities of the scoped device's node (for the other scopes: the
// analog catalogue or a mode some node reports) and an underlying mode
// only for the modes that take one.
func (m *Module) check(ctx context.Context, d Draft) (Draft, error) {
	d, err := clean(d)
	if err != nil {
		return Draft{}, err
	}

	var v []shared.Violation

	add := func(path, code, msg string) { v = append(v, shared.NewViolation(path, shared.Code(code), msg)) }

	devices, err := m.d.Devices.Devices(ctx)
	if err != nil {
		return Draft{}, fmt.Errorf("list devices: %w", err)
	}

	modes := slices.Clone(AnalogModes)

	knownDevice := true

	switch d.Scope.Kind() {
	case ScopeDevice:
		i := slices.IndexFunc(devices, func(x Device) bool { return x.ID == d.Scope.Device() })
		if knownDevice = i >= 0; knownDevice {
			modes = devices[i].Modes
		} else {
			add("device", "unknown", "choose a device of the registry")
		}
	case ScopePreset:
		ok, err := m.presetExists(ctx, d.Scope.Preset())
		if err != nil {
			return Draft{}, err
		}

		if !ok {
			add("preset", "unknown", "choose an existing preset")
		}

		fallthrough
	default:
		for _, dev := range devices {
			modes = append(modes, dev.Modes...)
		}
	}

	if knownDevice && !slices.Contains(modes, d.Modulation) {
		add("modulation", "unknown_mode", "use a mode the receivers offer, such as "+modeHint(modes))
	}

	switch {
	case d.Underlying == "":
	case !TakesUnderlying(d.Modulation):
		add("underlying", "not_allowed", "an analog mode takes no underlying mode")
	case !IsAnalog(d.Underlying):
		add("underlying", "unknown_mode", "use an analog mode such as usb or lsb")
	}

	if len(v) > 0 {
		return Draft{}, ErrInvalidBookmark.WithViolations(v...)
	}

	return d, nil
}

func modeHint(modes []string) string {
	if len(modes) == 0 {
		return "a mode its node reports (none reported yet)"
	}

	s := slices.Clone(modes)
	slices.Sort(s)
	s = slices.Compact(s)

	out := ""

	for i, mode := range s {
		if i == 6 {
			return out + "…"
		}

		if i > 0 {
			out += ", "
		}

		out += mode
	}

	return out
}

func (m *Module) presetExists(ctx context.Context, id shared.UUID) (bool, error) {
	all, err := m.d.Presets.Presets(ctx)
	if err != nil {
		return false, fmt.Errorf("list presets: %w", err)
	}

	return slices.ContainsFunc(all, func(p Preset) bool { return p.ID == id }), nil
}

// Create validates and stores a hub bookmark, audited.
func (m *Module) Create(ctx context.Context, d Draft) (*Bookmark, error) {
	d, err := m.check(ctx, d)
	if err != nil {
		return nil, err
	}

	now := m.d.Now()

	id, err := m.d.IDs.New(now)
	if err != nil {
		return nil, fmt.Errorf("bookmark id: %w", err)
	}

	b, err := NewBookmark(id, d, m.d.User(ctx), now)
	if err != nil {
		return nil, err
	}

	err = m.d.DB.WithinTx(ctx, func(ctx context.Context) error {
		if err := m.unique(ctx, b); err != nil {
			return err
		}

		if err := m.repo.Create(ctx, b); err != nil {
			return err
		}

		return m.audit(ctx, audit.Record{Action: ActionCreate, TargetType: "bookmark", TargetID: id.String(), After: auditFields(b)})
	})
	if err != nil {
		return nil, err
	}

	m.changed(ctx, OpUpsert, b)

	return b, nil
}

func (m *Module) unique(ctx context.Context, b *Bookmark) error {
	taken, err := m.repo.KeyTaken(ctx, b.Name(), b.Frequency(), b.Modulation(), b.Scope(), b.ID())
	if err != nil {
		return err
	}

	if taken {
		return ErrDuplicate
	}

	return nil
}

// Update replaces a hub bookmark when expectedVersion is current, audited.
// Pack bookmarks are read-only (ErrReadOnly).
func (m *Module) Update(ctx context.Context, id string, expectedVersion int, d Draft) (*Bookmark, error) {
	bid, err := ParseID(id)
	if err != nil {
		return nil, err
	}

	var b *Bookmark

	err = m.d.DB.WithinTx(ctx, func(ctx context.Context) error {
		cur, err := m.repo.Get(ctx, bid)
		if err != nil {
			return err
		}

		if cur.ReadOnly() {
			return ErrReadOnly
		}

		d, err := m.check(ctx, d)
		if err != nil {
			return err
		}

		before := auditFields(cur)

		if err := cur.Update(d, expectedVersion, m.d.Now()); err != nil {
			return err
		}

		if err := m.unique(ctx, cur); err != nil {
			return err
		}

		if err := m.repo.Update(ctx, cur, expectedVersion); err != nil {
			return err
		}

		b = cur

		return m.audit(ctx, audit.Record{Action: ActionUpdate, TargetType: "bookmark", TargetID: bid.String(), Before: before, After: auditFields(cur)})
	})
	if err != nil {
		return nil, err
	}

	m.changed(ctx, OpUpsert, b)

	return b, nil
}

// Delete deletes a hub bookmark when expectedVersion is current, audited.
func (m *Module) Delete(ctx context.Context, id string, expectedVersion int) error {
	bid, err := ParseID(id)
	if err != nil {
		return err
	}

	var b *Bookmark

	err = m.d.DB.WithinTx(ctx, func(ctx context.Context) error {
		cur, err := m.repo.Get(ctx, bid)
		if err != nil {
			return err
		}

		switch {
		case cur.ReadOnly():
			return ErrReadOnly
		case cur.Version() != expectedVersion:
			return ErrVersionConflict
		}

		if err := m.repo.Delete(ctx, bid, cur.Origin()); err != nil {
			return err
		}

		b = cur

		return m.audit(ctx, audit.Record{Action: ActionDelete, TargetType: "bookmark", TargetID: bid.String(), Before: auditFields(cur)})
	})
	if err != nil {
		return err
	}

	m.changed(ctx, OpDelete, b)

	return nil
}

func (m *Module) audit(ctx context.Context, r audit.Record) error { return m.d.Audit.Append(ctx, r) }

func (m *Module) changed(ctx context.Context, op string, b *Bookmark) {
	if m.d.Changed != nil {
		m.d.Changed(ctx, Change{Op: op, Bookmark: b})
	}
}

// auditFields is the audited form of a bookmark.
func auditFields(b *Bookmark) map[string]string {
	out := map[string]string{
		"name": b.Name(), "frequency": strconv.FormatInt(b.Frequency(), 10), "modulation": b.Modulation(),
		"scannable": strconv.FormatBool(b.Scannable()), "scope": string(b.Scope().Kind()), "version": strconv.Itoa(b.Version()),
	}

	if b.Underlying() != "" {
		out["underlying"] = b.Underlying()
	}

	if b.Description() != "" {
		out["description"] = b.Description()
	}

	if d := b.Scope().Device(); !d.IsZero() {
		out["device_id"] = d.String()
	}

	if p := b.Scope().Preset(); !p.IsZero() {
		out["preset_id"] = p.String()
	}

	return out
}
