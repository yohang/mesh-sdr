package settings_test

import (
	"errors"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/settings"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

func TestKey(t *testing.T) {
	for _, ok := range []string{"listen_policy", "ui.theme_mode", "auth.lockout.lock_for"} {
		if !settings.ValidKey(ok) {
			t.Errorf("%q rejected", ok)
		}
	}

	for _, bad := range []string{"", "UI.theme", "ui..x", ".ui", "ui.", "1ui", "ui.theme-mode", string(make([]byte, 161))} {
		if settings.ValidKey(bad) {
			t.Errorf("%q accepted", bad)
		}
	}

	if k := settings.ConfigKey("ui.theme_mode"); k != "settings.ui.theme_mode" {
		t.Errorf("config key = %s", k)
	}
}

func TestValue(t *testing.T) {
	v, err := settings.NewValue([]byte(" { \"lat\" : 1 } "))
	if err != nil || v.String() != `{"lat":1}` {
		t.Errorf("value = %s, %v", v, err)
	}

	if _, err := settings.NewValue([]byte("{")); !errors.Is(err, settings.ErrInvalidValue) {
		t.Errorf("invalid JSON: %v", err)
	}

	if _, err := settings.NewValue(nil); !errors.Is(err, settings.ErrInvalidValue) {
		t.Errorf("empty: %v", err)
	}

	if !settings.MustValue("null").IsNull() || (settings.Value{}).String() != "null" {
		t.Error("null value")
	}
}

func definition(t *testing.T) settings.Definition {
	t.Helper()

	d, err := settings.NewDefinition(settings.DefinitionSpec{
		Key: "ui.theme_mode", Default: settings.MustValue(`"auto"`), Apply: settings.ApplyLive,
		Input: settings.Input{Kind: settings.InputEnum, Options: []string{"light", "dark", "auto"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	return d
}

func TestDefinition(t *testing.T) {
	d := definition(t)
	if d.Label() != "ui.theme_mode" || d.Apply() != settings.ApplyLive {
		t.Errorf("definition = %+v", d)
	}

	bad := []settings.DefinitionSpec{
		{Key: "a", Apply: "later", Input: settings.Input{Kind: settings.InputText}},
		{Key: "a", Apply: settings.ApplyLive, Input: settings.Input{Kind: "slider"}},
		{Key: "a", Apply: settings.ApplyLive, Input: settings.Input{Kind: settings.InputEnum}},
		{Key: "a", Apply: settings.ApplyLive, Secret: true, Public: true, Input: settings.Input{Kind: settings.InputText}},
		{Apply: settings.ApplyLive, Input: settings.Input{Kind: settings.InputText}},
	}

	for i, s := range bad {
		if _, err := settings.NewDefinition(s); err == nil {
			t.Errorf("spec %d accepted", i)
		}
	}
}

func TestSetting(t *testing.T) {
	k := "ui.theme_mode"

	s, err := settings.NewSetting(k, settings.MustValue(`"dark"`), 3, shared.UUID{}, t0)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Replace(settings.MustValue(`"light"`), 3, shared.UUID{}, t0); err == nil {
		t.Error("replace at the same revision accepted")
	}

	if err := s.Replace(settings.MustValue(`"light"`), 4, shared.UUID{}, t0); err != nil || s.Version() != 4 || s.Value().String() != `"light"` {
		t.Errorf("replace: %v, %d %s", err, s.Version(), s.Value())
	}

	for _, bad := range []func() error{
		func() error {
			_, err := settings.NewSetting("", settings.MustValue("1"), 1, shared.UUID{}, t0)
			return err
		},
		func() error {
			_, err := settings.NewSetting(k, settings.MustValue("null"), 1, shared.UUID{}, t0)
			return err
		},
		func() error {
			_, err := settings.NewSetting(k, settings.MustValue("1"), 0, shared.UUID{}, t0)
			return err
		},
		func() error {
			_, err := settings.NewSetting(k, settings.MustValue("1"), 1, shared.UUID{}, time.Time{})
			return err
		},
	} {
		if err := bad(); !errors.Is(err, settings.ErrInvalidSettingRow) {
			t.Errorf("err = %v", err)
		}
	}
}

func TestResolve(t *testing.T) {
	d := definition(t)

	row, err := settings.NewSetting(d.Key(), settings.MustValue(`"dark"`), 7, shared.UUID{}, t0)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &settings.Configured{Value: settings.MustValue(`"light"`), Origin: "hub.toml"}

	tests := []struct {
		name    string
		cfg     *settings.Configured
		row     *settings.Setting
		value   string
		source  settings.Source
		origin  string
		version int64
	}{
		{"default", nil, nil, `"auto"`, settings.SourceDefault, "default", 0},
		{"db", nil, row, `"dark"`, settings.SourceDB, "db", 7},
		{"config", cfg, nil, `"light"`, settings.SourceConfig, "hub.toml", 0},
		{"config over db", cfg, row, `"light"`, settings.SourceConfig, "hub.toml", 7},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := settings.Resolve(d, tt.cfg, tt.row)

			if e.Value().String() != tt.value || e.Source() != tt.source || e.Origin() != tt.origin ||
				e.Version() != tt.version || e.Locked() != (tt.cfg != nil) {
				t.Errorf("effective = %s %s %s v%d", e.Value(), e.Source(), e.Origin(), e.Version())
			}
		})
	}
}

func TestChangeSet(t *testing.T) {
	k := "ui.theme_mode"

	if _, err := settings.NewChangeSet(); !errors.Is(err, settings.ErrInvalidSetting) {
		t.Errorf("empty: %v", err)
	}

	if _, err := settings.NewChangeSet(settings.ResetTo(k, 0), settings.ResetTo(k, 0)); !errors.Is(err, settings.ErrInvalidSetting) {
		t.Errorf("duplicate: %v", err)
	}

	s, err := settings.NewChangeSet(settings.SetTo(k, settings.MustValue(`"dark"`), 2), settings.ResetTo("listen_policy", 0))
	if err != nil {
		t.Fatal(err)
	}

	c := s.Changes()
	if _, ok := c[1].Value(); ok {
		t.Errorf("reset carries a value: %+v", c[1])
	}

	if v, ok := c[0].Value(); !ok || v.String() != `"dark"` || c[0].Expected() != 2 {
		t.Errorf("changes = %+v", c)
	}
}
