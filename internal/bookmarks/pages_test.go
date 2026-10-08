package bookmarks

import (
	"context"
	"strings"
	"testing"
)

func TestRowNotice(t *testing.T) {
	e := newEnv(t)
	a := e.create(t, Draft{Name: "A", Frequency: 7_100_000, Modulation: "lsb", Scope: onDevice(t, "hf")})
	b := e.create(t, Draft{Name: "B", Frequency: 7_200_000, Modulation: "lsb", Scope: onDevice(t, "hf")})

	if got, want := noticePath("saved", a), ManagePath+"?done=saved&id="+a.ID().String()+"#bookmark-"+a.ID().String(); got != want {
		t.Errorf("noticePath = %q, want %q", got, want)
	}

	tests := []struct {
		name     string
		notice   string
		id       string
		rowShown bool
	}{
		{"row in the table", "Bookmark saved.", a.ID().String(), true},
		{"row not in the table", "Bookmark saved.", "01a11bb0-5c16-77ed-97ea-d72b8c93a6a2", false},
		{"no id (deleted)", "Bookmark deleted.", "", false},
		{"id without notice", "", a.ID().String(), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := listView{Rows: []*Bookmark{a, b}, Notice: tt.notice, NoticeID: tt.id}
			if got := v.rowNotice(); got != tt.rowShown {
				t.Errorf("rowNotice = %v, want %v", got, tt.rowShown)
			}

			if got := v.noticeFor(b); got != "" {
				t.Errorf("noticeFor(other row) = %q", got)
			}

			var html strings.Builder
			if err := listPage(v).Render(context.Background(), &html); err != nil {
				t.Fatal(err)
			}

			statuses := strings.Count(html.String(), `role="status">`+tt.notice+`<`)
			if tt.notice != "" && statuses != 1 {
				t.Errorf("notice %q shown %d times, want once", tt.notice, statuses)
			}

			row := `<tr id="bookmark-` + a.ID().String() + `" class="scroll-mt-20`
			if !strings.Contains(html.String(), row) {
				t.Errorf("row %s has no scroll margin", a.ID())
			}

			inRow := strings.Contains(html.String(), `font-semibold text-success" role="status">`+tt.notice+`</span>`)
			if tt.notice != "" && inRow != tt.rowShown {
				t.Errorf("notice in the row = %v, want %v", inRow, tt.rowShown)
			}
		})
	}
}
