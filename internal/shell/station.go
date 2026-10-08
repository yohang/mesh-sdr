package shell

import "context"

// StationSettings reads the station description the Receiver page shows
// (receiver.location, receiver.photo_title, receiver.photo_desc), the
// audio compression its receiver asks for (audio_compression) and whether
// the browser recorder is offered to every listener (ui.recorder_enabled).
type StationSettings interface {
	Location(ctx context.Context) string
	PhotoTitle(ctx context.Context) string
	PhotoDesc(ctx context.Context) string
	AudioCompression(ctx context.Context) string
	RecorderEnabled(ctx context.Context) bool
}

// Audio codecs of the rx.v1 media streams (DEM-010).
const (
	CodecADPCM = "adpcm-ima"
	CodecPCM   = "pcm-s16le"
)

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
	// AudioCodec is the codec the receiver asks the node for: CodecPCM
	// when audio_compression is pcm, else CodecADPCM.
	AudioCodec string
	// RecorderEnabled offers the browser recorder (REC-001) to every
	// listener; admins have it either way.
	RecorderEnabled bool
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
		AudioCodec: CodecADPCM, RecorderEnabled: s.settings.RecorderEnabled(ctx),
	}

	if s.settings.AudioCompression(ctx) == "pcm" {
		v.AudioCodec = CodecPCM
	}

	if s.images != nil {
		v.HasAvatar = s.images.HasImage(ctx, ImageAvatar)
		v.HasPanorama = s.images.HasImage(ctx, ImagePanorama)
	}

	return v
}
