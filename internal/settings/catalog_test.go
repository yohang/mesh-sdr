package settings_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/settings"
)

// Every default is valid for the shared validator, alone and together.
func TestSettingsDefaultsAreValid(t *testing.T) {
	cat, err := settings.NewConfigCatalog(config.DefaultHub(), config.Origins{})
	if err != nil {
		t.Fatal(err)
	}

	values := map[string]any{}

	for _, d := range cat.Definitions() {
		if d.Default().IsNull() {
			continue
		}

		v, err := cat.Validate(d.Key(), d.Default())
		if err != nil {
			t.Errorf("default of %s (%s): %v", d.Key(), d.Default(), err)
		}

		values[d.Key()] = v
	}

	if vs := cat.Check(func(k string) (any, bool) { v, ok := values[k]; return v, ok }); len(vs) > 0 {
		t.Errorf("defaults break checks across keys: %v", vs)
	}
}

// TestEnumLabels: x-enum-labels names the options of an enum setting.
func TestEnumLabels(t *testing.T) {
	cat, err := settings.NewConfigCatalog(config.DefaultHub(), config.Origins{})
	if err != nil {
		t.Fatal(err)
	}

	for _, d := range cat.Definitions() {
		in := d.Input()

		switch d.Key() {
		case "bandplan.region":
			if strings.Join(in.Options, ",") != "r1,r2,r3" || strings.Join(in.OptionLabels, ",") != "R1,R2,R3" || d.Default().String() != `"r1"` {
				t.Errorf("bandplan.region = %v %v, default %s", in.Options, in.OptionLabels, d.Default())
			}
		case "ui.theme_mode":
			if in.OptionLabels != nil {
				t.Errorf("theme_mode labels = %v", in.OptionLabels)
			}
		}
	}
}

func TestSettingsCatalog(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hub.toml"), []byte("schema_version = 1\n[hub]\nurl = \"https://sdr.example.org\"\n"+
		"[settings.receiver]\nname = \"F4XYZ\"\ngps = { lat = 50.5, lon = 3 }\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, meta, err := config.LoadHub(config.Options{Dir: dir, Env: map[string]string{"MESHSDR_SETTINGS__UI__THEME_MODE": "dark"}})
	if err != nil {
		t.Fatal(err)
	}

	cat, err := settings.NewConfigCatalog(cfg, meta.Origins)
	if err != nil {
		t.Fatal(err)
	}

	for key, want := range map[string]settings.Configured{
		"receiver.name": {Value: settings.MustValue(`"F4XYZ"`), Origin: "hub.toml"},
		"receiver.gps":  {Value: settings.MustValue(`{"lat":50.5,"lon":3}`), Origin: "hub.toml"},
		"ui.theme_mode": {Value: settings.MustValue(`"dark"`), Origin: "env:MESHSDR_SETTINGS__UI__THEME_MODE"},
	} {
		got, ok := cat.Configured(key)
		if !ok || !got.Value.Equal(want.Value) || got.Origin != want.Origin {
			t.Errorf("%s = %+v %v, want %+v", key, got, ok, want)
		}
	}

	if _, ok := cat.Configured("ui.shortcut_set"); ok {
		t.Error("ui.shortcut_set is not set in the config")
	}

	kinds := map[string]settings.InputKind{}
	for _, d := range cat.Definitions() {
		kinds[d.Key()] = d.Input().Kind
	}

	for key, want := range map[string]settings.InputKind{
		"ui.theme_mode":              settings.InputEnum,
		"ui.recorder_enabled":        settings.InputBoolean,
		"grid.heartbeat_interval_s":  settings.InputInteger,
		"retention.audit_log":        settings.InputDuration,
		"auth.login_rate_limit":      settings.InputRate,
		"receiver.gps":               settings.InputGeo,
		"receiver.help_url":          settings.InputURL,
		"receiver.usage_policy_text": settings.InputMarkdown,
		"receiver.location":          settings.InputText,
	} {
		if kinds[key] != want {
			t.Errorf("input of %s = %s, want %s", key, kinds[key], want)
		}
	}

	var schema map[string]any
	if err := json.Unmarshal(cat.Schema(), &schema); err != nil {
		t.Fatal(err)
	}

	child := func(node map[string]any, name string) map[string]any {
		props, _ := node["properties"].(map[string]any)
		c, _ := props[name].(map[string]any)

		return c
	}

	theme := child(child(schema, "ui"), "theme_mode")
	if theme["lockable"] != true || theme["x-public"] != true || theme["x-apply"] != "live" || theme["default"] != "auto" {
		t.Errorf("theme_mode schema = %v", theme)
	}
}
