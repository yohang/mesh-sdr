package wire_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestPresetCloneAndMovePages: ADM-019 and ADM-021 over HTTP. Both are
// admin-only POST forms with CSRF; reordering answers the list fragment.
func TestPresetCloneAndMovePages(t *testing.T) {
	h := newAdminHub(t, nil)
	admin := h.browser("root")

	var ids []string

	for _, n := range []string{"Alpha", "Bravo", "Charlie"} {
		res, _ := admin.form("/admin/presets", url.Values{"name": {n}, "center_freq": {"145000000"}, "samp_rate": {"2048000"}}, false)
		ids = append(ids, strings.TrimSuffix(strings.TrimPrefix(res.Header.Get("Location"), "/admin/presets/"), "?done=created"))
	}

	for _, user := range []string{"", "lis", "op"} {
		b := h.browser(user)

		for _, path := range []string{"/clone", "/move"} {
			res, _ := b.form("/admin/presets/"+ids[0]+path, url.Values{"direction": {"down"}}, false)
			if res.StatusCode != http.StatusForbidden && !strings.HasPrefix(res.Header.Get("Location"), "/login") {
				t.Errorf("%q POST %s = %d", user, path, res.StatusCode)
			}
		}
	}

	if n := h.count("SELECT count(*) FROM presets"); n != 3 {
		t.Fatalf("%d presets after refused forms", n)
	}

	// Clone: a new preset, opened for editing.
	res, _ := admin.form("/admin/presets/"+ids[0]+"/clone", url.Values{}, false)
	if res.StatusCode != http.StatusSeeOther || !strings.HasSuffix(res.Header.Get("Location"), "?done=cloned") {
		t.Fatalf("clone = %d %s", res.StatusCode, res.Header.Get("Location"))
	}

	_, edit := admin.do(http.MethodGet, res.Header.Get("Location"), "", "", nil)
	if !strings.Contains(string(edit), `value="Alpha (copy)"`) || !strings.Contains(string(edit), "Preset cloned") {
		t.Errorf("cloned preset page = %s", edit)
	}

	if n := h.count("SELECT count(*) FROM audit_log WHERE action = 'preset.clone'"); n != 1 {
		t.Errorf("%d clone audit rows", n)
	}

	// Move: down, up and to a position, answered as the list fragment.
	pos := func(name string) int {
		return h.count("SELECT sort_order FROM presets WHERE name = ?", name)
	}

	res, frag := admin.form("/admin/presets/"+ids[0]+"/move", url.Values{"direction": {"down"}}, true)
	if res.StatusCode != http.StatusOK || !strings.Contains(frag, `id="presets-list"`) || strings.Contains(frag, "<html") ||
		!strings.Contains(frag, "Preset moved.") || pos("Alpha") != 1 || pos("Bravo") != 0 {
		t.Fatalf("move down = %d %s", res.StatusCode, frag)
	}

	if res, _ := admin.form("/admin/presets/"+ids[2]+"/move", url.Values{"position": {"1"}}, true); res.StatusCode != http.StatusOK || pos("Charlie") != 0 {
		t.Errorf("move to the top = %d, position %d", res.StatusCode, pos("Charlie"))
	}

	for _, bad := range []url.Values{{}, {"position": {"0"}}, {"position": {"x"}}, {"direction": {"sideways"}}} {
		if res, _ := admin.form("/admin/presets/"+ids[0]+"/move", bad, true); res.StatusCode != http.StatusBadRequest {
			t.Errorf("move %v = %d", bad, res.StatusCode)
		}
	}

	if res, _ := admin.form("/admin/presets/0192f2b4-0000-7000-8000-0000000000aa/move", url.Values{"direction": {"up"}}, true); res.StatusCode != http.StatusNotFound {
		t.Errorf("move unknown = %d", res.StatusCode)
	}

	// A reorder is not an edit: versions are untouched.
	if n := h.count("SELECT count(*) FROM presets WHERE version <> 1"); n != 0 {
		t.Errorf("%d presets changed version", n)
	}

	_, list := admin.do(http.MethodGet, "/admin/presets", "", "", nil)
	if !strings.Contains(string(list), "Move up") || !strings.Contains(string(list), "data-msdr-sortable") {
		t.Errorf("list lacks the reorder controls: %s", list)
	}
}
