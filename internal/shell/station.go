package shell

import "context"

// StationSettings reads the station description the Receiver page shows
// (receiver.location, receiver.photo_title, receiver.photo_desc) and the
// audio compression its receiver asks for (audio_compression).
type StationSettings interface {
	Location(ctx context.Context) string
	PhotoTitle(ctx context.Context) string
	PhotoDesc(ctx context.Context) string
	AudioCompression(ctx context.Context) string
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
		AudioCodec: CodecADPCM,
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
