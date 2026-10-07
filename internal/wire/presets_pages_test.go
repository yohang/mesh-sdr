package wire_test

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/schedules"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// TestPresetPagesAccess: Admin › Presets and Admin › Schedules are for
// admins only, and their forms need the CSRF token.
func TestPresetPagesAccess(t *testing.T) {
	h := newAdminHub(t, nil)
	create := url.Values{"name": {"2 m"}, "center_freq": {"145000000"}, "samp_rate": {"2048000"}}

	for _, tt := range []struct {
		user string
		want int
	}{{"", http.StatusSeeOther}, {"lis", http.StatusForbidden}, {"op", http.StatusForbidden}, {"root", http.StatusOK}} {
		b := h.browser(tt.user)

		for _, path := range []string{"/admin/presets", "/admin/presets/new", "/admin/schedules"} {
			if res, _ := b.do(http.MethodGet, path, "", "", nil); res.StatusCode != tt.want {
				t.Errorf("%q GET %s = %d, want %d", tt.user, path, res.StatusCode, tt.want)
			}
		}

		if tt.user == "root" {
			continue
		}

		if res, _ := b.form("/admin/presets", create, false); res.StatusCode != http.StatusForbidden &&
			!strings.HasPrefix(res.Header.Get("Location"), "/login") {
			t.Errorf("%q create = %d %s", tt.user, res.StatusCode, res.Header.Get("Location"))
		}
	}

	admin := h.browser("root")
	token := admin.token
	admin.token = "forged"

	if res, _ := admin.form("/admin/presets", create, false); res.StatusCode != http.StatusForbidden {
		t.Errorf("create without the CSRF token = %d", res.StatusCode)
	}

	admin.token = token

	if n := h.count("SELECT count(*) FROM presets"); n != 0 {
		t.Errorf("%d presets created by refused forms", n)
	}
}

// TestPresetPages: an admin creates, edits and deletes a preset with the
// HTML forms (ADM-017, ADR 0026); the schedules page lists the schedules.
func TestPresetPages(t *testing.T) {
	h := newAdminHub(t, nil)
	admin := h.browser("root")

	// Invalid input is answered on the form, field by field.
	res, body := admin.form("/admin/presets", url.Values{"name": {""}, "center_freq": {"abc"}, "samp_rate": {"2048000"}}, false)
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Centre frequency (Hz): Enter a whole number.") ||
		!strings.Contains(body, `aria-invalid="true"`) {
		t.Fatalf("invalid form = %d %s", res.StatusCode, body)
	}

	res, body = admin.form("/admin/presets", url.Values{"name": {""}, "center_freq": {"145000000"}, "samp_rate": {"2048000"}}, false)
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Name: Required.") {
		t.Fatalf("form without a name = %d %s", res.StatusCode, body)
	}

	res, _ = admin.form("/admin/presets", url.Values{
		"name": {"2 m FM"}, "center_freq": {"145000000"}, "samp_rate": {"2048000"}, "start_freq": {"145500000"},
		"start_mod": {"nfm"}, "tuning_step": {"12500"}, "initial_squelch_level": {"-60"}, "tags": {"vhf, fm"},
	}, false)

	loc := res.Header.Get("Location")
	if res.StatusCode != http.StatusSeeOther || !strings.HasSuffix(loc, "?done=created") {
		t.Fatalf("create = %d %s", res.StatusCode, loc)
	}

	edit := strings.TrimSuffix(loc, "?done=created")

	_, page := admin.do(http.MethodGet, loc, "", "", nil)
	for _, want := range []string{"Preset created.", `value="145500000"`, `value="12500"`, `value="fm, vhf"`, `name="version" value="1"`} {
		if !strings.Contains(string(page), want) {
			t.Errorf("edit page lacks %q", want)
		}
	}

	edited := url.Values{"version": {"1"}, "name": {"2 m FM calling"}, "center_freq": {"145000000"}, "samp_rate": {"2048000"}}
	if res, _ := admin.form(edit, edited, false); res.StatusCode != http.StatusSeeOther || !strings.HasSuffix(res.Header.Get("Location"), "?done=saved") {
		t.Fatalf("save = %d %s", res.StatusCode, res.Header.Get("Location"))
	}

	// A stale version is a conflict.
	if res, body := admin.form(edit, edited, false); res.StatusCode != http.StatusConflict || !strings.Contains(body, "changed meanwhile") {
		t.Fatalf("stale save = %d %s", res.StatusCode, body)
	}

	if _, body := admin.do(http.MethodGet, "/admin/presets", "", "", nil); !strings.Contains(string(body), "2 m FM calling") ||
		!strings.Contains(string(body), "145 MHz") {
		t.Errorf("list = %s", body)
	}

	// The schedules page lists a schedule of the preset; the preset in use
	// cannot be deleted.
	id := regexp.MustCompile(`/admin/presets/([0-9a-f-]{36})`).FindStringSubmatch(edit)[1]
	sid := addSchedule(t, h, id)

	if _, body := admin.do(http.MethodGet, "/admin/schedules", "", "", nil); !strings.Contains(string(body), "2 m FM calling") ||
		!strings.Contains(string(body), "0000-0100 UTC") || !strings.Contains(string(body), ">hf<") {
		t.Errorf("schedules page = %s", body)
	}

	if res, _ := admin.do(http.MethodGet, edit+"/delete", "", "", nil); res.StatusCode != http.StatusOK {
		t.Fatalf("delete page = %d", res.StatusCode)
	}

	if res, body := admin.form(edit+"/delete", url.Values{}, false); res.StatusCode != http.StatusConflict || !strings.Contains(body, "schedules") {
		t.Fatalf("delete of a preset in use = %d %s", res.StatusCode, body)
	}

	if err := schedules.NewSchedules(h.db).Delete(context.Background(), sid); err != nil {
		t.Fatal(err)
	}

	if res, _ := admin.form(edit+"/delete", url.Values{}, false); res.StatusCode != http.StatusSeeOther ||
		res.Header.Get("Location") != "/admin/presets?done=deleted" {
		t.Fatalf("delete = %d %s", res.StatusCode, res.Header.Get("Location"))
	}

	if res, _ := admin.do(http.MethodGet, edit, "", "", nil); res.StatusCode != http.StatusNotFound {
		t.Errorf("deleted preset page = %d", res.StatusCode)
	}
}

// addSchedule stores a schedule of the preset on device hf, 00:00–01:00 UTC.
func addSchedule(t *testing.T, h *adminHub, preset string) shared.UUID {
	t.Helper()

	now := time.Now()
	id, _ := shared.NewUUIDv7Generator().New(now)

	spec, err := schedules.NewSpec(schedules.Draft{DeviceID: "hf", PresetID: preset, StartMinute: new(0), EndMinute: new(60)})
	if err != nil {
		t.Fatal(err)
	}

	sc, _ := schedules.NewSchedule(id, spec, now)
	if err := schedules.NewSchedules(h.db).Create(context.Background(), sc); err != nil {
		t.Fatal(err)
	}

	return id
}
