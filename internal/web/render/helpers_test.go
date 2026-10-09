package render_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
	"github.com/yohang/mesh-sdr/internal/web/layout"
	"github.com/yohang/mesh-sdr/internal/web/render"
)

func TestRedirect(t *testing.T) {
	for _, tt := range []struct {
		name     string
		htmx     bool
		status   int
		location string
	}{
		{"plain form", false, http.StatusSeeOther, "Location"},
		{"htmx", true, http.StatusNoContent, "HX-Redirect"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/x", nil)
			if tt.htmx {
				r.Header.Set("HX-Request", "true")
			}

			rec := httptest.NewRecorder()
			render.Redirect(rec, r, "/done?x=1")

			if rec.Code != tt.status || rec.Header().Get(tt.location) != "/done?x=1" {
				t.Errorf("got %d %v", rec.Code, rec.Header())
			}
		})
	}
}

func TestNoIndex(t *testing.T) {
	rec := httptest.NewRecorder()
	render.NoIndex(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Header().Get("X-Robots-Tag") != "noindex" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("headers = %v", rec.Header())
	}
}

func TestParseForm(t *testing.T) {
	rd := newRenderer(layout.ThemeAuto)

	for _, tt := range []struct {
		name   string
		body   string
		limit  int64
		ok     bool
		status int
	}{
		{"small", "a=1", 16, true, http.StatusOK},
		{"too large", "a=" + strings.Repeat("x", 64), 16, false, http.StatusRequestEntityTooLarge},
		{"malformed", "a=%zz", 0, false, http.StatusBadRequest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tt.body))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

			rec := httptest.NewRecorder()
			if ok := rd.ParseForm(rec, r, tt.limit); ok != tt.ok || rec.Code != tt.status {
				t.Errorf("ok = %v, status %d", ok, rec.Code)
			}
		})
	}
}

func TestAdminPage(t *testing.T) {
	for _, tt := range []struct {
		name  string
		admin func(context.Context) bool
		users bool
	}{
		{"admin", func(context.Context) bool { return true }, true},
		{"operator", func(context.Context) bool { return false }, false},
		{"no gate", nil, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rd := render.New(shellSource{SiteName: "TestSDR"}, tt.admin, slog.New(slog.DiscardHandler))

			rec := httptest.NewRecorder()
			rd.AdminPage(rec, httptest.NewRequest(http.MethodGet, "/admin/nodes", nil), http.StatusOK, "Nodes", "nodes", text("<h1>Nodes</h1>"), nil)

			body := rec.Body.String()
			if users := strings.Contains(body, `href="/admin/users"`); users != tt.users ||
				!strings.Contains(body, `href="/admin/nodes" class="block rounded px-2 py-1 bg-surface-raised font-semibold" aria-current="page"`) {
				t.Errorf("users link = %v:\n%s", users, body)
			}
		})
	}
}

func TestSentence(t *testing.T) {
	for in, want := range map[string]string{"": "", "at most 5": "At most 5.", "Done.": "Done.", "économie": "Économie."} {
		if got := render.Sentence(in); got != want {
			t.Errorf("Sentence(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseKHz(t *testing.T) {
	for in, want := range map[string]int64{"145500": 145_500_000, "7074.5": 7_074_500, " 0 ": 0} {
		if got, ok := render.ParseKHz(in); !ok || got != want {
			t.Errorf("ParseKHz(%q) = %d, %v", in, got, ok)
		}
	}

	for _, in := range []string{"", "-1", "NaN", "1e10", "abc"} {
		if _, ok := render.ParseKHz(in); ok {
			t.Errorf("ParseKHz(%q) accepted", in)
		}
	}
}

func TestPageOf(t *testing.T) {
	for in, want := range map[string]int{"": 1, "page=3": 3, "page=0": 1, "page=-2": 1, "page=x": 1, "page=99999": 1} {
		q, _ := url.ParseQuery(in)
		if got := render.PageOf(q); got != want {
			t.Errorf("PageOf(%q) = %d, want %d", in, got, want)
		}
	}
}

var errInvalid = shared.NewError(shared.KindInvalid, "invalid_thing", "invalid thing")

func TestForm(t *testing.T) {
	var f render.Form

	if f.AddViolations(errors.New("db down")) {
		t.Error("an infra error is not a domain error")
	}

	err := errInvalid.WithViolations(
		shared.NewViolation("name", "required", "required"),
		shared.NewViolation("tags.2", "too_long", "at most 32 characters"),
		shared.NewViolation("name", "too_long", "too long"),
		shared.NewViolation("other", "x", "something else"),
	)
	if !f.AddViolations(err) || f.Status() != http.StatusUnprocessableEntity {
		t.Fatalf("form = %+v", f)
	}

	got := f.Problems("p-", []render.Field{{Key: "name", Label: "Name"}, {Key: "tags", Label: "Tags"}})
	want := []layout.Problem{
		{ID: "p-name", Text: "Name: Required."}, {ID: "p-tags", Text: "Tags: At most 32 characters."}, {Text: "Something else."},
	}

	if !slices.Equal(got, want) {
		t.Errorf("problems = %+v", got)
	}

	var whole render.Form
	if whole.AddViolations(errInvalid); whole.Failure != "Invalid thing." || len(whole.Errors) != 0 {
		t.Errorf("whole form = %+v", whole)
	}

	if whole.Conflict = true; whole.Status() != http.StatusConflict {
		t.Error("conflict status")
	}

	if whole.Broken = true; whole.Status() != http.StatusInternalServerError {
		t.Error("broken status")
	}
}
