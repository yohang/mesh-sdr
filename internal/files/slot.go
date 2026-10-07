package files

import "strconv"

// Receiver image limits (ADM-004, TECHNICAL_SPEC §7.2-§7.3).
const (
	// MaxAvatarUpload bounds an uploaded avatar.
	MaxAvatarUpload = 250 << 10
	// MaxPanoramaUpload bounds an uploaded panorama.
	MaxPanoramaUpload = 2 << 20
	// MaxStoredImage bounds a re-encoded receiver image
	// (files.receiver_image_max_size).
	MaxStoredImage = 2 << 20
	// MaxImageSide bounds each side of an image, in pixels.
	MaxImageSide = 8192
	// MaxAvatarPixels and MaxPanoramaPixels bound the decoded size of an
	// image (decoding cost and memory).
	MaxAvatarPixels   = 1024 * 1024
	MaxPanoramaPixels = 40_000_000
)

// Slot is a receiver image: the avatar or the panorama.
type Slot struct {
	name      string
	kind      Kind
	maxUpload int64
	maxPixels int
	output    MIMEType
}

// Slots.
var (
	SlotAvatar   = Slot{name: "avatar", kind: KindReceiverAvatar, maxUpload: MaxAvatarUpload, maxPixels: MaxAvatarPixels, output: MIMEPNG}
	SlotPanorama = Slot{name: "panorama", kind: KindReceiverPhoto, maxUpload: MaxPanoramaUpload, maxPixels: MaxPanoramaPixels, output: MIMEJPEG}
)

// Slots lists the receiver images.
func Slots() []Slot { return []Slot{SlotAvatar, SlotPanorama} }

// ParseSlot returns the slot named s.
func ParseSlot(s string) (Slot, error) {
	for _, slot := range Slots() {
		if slot.name == s {
			return slot, nil
		}
	}

	return Slot{}, ErrUnknownImageSlot.WithDetail("unknown receiver image " + strconv.Quote(s))
}

// Name returns "avatar" or "panorama".
func (s Slot) Name() string { return s.name }

// Kind returns the file kind of the slot.
func (s Slot) Kind() Kind { return s.kind }

// MaxUpload returns the largest accepted upload, in bytes.
func (s Slot) MaxUpload() int64 { return s.maxUpload }

// Output returns the type the image is re-encoded to: PNG for the avatar,
// JPEG for the panorama.
func (s Slot) Output() MIMEType { return s.output }

// MaxPixels returns the largest accepted image, in pixels (width × height).
func (s Slot) MaxPixels() int { return s.maxPixels }
