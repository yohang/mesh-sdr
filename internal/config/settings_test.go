package config

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	settingsdomain "github.com/yohang/mesh-sdr/internal/settings/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Every default is valid for the shared validator, alone and together.
func TestSettingsDefaultsAreValid(t *testing.T) {
	cat, err := NewSettingsCatalog(DefaultHub(), Origins{})
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

		values[d.Key().String()] = v
	}

	if vs := cat.Check(func(k string) (any, bool) { v, ok := values[k]; return v, ok }); len(vs) > 0 {
		t.Errorf("defaults break checks across keys: %v", vs)
	}
}

func violationCode(t *testing.T, err error) string {
	t.Helper()

	var de *shared.Error
	if !errors.As(err, &de) {
		t.Fatalf("error %v is not a domain error", err)
	}

	if !errors.Is(err, settingsdomain.ErrInvalidSetting) {
		return string(de.Code())
	}

	vs := de.Violations()
	if len(vs) == 0 {
		t.Fatalf("no violation in %v", err)
	}

	return string(vs[0].Code())
}

func TestValidateSetting(t *testing.T) {
	tests := []struct {
		key, raw, code string // code "" = valid
	}{
		{"ui.theme_mode", `"dark"`, ""},
		{"ui.theme_mode", `"sepia"`, CodeInvalidValue},
		{"ui.theme_mode", `3`, CodeInvalidType},
		{"ui.tuning_precision", `6`, ""},
		{"ui.tuning_precision", `7`, CodeInvalidValue},
		{"ui.tuning_precision", `1.5`, CodeInvalidType},
		{"ui.tuning_precision", `"2"`, CodeInvalidType},
		{"ui.recorder_enabled", `false`, ""},
		{"ui.recorder_enabled", `"no"`, CodeInvalidType},
		{"retention.audit_log", `"30d"`, ""},
		{"retention.audit_log", `"29d"`, CodeInvalidValue},
		{"retention.audit_log", `"soon"`, CodeInvalidValue},
		{"session.idle_timeout", `"1h30m"`, ""},
		{"receiver.name", `""`, CodeRequired},
		{"receiver.name", `"` + strings.Repeat("a", 65) + `"`, CodeInvalidValue},
		{"receiver.admin_email", `"admin@example.org"`, ""},
		{"receiver.admin_email", `""`, ""},
		{"receiver.admin_email", `"Admin <admin@example.org>"`, CodeInvalidValue},
		{"receiver.help_url", `"https://example.org/help"`, ""},
		{"receiver.help_url", `"javascript:alert(1)"`, CodeInvalidValue},
		{"receiver.usage_policy_url", `"/policy"`, ""},
		{"receiver.usage_policy_url", `"//evil.example"`, CodeInvalidValue},
		{"receiver.country", `"FR"`, ""},
		{"receiver.country", `"fr"`, CodeInvalidValue},
		{"receiver.gps", `{"lat": 50.63, "lon": 3.06}`, ""},
		{"receiver.gps", `{"lat": 91, "lon": 0}`, CodeInvalidValue},
		{"receiver.gps", `{"lat": 1, "lon": 2, "alt": 3}`, CodeInvalidType},
		{"receiver.gps", `"50,3"`, CodeInvalidType},
		{"receiver.usage_policy_text", `"# Rules"`, ""},
		{"receiver.usage_policy_text", `"` + strings.Repeat("é", 20001) + `"`, "invalid_usage_policy"},
		{"auth.login_rate_limit", `"10/1h"`, ""},
		{"auth.password_min_length", `12`, ""},
		{"auth.password_min_length", `7`, CodeInvalidValue},
		{"auth.password_min_length", `257`, CodeInvalidValue},
		{"auth.login_rate_limit", `"0/1m"`, CodeInvalidValue},
		{"auth.login_rate_limit", `"100/1m"`, ""},
		{"auth.login_rate_limit", `"101/1h"`, CodeInvalidValue},
		{"auth.login_rate_limit", `"5/30s"`, CodeInvalidValue},
		{"listen_policy", `"registered"`, ""},
		{"listen_policy", `"everyone"`, CodeInvalidValue},
		{"no.such_key", `1`, "unknown_setting"},
	}

	for _, tt := range tests {
		t.Run(tt.key+"="+tt.raw[:min(len(tt.raw), 24)], func(t *testing.T) {
			_, err := ValidateSetting(tt.key, []byte(tt.raw))

			switch {
			case tt.code == "" && err != nil:
				t.Fatalf("rejected: %v", err)
			case tt.code != "" && err == nil:
				t.Fatalf("accepted, want %s", tt.code)
			case tt.code != "":
				if got := violationCode(t, err); got != tt.code {
					t.Errorf("code = %s, want %s (%v)", got, tt.code, err)
				}
			}
		})
	}
}

// A value rejected in a file is rejected with the same code in the DB.
func TestSettingsSameValidatorForFileAndDB(t *testing.T) {
	dir := writeFiles(t, map[string]string{"hub.toml": minimalHub + "[settings.retention]\naudit_log = \"7d\"\n"})

	_, _, err := LoadHub(Options{Dir: dir, Env: map[string]string{}})

	var cerr *Error
	if !errors.As(err, &cerr) || len(cerr.Problems) != 1 {
		t.Fatalf("err = %v", err)
	}

	p := cerr.Problems[0]
	if p.Key != "settings.retention.audit_log" || p.Origin != "hub.toml:6" {
		t.Errorf("problem = %+v", p)
	}

	_, dbErr := ValidateSetting("retention.audit_log", []byte(`"7d"`))
	if got := violationCode(t, dbErr); got != p.Code {
		t.Errorf("DB code %s, file code %s", got, p.Code)
	}
}

func TestSettingsChecksAcrossKeys(t *testing.T) {
	tests := []struct {
		name, toml string
		bad        bool
	}{
		{"both set, invalid", "[settings.auth.lockout]\ndelay_after = 5\nlock_after = 3\n", true},
		{"only one key set", "[settings.auth.lockout]\nlock_after = 3\n", false},
		{"max lock shorter", "[settings.auth.lockout]\nlock_for = \"1h\"\nmax_lock = \"30m\"\n", true},
		{"valid", "[settings.auth.lockout]\ndelay_after = 3\nlock_after = 6\n", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeFiles(t, map[string]string{"hub.toml": minimalHub + tt.toml})

			_, _, err := LoadHub(Options{Dir: dir, Env: map[string]string{}})
			if (err != nil) != tt.bad {
				t.Errorf("err = %v, want error %v", err, tt.bad)
			}
		})
	}
}

func TestSettingsCatalog(t *testing.T) {
	dir := writeFiles(t, map[string]string{"hub.toml": minimalHub + "[settings.receiver]\nname = \"F4XYZ\"\ngps = { lat = 50.5, lon = 3 }\n"})

	cfg, meta, err := LoadHub(Options{Dir: dir, Env: map[string]string{"MESHSDR_SETTINGS__UI__THEME_MODE": "dark"}})
	if err != nil {
		t.Fatal(err)
	}

	cat, err := NewSettingsCatalog(cfg, meta.Origins)
	if err != nil {
		t.Fatal(err)
	}

	for key, want := range map[string]settingsdomain.Configured{
		"receiver.name": {Value: settingsdomain.MustValue(`"F4XYZ"`), Origin: "hub.toml:6"},
		"receiver.gps":  {Value: settingsdomain.MustValue(`{"lat":50.5,"lon":3}`), Origin: "hub.toml:7"},
		"ui.theme_mode": {Value: settingsdomain.MustValue(`"dark"`), Origin: "env:MESHSDR_SETTINGS__UI__THEME_MODE"},
	} {
		got, ok := cat.Configured(settingsdomain.MustKey(key))
		if !ok || !got.Value.Equal(want.Value) || got.Origin != want.Origin {
			t.Errorf("%s = %+v %v, want %+v", key, got, ok, want)
		}
	}

	if _, ok := cat.Configured(settingsdomain.MustKey("ui.shortcut_set")); ok {
		t.Error("ui.shortcut_set is not set in the config")
	}

	kinds := map[string]settingsdomain.InputKind{}
	for _, d := range cat.Definitions() {
		kinds[d.Key().String()] = d.Input().Kind
	}

	for key, want := range map[string]settingsdomain.InputKind{
		"ui.theme_mode":              settingsdomain.InputEnum,
		"ui.recorder_enabled":        settingsdomain.InputBoolean,
		"ui.tuning_precision":        settingsdomain.InputInteger,
		"retention.audit_log":        settingsdomain.InputDuration,
		"auth.login_rate_limit":      settingsdomain.InputRate,
		"receiver.gps":               settingsdomain.InputGeo,
		"receiver.admin_email":       settingsdomain.InputEmail,
		"receiver.help_url":          settingsdomain.InputURL,
		"receiver.usage_policy_text": settingsdomain.InputMarkdown,
		"receiver.location":          settingsdomain.InputText,
	} {
		if kinds[key] != want {
			t.Errorf("input of %s = %s, want %s", key, kinds[key], want)
		}
	}

	var schema map[string]any
	if err := json.Unmarshal(cat.Schema(), &schema); err != nil {
		t.Fatal(err)
	}

	theme := child(child(schema, "ui"), "theme_mode")
	if theme["lockable"] != true || theme["x-public"] != true || theme["x-apply"] != "live" || theme["default"] != "auto" {
		t.Errorf("theme_mode schema = %v", theme)
	}
}

func TestRateAndGeoPoint(t *testing.T) {
	r, err := ParseRate("5/1m")
	if err != nil || r.Count() != 5 || r.Window().Minutes() != 1 || r.String() != "5/1m" {
		t.Errorf("rate = %v %v", r, err)
	}

	for _, bad := range []string{"5", "0/1m", "5/0s", "5/2d", "x/1m"} {
		if _, err := ParseRate(bad); err == nil {
			t.Errorf("rate %q accepted", bad)
		}
	}

	var g GeoPoint
	if err := g.UnmarshalText([]byte("50.63, 3.06")); err != nil || g.Lat() != 50.63 || g.Lon() != 3.06 {
		t.Errorf("geo = %+v %v", g, err)
	}

	if b, _ := json.Marshal(GeoPoint{}); string(b) != "null" {
		t.Errorf("unset geo = %s", b)
	}
}
