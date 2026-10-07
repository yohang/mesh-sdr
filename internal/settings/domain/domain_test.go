package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/settings/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

func TestKey(t *testing.T) {
	for _, ok := range []string{"listen_policy", "ui.theme_mode", "auth.lockout.lock_for"} {
		if _, err := domain.NewKey(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}

	for _, bad := range []string{"", "UI.theme", "ui..x", ".ui", "ui.", "1ui", "ui.theme-mode", string(make([]byte, 161))} {
		if _, err := domain.NewKey(bad); !errors.Is(err, domain.ErrInvalidKey) {
			t.Errorf("%q: err = %v", bad, err)
		}
	}

	if k := domain.MustKey("ui.theme_mode"); k.ConfigKey() != "settings.ui.theme_mode" {
		t.Errorf("config key = %s", k.ConfigKey())
	}
}

func TestValue(t *testing.T) {
	v, err := domain.NewValue([]byte(" { \"lat\" : 1 } "))
	if err != nil || v.String() != `{"lat":1}` {
		t.Errorf("value = %s, %v", v, err)
	}

	if _, err := domain.NewValue([]byte("{")); !errors.Is(err, domain.ErrInvalidValue) {
		t.Errorf("invalid JSON: %v", err)
	}

	if _, err := domain.NewValue(nil); !errors.Is(err, domain.ErrInvalidValue) {
		t.Errorf("empty: %v", err)
	}

	if !domain.MustValue("null").IsNull() || (domain.Value{}).String() != "null" {
		t.Error("null value")
	}
}

func definition(t *testing.T) domain.Definition {
	t.Helper()

	d, err := domain.NewDefinition(domain.DefinitionSpec{
		Key: domain.MustKey("ui.theme_mode"), Default: domain.MustValue(`"auto"`), Apply: domain.ApplyLive,
		Input: domain.Input{Kind: domain.InputEnum, Options: []string{"light", "dark", "auto"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	return d
}

func TestDefinition(t *testing.T) {
	d := definition(t)
	if d.Label() != "ui.theme_mode" || d.Apply() != domain.ApplyLive {
		t.Errorf("definition = %+v", d)
	}

	bad := []domain.DefinitionSpec{
		{Key: domain.MustKey("a"), Apply: "later", Input: domain.Input{Kind: domain.InputText}},
		{Key: domain.MustKey("a"), Apply: domain.ApplyLive, Input: domain.Input{Kind: "slider"}},
		{Key: domain.MustKey("a"), Apply: domain.ApplyLive, Input: domain.Input{Kind: domain.InputEnum}},
		{Key: domain.MustKey("a"), Apply: domain.ApplyLive, Secret: true, Public: true, Input: domain.Input{Kind: domain.InputText}},
		{Apply: domain.ApplyLive, Input: domain.Input{Kind: domain.InputText}},
	}

	for i, s := range bad {
		if _, err := domain.NewDefinition(s); err == nil {
			t.Errorf("spec %d accepted", i)
		}
	}
}

var t0 = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func TestSetting(t *testing.T) {
	k := domain.MustKey("ui.theme_mode")

	s, err := domain.NewSetting(k, domain.MustValue(`"dark"`), 3, shared.UUID{}, t0)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Replace(domain.MustValue(`"light"`), 3, shared.UUID{}, t0); err == nil {
		t.Error("replace at the same revision accepted")
	}

	if err := s.Replace(domain.MustValue(`"light"`), 4, shared.UUID{}, t0); err != nil || s.Version() != 4 || s.Value().String() != `"light"` {
		t.Errorf("replace: %v, %d %s", err, s.Version(), s.Value())
	}

	for _, bad := range []func() error{
		func() error {
			_, err := domain.NewSetting(domain.Key{}, domain.MustValue("1"), 1, shared.UUID{}, t0)
			return err
		},
		func() error {
			_, err := domain.NewSetting(k, domain.MustValue("null"), 1, shared.UUID{}, t0)
			return err
		},
		func() error { _, err := domain.NewSetting(k, domain.MustValue("1"), 0, shared.UUID{}, t0); return err },
		func() error {
			_, err := domain.NewSetting(k, domain.MustValue("1"), 1, shared.UUID{}, time.Time{})
			return err
		},
	} {
		if err := bad(); !errors.Is(err, domain.ErrInvalidSettingRow) {
			t.Errorf("err = %v", err)
		}
	}
}

func TestResolve(t *testing.T) {
	d := definition(t)

	row, err := domain.NewSetting(d.Key(), domain.MustValue(`"dark"`), 7, shared.UUID{}, t0)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &domain.Configured{Value: domain.MustValue(`"light"`), Origin: "hub.toml"}

	tests := []struct {
		name     string
		cfg      *domain.Configured
		row      *domain.Setting
		value    string
		source   domain.Source
		origin   string
		version  int64
		shadowed bool
	}{
		{"default", nil, nil, `"auto"`, domain.SourceDefault, "default", 0, false},
		{"db", nil, row, `"dark"`, domain.SourceDB, "db", 7, false},
		{"config", cfg, nil, `"light"`, domain.SourceConfig, "hub.toml", 0, false},
		{"config over db", cfg, row, `"light"`, domain.SourceConfig, "hub.toml", 7, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := domain.Resolve(d, tt.cfg, tt.row)
			_, shadowed := e.Shadowed()

			if e.Value().String() != tt.value || e.Source() != tt.source || e.Origin() != tt.origin ||
				e.Version() != tt.version || shadowed != tt.shadowed || e.Locked() != (tt.cfg != nil) {
				t.Errorf("effective = %s %s %s v%d shadowed %v", e.Value(), e.Source(), e.Origin(), e.Version(), shadowed)
			}
		})
	}
}

func TestChangeSet(t *testing.T) {
	k := domain.MustKey("ui.theme_mode")

	if _, err := domain.NewChangeSet(); !errors.Is(err, domain.ErrInvalidSetting) {
		t.Errorf("empty: %v", err)
	}

	if _, err := domain.NewChangeSet(domain.ResetTo(k, 0), domain.ResetTo(k, 0)); !errors.Is(err, domain.ErrInvalidSetting) {
		t.Errorf("duplicate: %v", err)
	}

	s, err := domain.NewChangeSet(domain.SetTo(k, domain.MustValue(`"dark"`), 2), domain.ResetTo(domain.MustKey("listen_policy"), 0))
	if err != nil {
		t.Fatal(err)
	}

	c := s.Changes()
	if v, ok := c[0].Value(); !ok || v.String() != `"dark"` || c[0].Expected() != 2 || !c[1].IsReset() {
		t.Errorf("changes = %+v", c)
	}
}
