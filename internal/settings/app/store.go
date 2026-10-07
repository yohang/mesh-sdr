package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/yohang/mesh-sdr/internal/shared/audit"

	"github.com/yohang/mesh-sdr/internal/settings/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// secretMask replaces secret values in audit records.
const secretMask = `"***"`

// Store is the settings store: it resolves every key with the precedence
// config (locked) > DB > default, validates and saves admin writes with
// their audit records, and publishes an immutable snapshot after each
// committed write (ADM-002, ADR 0010).
type Store struct {
	repo    domain.Repository
	catalog Catalog
	tx      Transactor
	audit   audit.Appender
	now     Clock
	logger  *slog.Logger

	mu   sync.Mutex // serialises writes
	snap atomic.Pointer[Snapshot]
	subs []func(*Snapshot)
}

// StoreDeps are the dependencies of Store.
type StoreDeps struct {
	Repo    domain.Repository
	Catalog Catalog
	Tx      Transactor
	Audit   audit.Appender
	Now     Clock
	Logger  *slog.Logger
}

// NewStore returns a store whose snapshot holds the config values and the
// defaults until Load reads the DB.
func NewStore(d StoreDeps) *Store {
	s := &Store{repo: d.Repo, catalog: d.Catalog, tx: d.Tx, audit: d.Audit, now: d.Now, logger: d.Logger}
	s.snap.Store(s.resolve(0, nil, nil))

	return s
}

// Snapshot returns the current snapshot.
func (s *Store) Snapshot() *Snapshot { return s.snap.Load() }

// Schema returns the JSON Schema of the settings namespace.
func (s *Store) Schema() json.RawMessage { return s.catalog.Schema() }

// Subscribe registers fn, called with the new snapshot after each committed
// write. Consumers that derive state from settings use it; the others read
// the snapshot on every use.
func (s *Store) Subscribe(fn func(*Snapshot)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.subs = append(s.subs, fn)
}

// Load reads the DB settings and publishes the first snapshot. Rows whose
// key the schema no longer defines, and invalid values, are ignored (the
// default applies) and logged; invalid values are also audited (TECHNICAL_SPEC
// §7.4 "Validation at startup" step 6). They never fail the start.
func (s *Store) Load(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	snap, ignored, err := s.read(ctx)
	if err != nil {
		return err
	}

	for _, ig := range ignored {
		if errors.Is(ig.err, domain.ErrUnknownSetting) {
			s.logger.WarnContext(ctx, "DB setting with an unknown key ignored", slog.String("key", ig.row.Key().String()))

			continue
		}

		s.logger.WarnContext(ctx, "invalid DB setting ignored, the default applies",
			slog.String("key", ig.row.Key().String()), slog.Any("error", ig.err))

		rec := record(ActionIgnored, ig.row.Key().String(), audit.ResultOK, ig.row.Value().String(), "")
		rec.Actor = audit.System

		if err := s.audit.Append(ctx, rec); err != nil {
			return fmt.Errorf("audit ignored setting %s: %w", ig.row.Key(), err)
		}
	}

	s.snap.Store(snap)

	return nil
}

type ignoredRow struct {
	row *domain.Setting
	err error
}

// read resolves every key from the DB rows.
func (s *Store) read(ctx context.Context) (*Snapshot, []ignoredRow, error) {
	rev, err := s.repo.Revision(ctx)
	if err != nil {
		return nil, nil, err
	}

	rows, err := s.repo.List(ctx)
	if err != nil {
		return nil, nil, err
	}

	valid := map[string]*domain.Setting{}

	var ignored []ignoredRow

	for _, r := range rows {
		if _, err := s.catalog.Validate(r.Key(), r.Value()); err != nil {
			ignored = append(ignored, ignoredRow{row: r, err: err})

			continue
		}

		valid[r.Key().String()] = r
	}

	invalid := func() map[string]*domain.Setting {
		m := map[string]*domain.Setting{}
		for _, ig := range ignored {
			m[ig.row.Key().String()] = ig.row
		}

		return m
	}

	snap := s.resolve(rev, valid, invalid())

	// A DB value that breaks a check across keys (with the config values
	// or other DB values) is ignored too.
	dropped := false

	for _, v := range s.catalog.Check(func(k string) (any, bool) { t := snap.Typed(k); return t, t != nil }) {
		if r, ok := valid[v.Path()]; ok {
			ignored = append(ignored, ignoredRow{row: r, err: domain.ErrInvalidSetting.WithViolations(v)})
			delete(valid, v.Path())

			dropped = true
		}
	}

	if dropped {
		snap = s.resolve(rev, valid, invalid())
	}

	return snap, ignored, nil
}

// resolve builds the snapshot of the definitions over the valid DB rows;
// ignored rows keep only their version.
func (s *Store) resolve(rev int64, rows, ignored map[string]*domain.Setting) *Snapshot {
	defs := s.catalog.Definitions()
	entries := make([]domain.Effective, 0, len(defs))
	typed := make(map[string]any, len(defs))

	for _, d := range defs {
		var cfg *domain.Configured
		if c, ok := s.catalog.Configured(d.Key()); ok {
			cfg = &c
		}

		e := domain.Resolve(d, cfg, rows[d.Key().String()])
		if ig, ok := ignored[d.Key().String()]; ok && rows[d.Key().String()] == nil {
			e = domain.ResolveIgnoring(d, cfg, ig)
		}
		entries = append(entries, e)

		if !e.Value().IsNull() {
			if v, err := s.catalog.Validate(d.Key(), e.Value()); err == nil {
				typed[d.Key().String()] = v
			}
		}
	}

	return newSnapshot(rev, entries, typed)
}

// Apply writes a change set atomically for the user by (zero: the hub
// itself; the audit names the caller of the request in ctx) and returns the new
// snapshot. Errors: domain.ErrSettingLocked (audited as denied),
// domain.ErrSecretsUnavailable, domain.ErrInvalidSetting with one violation
// per rejected key (unknown keys included), domain.ErrVersionConflict
// naming the stale keys.
func (s *Store) Apply(ctx context.Context, by shared.UUID, set domain.ChangeSet) (*Snapshot, error) {
	snap, subs, err := s.apply(ctx, by, set)
	if err != nil {
		return nil, err
	}

	for _, fn := range subs {
		fn(snap)
	}

	return snap, nil
}

func (s *Store) apply(ctx context.Context, by shared.UUID, set domain.ChangeSet) (*Snapshot, []func(*Snapshot), error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cur := s.Snapshot()
	changes := set.Changes()

	prospective := map[string]any{}
	for _, e := range cur.All() {
		if t := cur.Typed(e.Key().String()); t != nil {
			prospective[e.Key().String()] = t
		}
	}

	var (
		violations []shared.Violation
		locked     []domain.Effective
	)

	for _, c := range changes {
		key := c.Key().String()

		e, ok := cur.Get(key)
		if !ok {
			violations = append(violations, shared.NewViolation(key, domain.ErrUnknownSetting.Code(), "unknown setting"))

			continue
		}

		if e.Locked() {
			locked = append(locked, e)

			continue
		}

		if e.Definition().Secret() {
			return nil, nil, domain.ErrSecretsUnavailable
		}

		v, ok := c.Value()
		if !ok {
			def := e.Definition().Default()
			if t, err := s.catalog.Validate(c.Key(), def); err == nil && !def.IsNull() {
				prospective[key] = t
			} else {
				delete(prospective, key)
			}

			continue
		}

		t, err := s.catalog.Validate(c.Key(), v)
		if err != nil {
			violations = append(violations, violationsOf(key, err)...)

			continue
		}

		prospective[key] = t
	}

	if len(locked) > 0 {
		return nil, nil, s.denyLocked(ctx, changes, locked)
	}

	if len(violations) == 0 {
		violations = s.catalog.Check(func(k string) (any, bool) { v, ok := prospective[k]; return v, ok })
	}

	if len(violations) > 0 {
		return nil, nil, domain.ErrInvalidSetting.WithViolations(violations...)
	}

	if err := s.tx.WithinTx(ctx, func(ctx context.Context) error { return s.write(ctx, by, cur, changes) }); err != nil {
		return nil, nil, err
	}

	snap, ignored, err := s.read(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("reload settings: %w", err)
	}

	for _, ig := range ignored {
		s.logger.WarnContext(ctx, "invalid DB setting ignored", slog.String("key", ig.row.Key().String()), slog.Any("error", ig.err))
	}

	s.snap.Store(snap)

	return snap, slices.Clone(s.subs), nil
}

func violationsOf(key string, err error) []shared.Violation {
	var de *shared.Error
	if errors.As(err, &de) {
		if vs := de.Violations(); len(vs) > 0 {
			return vs
		}

		return []shared.Violation{shared.NewViolation(key, de.Code(), de.Message())}
	}

	return []shared.Violation{shared.NewViolation(key, "invalid_value", "invalid value")}
}

// denyLocked audits the refused writes to locked keys and returns the 409.
func (s *Store) denyLocked(ctx context.Context, changes []domain.Change, locked []domain.Effective) error {
	byKey := map[string]domain.Change{}
	for _, c := range changes {
		byKey[c.Key().String()] = c
	}

	var origins []string

	for _, e := range locked {
		c := byKey[e.Key().String()]
		action, after := ActionUpdate, ""

		if v, ok := c.Value(); ok {
			after = mask(e.Definition(), v)
		} else {
			action = ActionReset
		}

		rec := record(action, e.Key().String(), audit.ResultDenied, mask(e.Definition(), e.Value()), after)
		if err := s.audit.Append(ctx, rec); err != nil {
			return fmt.Errorf("audit denied setting write: %w", err)
		}

		origins = append(origins, e.Key().String()+" is set in "+e.Origin())
	}

	return domain.ErrSettingLocked.WithDetail(strings.Join(origins, "; "))
}

func mask(d domain.Definition, v domain.Value) string {
	if d.Secret() && !v.IsNull() {
		return secretMask
	}

	return v.String()
}

// write runs in the transaction: version check, then one revision for the
// whole change set and one audit record per changed key.
func (s *Store) write(ctx context.Context, by shared.UUID, cur *Snapshot, changes []domain.Change) error {
	rows := make([]*domain.Setting, len(changes))

	var conflicts []shared.Violation

	for i, c := range changes {
		row, err := s.repo.Get(ctx, c.Key())
		if err != nil {
			return err
		}

		have := int64(0)
		if row != nil {
			have = row.Version()
		}

		if have != c.Expected() {
			conflicts = append(conflicts, shared.NewViolation(c.Key().String(), domain.ErrVersionConflict.Code(),
				"current version "+strconv.FormatInt(have, 10)))
		}

		rows[i] = row
	}

	if len(conflicts) > 0 {
		return domain.ErrVersionConflict.WithViolations(conflicts...)
	}

	rev, err := s.repo.NextRevision(ctx)
	if err != nil {
		return err
	}

	now := s.now()

	for i, c := range changes {
		row := rows[i]
		e, _ := cur.Get(c.Key().String())
		def := e.Definition()

		var before, after, action string
		if row != nil {
			before = mask(def, row.Value())
		}

		v, set := c.Value()

		switch {
		case !set && row == nil:
			continue // nothing to reset
		case !set:
			if err := s.repo.Delete(ctx, c.Key()); err != nil {
				return err
			}

			action, after = ActionReset, mask(def, def.Default())
		case row == nil:
			if row, err = domain.NewSetting(c.Key(), v, rev, by, now); err != nil {
				return err
			}

			fallthrough
		default:
			if row.Version() != rev {
				if err := row.Replace(v, rev, by, now); err != nil {
					return err
				}
			}

			if err := s.repo.Save(ctx, row); err != nil {
				return err
			}

			action, after = ActionUpdate, mask(def, v)
		}

		if err := s.audit.Append(ctx, record(action, c.Key().String(), audit.ResultOK, before, after)); err != nil {
			return fmt.Errorf("audit setting %s: %w", c.Key(), err)
		}
	}

	return nil
}
