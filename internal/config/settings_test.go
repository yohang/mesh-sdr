package config

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// violationCode is the code DecodeSetting gives a value: of its first
// violation, or of an unknown key.
func violationCode(t *testing.T, key, raw string) string {
	t.Helper()

	_, vs, err := DecodeSetting(key, []byte(raw))

	switch {
	case errors.Is(err, ErrUnknownSetting):
		return "unknown_setting"
	case err != nil:
		t.Fatal(err)
	case len(vs) > 0:
		return string(vs[0].Code())
	}

	return ""
}

func TestDecodeSetting(t *testing.T) {
	tests := []struct {
		key, raw, code string // code "" = valid
	}{
		{"ui.theme_mode", `"dark"`, ""},
		{"ui.theme_mode", `"sepia"`, CodeInvalidValue},
		{"ui.theme_mode", `3`, CodeInvalidType},
		{"ui.recorder_enabled", `false`, ""},
		{"ui.recorder_enabled", `"no"`, CodeInvalidType},
		{"grid.heartbeat_interval_s", `300`, ""},
		{"grid.heartbeat_interval_s", `301`, CodeInvalidValue},
		{"grid.heartbeat_interval_s", `1.5`, CodeInvalidType},
		{"grid.heartbeat_interval_s", `"2"`, CodeInvalidType},
		{"retention.audit_log", `"30d"`, ""},
		{"retention.audit_log", `"29d"`, CodeInvalidValue},
		{"retention.audit_log", `"soon"`, CodeInvalidValue},
		{"session.idle_timeout", `"1h30m"`, ""},
		{"receiver.name", `""`, CodeRequired},
		{"receiver.name", `"` + strings.Repeat("a", 65) + `"`, CodeInvalidValue},
		{"receiver.help_url", `"https://example.org/help"`, ""},
		{"receiver.help_url", `"javascript:alert(1)"`, CodeInvalidValue},
		{"receiver.usage_policy_url", `"/policy"`, ""},
		{"receiver.usage_policy_url", `"//evil.example"`, CodeInvalidValue},
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
		{"wfm_deemphasis", `75`, ""},
		{"audio_compression", `"pcm"`, ""},
		{"audio_compression", `"opus"`, CodeInvalidValue},
		{"wfm_deemphasis", `60`, CodeInvalidValue},
		{"bandplan.region", `"r3"`, ""},
		{"bandplan.region", `"r4"`, CodeInvalidValue},
		{"no.such_key", `1`, "unknown_setting"},
	}

	for _, tt := range tests {
		t.Run(tt.key+"="+tt.raw[:min(len(tt.raw), 24)], func(t *testing.T) {
			if got := violationCode(t, tt.key, tt.raw); got != tt.code {
				t.Errorf("code = %q, want %q", got, tt.code)
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
	if p.Key != "settings.retention.audit_log" || p.Origin != "hub.toml" {
		t.Errorf("problem = %+v", p)
	}

	if got := violationCode(t, "retention.audit_log", `"7d"`); got != p.Code {
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
		{"offline within two heartbeats", "[settings.grid]\nheartbeat_interval_s = 10\noffline_after_s = 20\n", true},
		{"offline after two heartbeats", "[settings.grid]\nheartbeat_interval_s = 10\noffline_after_s = 21\n", false},
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
