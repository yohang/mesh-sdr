package wire_test

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func (b *browser) form(path string, values url.Values, htmx bool) (*http.Response, string) {
	b.h.t.Helper()

	header := map[string]string{}
	if htmx {
		header["HX-Request"] = "true"
	}

	res, body := b.do(http.MethodPost, path, "application/x-www-form-urlencoded", values.Encode(), header)

	return res, string(body)
}

// version reads the version a form section carries for key.
func version(t *testing.T, body, key string) string {
	t.Helper()

	m := regexp.MustCompile(`name="version\.` + regexp.QuoteMeta(key) + `" value="(\d+)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no version of %s", key)
	}

	return m[1]
}

func TestAdminPagesAccess(t *testing.T) {
	h := newAdminHub(t, nil)

	res, _ := h.browser("").do(http.MethodGet, "/admin/site", "", "", nil)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/login?next=%2Fadmin%2Fsite" {
		t.Errorf("anonymous = %d %s", res.StatusCode, res.Header.Get("Location"))
	}

	if res, _ := h.browser("op").do(http.MethodGet, "/admin", "", "", nil); res.StatusCode != http.StatusForbidden {
		t.Errorf("operator = %d", res.StatusCode)
	}

	if _, home := h.browser("lis").do(http.MethodGet, "/", "", "", nil); strings.Contains(string(home), `href="/admin"`) {
		t.Error("listener sees the Admin link")
	}

	b := h.browser("root")

	if _, home := b.do(http.MethodGet, "/", "", "", nil); !strings.Contains(string(home), `href="/admin"`) {
		t.Errorf("admin does not see the Admin link: %s", home)
	}

	for path, want := range map[string]string{
		"/admin":               "<h1 class=\"text-2xl font-semibold\">Administration</h1>",
		"/admin/site":          `name="receiver.name"`,
		"/admin/access":        "0.0.0.0/0, ::/0",
		"/admin/look-and-feel": `name="ui.theme_mode"`,
		"/admin/retention":     "Purge now",
		"/admin/system":        "settings.ui.theme_mode",
	} {
		res, body := b.do(http.MethodGet, path, "", "", nil)
		if res.StatusCode != http.StatusOK || !strings.Contains(string(body), want) {
			t.Errorf("GET %s = %d, misses %q", path, res.StatusCode, want)
		}

		if !strings.Contains(string(body), `aria-current="page"`) || res.Header.Get("X-Robots-Tag") != "noindex" {
			t.Errorf("GET %s: no current section or robots header", path)
		}
	}
}

func TestAdminFormSave(t *testing.T) {
	h := newAdminHub(t, nil)
	b := h.browser("root")

	// Save the theme: the fragment comes back with the notice.
	res, body := b.form("/admin/look-and-feel", url.Values{
		"section": {"theme"}, "ui.theme_mode": {"dark"}, "version.ui.theme_mode": {"0"},
	}, true)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "Saved.") || strings.Contains(body, "<html") {
		t.Fatalf("save = %d %s", res.StatusCode, body)
	}

	if _, home := b.do(http.MethodGet, "/", "", "", nil); !strings.Contains(string(home), `data-theme="dark"`) {
		t.Error("theme not applied")
	}

	// Unchanged values are not written.
	if _, body = b.form("/admin/look-and-feel", url.Values{"section": {"theme"}, "ui.theme_mode": {"dark"}, "version.ui.theme_mode": {"1"}}, true); !strings.Contains(body, "No changes to save.") {
		t.Errorf("unchanged = %s", body)
	}

	// Display defaults: only the changed checkbox becomes a DB row.
	res, body = b.form("/admin/look-and-feel", url.Values{
		"section": {"display"}, "ui.shortcut_set": {"default"}, "ui.tuning_precision": {"2"},
		"ui.layout.side_panel_open": {"false"}, "ui.layout.spectrum": {"false", "true"}, "ui.layout.bandplan": {"false", "true"},
		"ui.layout.default_tab": {"decoders"}, "ui.layout.frequency_format": {"radio"},
	}, true)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "Saved.") {
		t.Fatalf("display = %d %s", res.StatusCode, body)
	}

	if n := h.count("SELECT count(*) FROM settings WHERE key LIKE 'ui.layout.%'"); n != 2 {
		t.Errorf("layout rows = %d, want 2 (side panel closed, spectrum shown)", n)
	}

	// Invalid values: 422 with inline errors and a summary.
	res, body = b.form("/admin/access", url.Values{
		"section": {"sessions"}, "session.idle_timeout": {"1m"}, "session.absolute_timeout": {"24h"}, "session.remember_me_timeout": {"30d"},
	}, true)
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Must be at least 5m.") ||
		!strings.Contains(body, `aria-invalid="true"`) || !strings.Contains(body, `href="#f-session-idle_timeout"`) {
		t.Errorf("invalid = %d %s", res.StatusCode, body)
	}

	// A position, then a reset by emptying the fields.
	station := url.Values{
		"section": {"station"}, "receiver.name": {"F4XYZ"}, "receiver.gps.lat": {"50.63"}, "receiver.gps.lon": {"3.06"},
		"receiver.altitude_m": {"0"}, "bandplan.region": {"0"},
	}
	if res, body = b.form("/admin/site", station, true); res.StatusCode != http.StatusOK {
		t.Fatalf("station = %d %s", res.StatusCode, body)
	}

	if !strings.Contains(body, `value="50.63"`) {
		t.Errorf("saved position not shown: %s", body)
	}

	station.Set("receiver.name", "")
	station.Set("receiver.gps.lat", "")
	station.Set("receiver.gps.lon", "")
	station.Set("version.receiver.name", version(t, body, "receiver.name"))
	station.Set("version.receiver.gps", version(t, body, "receiver.gps"))

	if res, body = b.form("/admin/site", station, true); res.StatusCode != http.StatusOK || !strings.Contains(body, `value="MeshSDR"`) {
		t.Errorf("reset = %d %s", res.StatusCode, body)
	}

	// Stale version: 409.
	if res, body = b.form("/admin/look-and-feel", url.Values{"section": {"theme"}, "ui.theme_mode": {"light"}, "version.ui.theme_mode": {"0"}}, true); res.StatusCode != http.StatusConflict || !strings.Contains(body, "changed meanwhile") {
		t.Errorf("stale = %d %s", res.StatusCode, body)
	}

	if n := h.count("SELECT count(*) FROM audit_log WHERE action LIKE 'settings.%'"); n < 4 {
		t.Errorf("audit rows = %d", n)
	}
}

func TestAdminLockedField(t *testing.T) {
	h := newAdminHub(t, map[string]string{"MESHSDR_SETTINGS__UI__THEME_MODE": "light"})
	b := h.browser("root")

	_, page := b.do(http.MethodGet, "/admin/look-and-feel", "", "", nil)
	for _, want := range []string{"readonly", "Locked: set by the environment variable MESHSDR_SETTINGS__UI__THEME_MODE."} {
		if !strings.Contains(string(page), want) {
			t.Errorf("locked theme misses %q", want)
		}
	}

	// A forged submission of a locked key changes nothing: locked fields
	// are not read from the form.
	if _, body := b.form("/admin/look-and-feel", url.Values{"section": {"theme"}, "ui.theme_mode": {"dark"}}, true); !strings.Contains(body, "No changes to save.") {
		t.Errorf("locked submission = %s", body)
	}
}

func TestAdminPurgeNow(t *testing.T) {
	h := newAdminHub(t, nil)
	b := h.browser("root")

	res, body := b.form("/admin/retention/purge", url.Values{"store": {"audit_log"}}, true)
	if res.StatusCode != http.StatusOK || !strings.Contains(body, "Purged audit_log: 0 rows deleted.") || !strings.Contains(body, `id="retention-stores"`) {
		t.Errorf("purge = %d %s", res.StatusCode, body)
	}

	if res, _ := b.form("/admin/retention/purge", url.Values{"store": {"files"}}, true); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown store = %d", res.StatusCode)
	}
}
