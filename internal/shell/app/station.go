package app

import "context"

// StationSettings reads the station description the Receiver page shows
// (receiver.location, receiver.photo_title, receiver.photo_desc).
type StationSettings interface {
	Location(ctx context.Context) string
	PhotoTitle(ctx context.Context) string
	PhotoDesc(ctx context.Context) string
}

// Image slots of the station (ADM-004).
const (
	ImageAvatar   = "avatar"
	ImagePanorama = "panorama"
)

// StationImages tells whether a station image is set.
type StationImages interface {
	HasImage(ctx context.Context, slot string) bool
}

// StationView is the station as the Receiver page presents it.
type StationView struct {
	Location   string
	PhotoTitle string
	// PhotoDesc is Markdown (raw HTML is not rendered).
	PhotoDesc   string
	HasAvatar   bool
	HasPanorama bool
}

// Station describes the station (Receiver page).
type Station struct {
	settings StationSettings
	images   StationImages
}

// NewStation returns the use case. images may be nil (no images).
func NewStation(settings StationSettings, images StationImages) *Station {
	return &Station{settings: settings, images: images}
}

// View returns the station description.
func (s *Station) View(ctx context.Context) StationView {
	v := StationView{
		Location: s.settings.Location(ctx), PhotoTitle: s.settings.PhotoTitle(ctx), PhotoDesc: s.settings.PhotoDesc(ctx),
	}

	if s.images != nil {
		v.HasAvatar = s.images.HasImage(ctx, ImageAvatar)
		v.HasPanorama = s.images.HasImage(ctx, ImagePanorama)
	}

	return v
}
