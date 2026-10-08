package wire_test

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/wire"
)

var bookmarkRow = regexp.MustCompile(`<tr id="bookmark-([0-9a-f-]{36})"`)

// TestBookmarkPagesAccess: Bookmarks › Manage is for operators and admins
// (BMK-005, §5.10); anonymous visitors and listeners can neither open it
// nor post its forms.
func TestBookmarkPagesAccess(t *testing.T) {
	h := newAdminHub(t, nil)
	create := func(name string) url.Values {
		return url.Values{"name": {name}, "frequency": {"145500000"}, "modulation": {"nfm"}, "scope": {"all"}}
	}

	browsers := map[string]*browser{}

	for _, tt := range []struct {
		user   string
		page   int
		create bool
	}{{"", http.StatusSeeOther, false}, {"lis", http.StatusForbidden, false}, {"op", http.StatusOK, true}, {"root", http.StatusOK, true}} {
		b := h.browser(tt.user)
		browsers[tt.user] = b

		res, body := b.do(http.MethodGet, "/bookmarks/manage", "", "", nil)
		if res.StatusCode != tt.page {
			t.Errorf("%q GET = %d, want %d", tt.user, res.StatusCode, tt.page)
		}

		// The user menu links to the page for operators and admins.
		_, home := b.do(http.MethodGet, "/files", "", "", nil)
		if got := strings.Contains(string(home), `href="/bookmarks/manage"`); got != tt.create {
			t.Errorf("%q menu link = %v", tt.user, got)
		}

		res, _ = b.form("/bookmarks/manage", create("2 m calling "+tt.user), false)

		switch {
		case tt.create && (res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(res.Header.Get("Location"), "/bookmarks/manage?done=created")):
			t.Errorf("%q create = %d %s", tt.user, res.StatusCode, res.Header.Get("Location"))
		case !tt.create && res.StatusCode != http.StatusForbidden && !strings.HasPrefix(res.Header.Get("Location"), "/login"):
			t.Errorf("%q create = %d %s", tt.user, res.StatusCode, res.Header.Get("Location"))
		}

		if tt.user == "op" && (!strings.Contains(string(body), `href="/admin/devices"`) || strings.Contains(string(body), `href="/admin/site"`)) {
			t.Error("operator page without the operator admin sections")
		}
	}

	if n := h.count("SELECT count(*) FROM bookmarks WHERE origin = 'db'"); n != 2 {
		t.Fatalf("%d hub bookmarks, want the operator's and the admin's", n)
	}

	_, page := browsers["op"].do(http.MethodGet, "/bookmarks/manage", "", "", nil)

	m := bookmarkRow.FindSubmatch(page)
	if m == nil {
		t.Fatalf("no bookmark row in %s", page)
	}

	row := "/bookmarks/manage/" + string(m[1])

	// Listeners and visitors cannot edit nor delete it.
	for _, user := range []string{"", "lis"} {
		b := browsers[user]

		for path, values := range map[string]url.Values{
			row:             {"version": {"1"}, "name": {"Taken"}, "frequency": {"145500000"}, "modulation": {"nfm"}, "scope": {"all"}},
			row + "/delete": {"version": {"1"}},
		} {
			if res, _ := b.form(path, values, false); res.StatusCode != http.StatusForbidden && !strings.HasPrefix(res.Header.Get("Location"), "/login") {
				t.Errorf("%q POST %s = %d", user, path, res.StatusCode)
			}
		}
	}

	if n := h.count("SELECT count(*) FROM bookmarks WHERE name = 'Taken'"); n != 0 {
		t.Error("a refused form changed a bookmark")
	}

	if n := h.count("SELECT count(*) FROM audit_log WHERE action = 'bookmark.create' AND target_type = 'bookmark'"); n != 2 {
		t.Errorf("%d create audit rows", n)
	}
}

// TestBookmarkPages: an operator adds, edits inline (htmx) and deletes a
// hub bookmark; the server validates every field (BMK-003); pack rows are
// read-only.
func TestBookmarkPages(t *testing.T) {
	h := newAdminHub(t, nil)
	op := h.browser("op")

	if _, err := wire.SyncBookmarks(context.Background(), h.db, discard); err != nil {
		t.Fatal(err)
	}

	// The add form is pre-filled from the query (the receiver's tuning).
	_, body := op.do(http.MethodGet, "/bookmarks/manage?f=7074000&m=usb&name=FT8+net&origin=db", "", "", nil)
	for _, want := range []string{`value="7074000"`, `value="usb"`, `value="FT8 net"`, "0 bookmarks"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("pre-filled page lacks %q", want)
		}
	}

	for _, tt := range []struct {
		values url.Values
		want   string
	}{
		{url.Values{"name": {""}, "frequency": {"7100000"}, "modulation": {"lsb"}, "scope": {"all"}}, "Name: Required."},
		{url.Values{"name": {"Net"}, "frequency": {"abc"}, "modulation": {"lsb"}, "scope": {"all"}}, "Frequency (Hz): Enter a whole number of Hz."},
		{url.Values{"name": {"Net"}, "frequency": {"0"}, "modulation": {"lsb"}, "scope": {"all"}}, "Frequency (Hz): Enter a frequency in Hz above 0."},
		{url.Values{"name": {"Net"}, "frequency": {"7100000"}, "modulation": {"dmr"}, "scope": {"all"}}, "Mode: Use a mode the receivers offer"},
		{url.Values{"name": {"Net"}, "frequency": {"7100000"}, "modulation": {"lsb"}, "underlying": {"usb"}, "scope": {"all"}}, "Underlying mode: An analog mode takes no underlying mode."},
		{url.Values{"name": {"Net"}, "frequency": {"7100000"}, "modulation": {"lsb"}, "scope": {"device"}, "device": {""}}, "Device: Choose a device."},
		{url.Values{"name": {"Net"}, "frequency": {"7100000"}, "modulation": {"lsb"}, "scope": {"device"}, "device": {"nope"}}, "Device: Choose a device of the registry."},
		{url.Values{"name": {"PMR1"}, "frequency": {"446006250"}, "modulation": {"nfm"}, "scope": {"all"}}, "Name: A bookmark with this name, frequency and mode exists."},
	} {
		res, body := op.form("/bookmarks/manage", tt.values, false)
		if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, tt.want) {
			t.Errorf("create %v = %d, lacks %q", tt.values, res.StatusCode, tt.want)
		}
	}

	res, _ := op.form("/bookmarks/manage", url.Values{
		"name": {"Net"}, "frequency": {"7100000"}, "modulation": {"lsb"}, "description": {"Sunday net"}, "scope": {"all"},
	}, false)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("create = %d", res.StatusCode)
	}

	id := strings.TrimPrefix(res.Header.Get("Location"), "/bookmarks/manage?done=created#bookmark-")
	row := "/bookmarks/manage/" + id
	htmx := map[string]string{"HX-Request": "true"}

	// Inline edit: the row becomes a form, a refused save keeps it, a
	// saved one returns the row.
	res, body = op.do(http.MethodGet, row, "", "", htmx)
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(strings.TrimSpace(string(body)), `<tr id="bookmark-`+id+`"`) ||
		!strings.Contains(string(body), `hx-post="`+row+`"`) || strings.Contains(string(body), "<html") {
		t.Fatalf("inline edit = %d %s", res.StatusCode, body)
	}

	edit := url.Values{"version": {"1"}, "name": {"Net 2"}, "frequency": {"7110000"}, "modulation": {"usb"}, "scope": {"all"}}
	bad := url.Values{"version": {"1"}, "name": {""}, "frequency": {"7110000"}, "modulation": {"usb"}, "scope": {"all"}}

	if res, body := op.form(row, bad, true); res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Name: Required.") {
		t.Errorf("refused inline save = %d %s", res.StatusCode, body)
	}

	if res, body := op.form(row, edit, true); res.StatusCode != http.StatusOK || !strings.Contains(body, "Net 2") || !strings.Contains(body, "7.11 MHz") {
		t.Fatalf("inline save = %d %s", res.StatusCode, body)
	}

	if res, _ := op.form(row, edit, false); res.StatusCode != http.StatusConflict {
		t.Errorf("stale save = %d", res.StatusCode)
	}

	if res, body := op.do(http.MethodGet, row+"/row", "", "", htmx); res.StatusCode != http.StatusOK || !strings.Contains(string(body), "Net 2") {
		t.Errorf("cancel = %d", res.StatusCode)
	}

	// Delete, after confirmation.
	if res, body := op.do(http.MethodGet, row+"/delete", "", "", nil); res.StatusCode != http.StatusOK || !strings.Contains(string(body), "Delete the bookmark") {
		t.Errorf("delete page = %d", res.StatusCode)
	}

	if res, _ := op.form(row+"/delete", url.Values{"version": {"2"}}, false); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete = %d", res.StatusCode)
	}

	for action, want := range map[string]int{"bookmark.create": 1, "bookmark.update": 1, "bookmark.delete": 1} {
		if n := h.count("SELECT count(*) FROM audit_log WHERE action = ? AND target_id = ?", action, id); n != want {
			t.Errorf("%d %s audit rows", n, action)
		}
	}

	// Pack rows are listed, read-only.
	_, page := op.do(http.MethodGet, "/bookmarks/manage?origin=builtin&from=446000000&to=446200000", "", "", nil)
	if !strings.Contains(string(page), "PMR1") || !strings.Contains(string(page), "Pack (pmr)") || !strings.Contains(string(page), "Read-only") ||
		strings.Contains(string(page), "Edit<span") {
		t.Fatalf("pack rows = %s", page)
	}

	pack := "/bookmarks/manage/" + string(bookmarkRow.FindSubmatch(page)[1])

	if res, _ := op.form(pack, edit, false); res.StatusCode != http.StatusConflict {
		t.Errorf("save of a pack row = %d", res.StatusCode)
	}

	if res, _ := op.form(pack+"/delete", url.Values{"version": {"1"}}, false); res.StatusCode != http.StatusConflict {
		t.Errorf("delete of a pack row = %d", res.StatusCode)
	}
}
