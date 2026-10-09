package shell

import "context"

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

// StationView is the station as the Receiver page presents it (RX-036).
type StationView struct {
	Location   string
	PhotoTitle string
	// PhotoDesc is Markdown (raw HTML is not rendered).
	PhotoDesc   string
	HasAvatar   bool
	HasPanorama bool
	// AudioCodec is the codec the receiver asks the node for.
	AudioCodec string
	// RecorderEnabled offers the browser recorder (REC-001) to every
	// listener; admins have it either way.
	RecorderEnabled bool
}

// station returns the station description.
func (m *Module) station(ctx context.Context) StationView {
	s := m.settings
	v := StationView{
		Location: s.Location(), PhotoTitle: s.PhotoTitle(), PhotoDesc: s.PhotoDesc(), AudioCodec: s.AudioCodec(),
		RecorderEnabled: s.RecorderEnabled(),
	}

	if m.images != nil {
		v.HasAvatar = m.images.HasImage(ctx, ImageAvatar)
		v.HasPanorama = m.images.HasImage(ctx, ImagePanorama)
	}

	return v
}
