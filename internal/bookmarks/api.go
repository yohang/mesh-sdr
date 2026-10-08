package bookmarks

import (
	"context"

	"github.com/yohang/mesh-sdr/internal/http/api"
)

var _ api.BookmarkHandlers = (*Module)(nil)

// GetBookmarks implements api.BookmarkHandlers (GET /bookmarks).
func (m *Module) GetBookmarks(ctx context.Context, req api.GetBookmarksRequestObject) (api.GetBookmarksResponseObject, error) {
	rg, err := NewRange(req.Params.From, req.Params.To)
	if err != nil {
		return nil, err
	}

	list, err := m.ForDevice(ctx, req.Params.DeviceId, rg)
	if err != nil {
		return nil, err
	}

	out := api.GetBookmarks200JSONResponse{Region: api.BookmarksRegion(m.Region()), Bookmarks: make([]api.Bookmark, 0, len(list))}
	for _, b := range list {
		out.Bookmarks = append(out.Bookmarks, apiBookmark(b))
	}

	return out, nil
}

// GetBandplan implements api.BookmarkHandlers (GET /bandplan).
func (m *Module) GetBandplan(_ context.Context, req api.GetBandplanRequestObject) (api.GetBandplanResponseObject, error) {
	rg, err := NewRange(req.Params.From, req.Params.To)
	if err != nil {
		return nil, err
	}

	from, to := rg.Bounds()
	plan := m.Bandplan()
	out := api.GetBandplan200JSONResponse{Region: api.BandplanRegion(plan.Region()), Bands: []api.Band{}, Dials: []api.Dial{}}

	for _, b := range plan.Bands(from, to) {
		out.Bands = append(out.Bands, api.Band{Name: b.Name, Low: b.Low, High: b.High, Tags: b.Tags})
	}

	for _, d := range plan.Dials(from, to) {
		out.Dials = append(out.Dials, api.Dial{Mode: d.Mode, Frequency: d.Frequency, Underlying: optional(d.Underlying), Band: d.Band})
	}

	return out, nil
}

// apiBookmark is the API view of a bookmark.
func apiBookmark(b *Bookmark) api.Bookmark {
	v := View(b)

	return api.Bookmark{
		Id: v.ID, Name: v.Name, Frequency: v.Frequency, Modulation: v.Modulation, Underlying: optional(v.Underlying),
		Description: optional(v.Description), Scannable: v.Scannable, Origin: api.BookmarkOrigin(v.Origin), Tags: v.Tags,
		Scope: api.BookmarkScope{Kind: api.BookmarkScopeKind(v.Scope.Kind), DeviceId: optional(v.Scope.DeviceID), PresetId: optional(v.Scope.PresetID)},
	}
}

func optional(s string) *string {
	if s == "" {
		return nil
	}

	return &s
}

// BookmarkView is the plain view of a bookmark, for the hub events
// (`bookmark.changed`) and the API: plain text, never HTML.
type BookmarkView struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Frequency   int64     `json:"frequency"`
	Modulation  string    `json:"modulation"`
	Underlying  string    `json:"underlying,omitempty"`
	Description string    `json:"description,omitempty"`
	Scannable   bool      `json:"scannable"`
	Origin      string    `json:"origin"`
	Tags        []string  `json:"tags"`
	Scope       ScopeView `json:"scope"`
}

// ScopeView is the plain view of a scope.
type ScopeView struct {
	Kind     string `json:"kind"`
	DeviceID string `json:"device_id,omitempty"`
	PresetID string `json:"preset_id,omitempty"`
}

// View returns the plain view of a bookmark.
func View(b *Bookmark) BookmarkView {
	sc := ScopeView{Kind: string(b.Scope().Kind()), DeviceID: b.Scope().Device().String()}
	if p := b.Scope().Preset(); !p.IsZero() {
		sc.PresetID = p.String()
	}

	return BookmarkView{
		ID: b.ID().String(), Name: b.Name(), Frequency: b.Frequency(), Modulation: b.Modulation(), Underlying: b.Underlying(),
		Description: b.Description(), Scannable: b.Scannable(), Origin: string(b.Origin()), Tags: b.Tags(), Scope: sc,
	}
}
