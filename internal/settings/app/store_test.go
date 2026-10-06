package app_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/settings/app"
	"github.com/yohang/mesh-sdr/internal/settings/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// memRepo is an in-memory repository whose transactions roll back on error.
type memRepo struct {
	mu   sync.Mutex
	rows map[string]*domain.Setting
	rev  int64
}

func newMemRepo() *memRepo { return &memRepo{rows: map[string]*domain.Setting{}} }

func (r *memRepo) List(context.Context) ([]*domain.Setting, error) {
	out := make([]*domain.Setting, 0, len(r.rows))
	for _, s := range r.rows {
		c := *s
		out = append(out, &c)
	}

	return out, nil
}

func (r *memRepo) Get(_ context.Context, k domain.Key) (*domain.Setting, error) {
	s, ok := r.rows[k.String()]
	if !ok {
		return nil, nil //nolint:nilnil // no row
	}

	c := *s

	return &c, nil
}

func (r *memRepo) Save(_ context.Context, s *domain.Setting) error {
	c := *s
	r.rows[s.Key().String()] = &c

	return nil
}

func (r *memRepo) Delete(_ context.Context, k domain.Key) error {
	delete(r.rows, k.String())

	return nil
}

func (r *memRepo) NextRevision(context.Context) (int64, error) { r.rev++; return r.rev, nil }
func (r *memRepo) Revision(context.Context) (int64, error)     { return r.rev, nil }

func (r *memRepo) WithinTx(ctx context.Context, fn func(context.Context) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	rows, rev := maps.Clone(r.rows), r.rev
	if err := fn(ctx); err != nil {
		r.rows, r.rev = rows, rev

		return err
	}

	return nil
}

type memAudit struct{ records []app.AuditRecord }

func (a *memAudit) Record(_ context.Context, r app.AuditRecord) error {
	a.records = append(a.records, r)

	return nil
}

var (
	t0    = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	admin = shared.MustParseUUID("0192c3a4-5b6c-7d8e-9f01-23456789abcd")
)

type fixture struct {
	store *app.Store
	repo  *memRepo
	audit *memAudit
}

func newFixture(t *testing.T, env map[string]string) fixture {
	t.Helper()

	cfg := config.DefaultHub()
	origins := config.Origins{}

	if len(env) > 0 {
		dir := t.TempDir()
		if err := writeHub(dir); err != nil {
			t.Fatal(err)
		}

		var (
			meta config.Meta
			err  error
		)

		cfg, meta, err = config.LoadHub(config.Options{Dir: dir, Env: env})
		if err != nil {
			t.Fatal(err)
		}

		origins = meta.Origins
	}

	cat, err := config.NewSettingsCatalog(cfg, origins)
	if err != nil {
		t.Fatal(err)
	}

	f := fixture{repo: newMemRepo(), audit: &memAudit{}}
	f.store = app.NewStore(app.StoreDeps{
		Repo: f.repo, Catalog: cat, Tx: f.repo, Audit: f.audit, Now: func() time.Time { return t0 },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	if err := f.store.Load(context.Background()); err != nil {
		t.Fatal(err)
	}

	return f
}

func set(t *testing.T, kv ...any) domain.ChangeSet {
	t.Helper()

	var changes []domain.Change

	for i := 0; i < len(kv); i += 3 {
		k := domain.MustKey(kv[i].(string))
		version := int64(kv[i+2].(int))

		if kv[i+1] == nil {
			changes = append(changes, domain.ResetTo(k, version))

			continue
		}

		changes = append(changes, domain.SetTo(k, domain.MustValue(kv[i+1].(string)), version))
	}

	s, err := domain.NewChangeSet(changes...)
	if err != nil {
		t.Fatal(err)
	}

	return s
}

func effective(t *testing.T, s *app.Snapshot, key string) domain.Effective {
	t.Helper()

	e, ok := s.Get(key)
	if !ok {
		t.Fatalf("no setting %s", key)
	}

	return e
}

func TestStoreDefaultsAndTypedValues(t *testing.T) {
	f := newFixture(t, nil)
	s := f.store.Snapshot()

	if e := effective(t, s, "ui.theme_mode"); e.Source() != domain.SourceDefault || e.Value().String() != `"auto"` || e.Locked() {
		t.Errorf("theme = %s %s", e.Value(), e.Source())
	}

	if s.String("receiver.name") != "MeshSDR" || !s.Bool("ui.recorder_enabled") || s.Int("ui.tuning_precision") != 2 {
		t.Error("typed defaults")
	}

	if s.Duration("session.remember_me_timeout") != 30*24*time.Hour || s.Duration("retention.audit_log") != 365*24*time.Hour {
		t.Error("durations")
	}

	if n, w := s.Rate("auth.login_rate_limit"); n != 5 || w != time.Minute {
		t.Errorf("rate = %d/%s", n, w)
	}

	if _, _, ok := s.Geo("receiver.gps"); ok {
		t.Error("gps set by default")
	}
}

func TestStoreUpdateAudited(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()

	var published *app.Snapshot

	f.store.Subscribe(func(s *app.Snapshot) { published = s })

	actor := app.Actor{User: admin, RequestID: "req-1"}

	snap, err := f.store.Apply(ctx, actor, set(t, "ui.theme_mode", `"dark"`, 0, "receiver.gps", `{"lat":50.5,"lon":3}`, 0))
	if err != nil {
		t.Fatal(err)
	}

	e := effective(t, snap, "ui.theme_mode")
	if e.Source() != domain.SourceDB || e.Value().String() != `"dark"` || e.Version() != 1 || snap.Revision() != 1 {
		t.Errorf("theme = %s %s v%d rev %d", e.Value(), e.Source(), e.Version(), snap.Revision())
	}

	if lat, lon, ok := snap.Geo("receiver.gps"); !ok || lat != 50.5 || lon != 3 {
		t.Errorf("gps = %v %v %v", lat, lon, ok)
	}

	if published != snap || f.store.Snapshot() != snap {
		t.Error("snapshot not published")
	}

	if len(f.audit.records) != 2 {
		t.Fatalf("audit = %+v", f.audit.records)
	}

	r := f.audit.records[0]
	if r.Action != app.ActionUpdate || r.Key != "ui.theme_mode" || r.Result != app.ResultOK || r.After != `"dark"` || r.Before != "" || r.Actor != actor {
		t.Errorf("audit = %+v", r)
	}
}

func TestStoreVersionConflict(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()

	if _, err := f.store.Apply(ctx, app.Actor{User: admin}, set(t, "ui.theme_mode", `"dark"`, 0)); err != nil {
		t.Fatal(err)
	}

	// Reset then write again: the version is new, so a writer that saw
	// version 1 still conflicts (no ABA).
	if _, err := f.store.Apply(ctx, app.Actor{User: admin}, set(t, "ui.theme_mode", nil, 1)); err != nil {
		t.Fatal(err)
	}

	if _, err := f.store.Apply(ctx, app.Actor{User: admin}, set(t, "ui.theme_mode", `"light"`, 0)); err != nil {
		t.Fatal(err)
	}

	audits := len(f.audit.records)

	_, err := f.store.Apply(ctx, app.Actor{User: admin}, set(t, "ui.shortcut_set", `"off"`, 0, "ui.theme_mode", `"dark"`, 1))
	if !errors.Is(err, domain.ErrVersionConflict) {
		t.Fatalf("err = %v", err)
	}

	var de *shared.Error
	if errors.As(err, &de); len(de.Violations()) != 1 || de.Violations()[0].Path() != "ui.theme_mode" {
		t.Errorf("violations = %v", de.Violations())
	}

	s := f.store.Snapshot()
	if effective(t, s, "ui.shortcut_set").Source() != domain.SourceDefault || len(f.audit.records) != audits {
		t.Error("a conflicting change set was partly written")
	}

	if v := effective(t, s, "ui.theme_mode").Version(); v != 3 {
		t.Errorf("version = %d, want 3", v)
	}
}

func TestStoreLockedKey(t *testing.T) {
	f := newFixture(t, map[string]string{"MESHSDR_SETTINGS__UI__THEME_MODE": "light"})
	ctx := context.Background()

	e := effective(t, f.store.Snapshot(), "ui.theme_mode")
	if !e.Locked() || e.Origin() != "env:MESHSDR_SETTINGS__UI__THEME_MODE" {
		t.Fatalf("theme = %s %s", e.Source(), e.Origin())
	}

	_, err := f.store.Apply(ctx, app.Actor{User: admin}, set(t, "ui.theme_mode", `"dark"`, 0, "ui.shortcut_set", `"off"`, 0))
	if !errors.Is(err, domain.ErrSettingLocked) || !strings.Contains(err.Error(), "env:MESHSDR_SETTINGS__UI__THEME_MODE") {
		t.Fatalf("err = %v", err)
	}

	if len(f.audit.records) != 1 || f.audit.records[0].Result != app.ResultDenied || f.audit.records[0].Key != "ui.theme_mode" {
		t.Errorf("audit = %+v", f.audit.records)
	}

	if effective(t, f.store.Snapshot(), "ui.shortcut_set").Source() != domain.SourceDefault {
		t.Error("unlocked key of a refused change set written")
	}
}

func TestStoreInvalidValues(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()

	_, err := f.store.Apply(ctx, app.Actor{User: admin}, set(t,
		"ui.theme_mode", `"sepia"`, 0, "no.such_key", `1`, 0, "retention.audit_log", `"7d"`, 0, "receiver.name", `"F4XYZ"`, 0))
	if !errors.Is(err, domain.ErrInvalidSetting) {
		t.Fatalf("err = %v", err)
	}

	var de *shared.Error

	errors.As(err, &de)

	paths := map[string]string{}
	for _, v := range de.Violations() {
		paths[v.Path()] = string(v.Code())
	}

	want := map[string]string{"ui.theme_mode": "invalid_value", "no.such_key": "unknown_setting", "retention.audit_log": "invalid_value"}
	if !maps.Equal(paths, want) {
		t.Errorf("violations = %v", paths)
	}

	if len(f.repo.rows) != 0 || len(f.audit.records) != 0 {
		t.Error("an invalid change set was written")
	}

	// Checks across keys use the prospective values.
	_, err = f.store.Apply(ctx, app.Actor{User: admin}, set(t, "auth.lockout.lock_after", `3`, 0))
	if !errors.Is(err, domain.ErrInvalidSetting) {
		t.Errorf("lock_after below delay_after: %v", err)
	}

	if _, err := f.store.Apply(ctx, app.Actor{User: admin}, set(t, "auth.lockout.lock_after", `3`, 0, "auth.lockout.delay_after", `2`, 0)); err != nil {
		t.Errorf("valid combination: %v", err)
	}
}

func TestStoreReset(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()

	if _, err := f.store.Apply(ctx, app.Actor{User: admin}, set(t, "receiver.name", `"F4XYZ"`, 0)); err != nil {
		t.Fatal(err)
	}

	snap, err := f.store.Apply(ctx, app.Actor{User: admin}, set(t, "receiver.name", nil, 1, "ui.shortcut_set", nil, 0))
	if err != nil {
		t.Fatal(err)
	}

	if e := effective(t, snap, "receiver.name"); e.Source() != domain.SourceDefault || snap.String("receiver.name") != "MeshSDR" {
		t.Errorf("name = %s %s", e.Value(), e.Source())
	}

	if len(f.audit.records) != 2 {
		t.Fatalf("audit = %+v", f.audit.records)
	}

	if r := f.audit.records[1]; r.Action != app.ActionReset || r.Before != `"F4XYZ"` || r.After != `"MeshSDR"` {
		t.Errorf("reset audit = %+v", r)
	}
}

func TestStoreLoadIgnoresInvalidRows(t *testing.T) {
	f := newFixture(t, map[string]string{"MESHSDR_SETTINGS__UI__THEME_MODE": "light"})
	ctx := context.Background()

	for key, raw := range map[string]string{
		"ui.theme_mode":       `"dark"`,
		"ui.shortcut_set":     `"vim"`,
		"retired.key":         `true`,
		"ui.tuning_precision": `4`,
	} {
		s, _ := domain.NewSetting(domain.MustKey(key), domain.MustValue(raw), 1, shared.UUID{}, t0)
		_ = f.repo.Save(ctx, s)
	}

	f.repo.rev = 1

	if err := f.store.Load(ctx); err != nil {
		t.Fatal(err)
	}

	s := f.store.Snapshot()

	if e := effective(t, s, "ui.shortcut_set"); e.Source() != domain.SourceDefault {
		t.Errorf("invalid row used: %s", e.Source())
	}

	if s.Int("ui.tuning_precision") != 4 {
		t.Error("valid row not used")
	}

	theme := effective(t, s, "ui.theme_mode")
	if v, ok := theme.Shadowed(); !ok || v.String() != `"dark"` || theme.Value().String() != `"light"` {
		t.Errorf("shadowed = %s %v", v, ok)
	}

	if len(f.audit.records) != 1 || f.audit.records[0].Action != app.ActionIgnored || f.audit.records[0].Key != "ui.shortcut_set" {
		t.Errorf("audit = %+v", f.audit.records)
	}
}

func writeHub(dir string) error {
	return os.WriteFile(filepath.Join(dir, "hub.toml"), []byte("schema_version = 1\n[hub]\nurl = \"https://sdr.example.org\"\n"), 0o600)
}
