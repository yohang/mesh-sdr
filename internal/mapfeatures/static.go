package mapfeatures

import (
	"context"
	"log/slog"
	"slices"
)

// Static is a static marker of the config (MAP-017): id is unique in its
// file.
type Static struct {
	File, ID, Name string
	Lat, Lon       float64
	Comment, URL   string
}

// SyncStatic replaces the static markers of map_features by markers (read
// from map.static_markers_dir at hub start): permanent, without a device,
// shown to every visitor (Public). The removed and written markers are
// published.
func (m *Module) SyncStatic(ctx context.Context, markers []Static) error {
	now := m.d.Now()

	var changes []Change

	err := m.d.DB.WithinTx(ctx, func(ctx context.Context) error {
		changes = nil

		old, err := m.repo.DeleteStatic(ctx)
		if err != nil {
			return err
		}

		keys := make([]string, 0, len(markers))

		for _, s := range markers {
			lat, lon := s.Lat, s.Lon
			details := map[string]any{"label": s.Name}

			setText(details, "comment", s.Comment)
			setText(details, "url", s.URL)

			f := Feature{
				Key: "static:" + s.File + ":" + s.ID, Kind: KindStatic, Subject: s.File + ":" + s.ID, Source: SourceConfig,
				Lat: &lat, Lon: &lon, Geometry: Geometry{Type: GeometryPoint, Lat: &lat, Lon: &lon}, Details: details, UpdatedAt: now,
			}

			if err := m.repo.Upsert(ctx, f); err != nil {
				return err
			}

			keys = append(keys, f.Key)
			changes = append(changes, Change{Upsert: &f})
		}

		for _, k := range old {
			if !slices.Contains(keys, k) {
				changes = append(changes, Change{Remove: &Removal{Key: k, Reason: ReasonDeleted, Public: true}})
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	m.d.Logger.InfoContext(ctx, "static map markers loaded", slog.Int("count", len(markers)))

	if m.d.Published != nil {
		for _, c := range changes {
			m.d.Published(ctx, c)
		}
	}

	return nil
}
