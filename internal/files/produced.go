package files

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"regexp"
	"strconv"
	"time"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// The files the nodes send (FIL-005) and their reception metadata
// (FIL-008).

// Errors of the files the nodes send.
var (
	ErrFileRefused = shared.NewError(shared.KindInvalid, "file_refused", "file refused")
	ErrFileTooBig  = shared.NewError(shared.KindInvalid, "file_too_large", "the file exceeds its size cap")
)

// Limits of the files the nodes send.
const (
	// MaxFileSize is the size cap of a file; MaxFAXSize that of a FAX
	// image (FIL-005).
	MaxFileSize = 8 << 20
	MaxFAXSize  = 16 << 20
	// MaxWireChunk bounds the content of one file.chunk (§4.4).
	MaxWireChunk = 256 << 10
	// MaxMetadata bounds the per-kind metadata, as JSON.
	MaxMetadata = 4 << 10
	// MaxProducedPixels bounds a decoded image (FAX pages are long).
	MaxProducedPixels = 16_000_000
	// ThumbnailSide is the longest side of a thumbnail, in pixels.
	ThumbnailSide = 320
	// ClockSkewLimit is the clock difference above which a file records
	// the node's clock skew (FIL-008).
	ClockSkewLimit = 5 * time.Second
	// IncompleteAfter is how long a file may stay incomplete before it is
	// deleted with its chunks (§7.3).
	IncompleteAfter = 10 * time.Minute
)

// ProducedKinds are the kinds of the files decoders and recordings produce
// (the FIL-008 rule): the gallery lists them and the retention covers
// them; the receiver images are not among them.
var ProducedKinds = []Kind{"sstv", "fax", "recording", "speech", "text_log", "satellite"}

// nodeKinds are the kinds a node may send, with their media type.
var nodeKinds = map[Kind]MIMEType{KindSSTV: MIMEPNG, KindFAX: MIMEPNG, KindTextLog: MIMEText}

// namePrefix is the prefix of the generated file names.
var namePrefix = map[Kind]string{KindSSTV: "SSTV", KindFAX: "FAX", KindTextLog: "LOG"}

var modePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,23}$`)

// Incoming is a file a node announces with file.begin. Every field comes
// from the node and is checked by Check.
type Incoming struct {
	ID               shared.UUID
	Node             string
	Kind             Kind
	MIME             MIMEType
	Size             int64
	SHA256           []byte
	DeviceID         shared.DeviceID
	PresetID         shared.UUID // zero when unknown
	DecoderSessionID shared.UUID // zero when unknown
	Mode             string
	FrequencyHz      int64
	ReceivedStart    time.Time
	ReceivedEnd      time.Time // zero when unknown
	// Metadata is the JSON object of the per-kind details.
	Metadata json.RawMessage
}

// MaxSize returns the size cap of a kind.
func MaxSize(k Kind) int64 {
	if k == KindFAX {
		return MaxFAXSize
	}

	return MaxFileSize
}

// Check validates an announced file: a kind a node may send with its media
// type, a size within the cap, a digest, a mode, a reception frequency and
// start, and metadata that is a small JSON object.
func (in Incoming) Check() error {
	mime, ok := nodeKinds[in.Kind]

	switch {
	case in.ID.IsZero(), in.Node == "", in.DeviceID.IsZero():
		return ErrFileRefused.WithDetail("the file, node and device ids are required")
	case !ok:
		return ErrUnsupportedKind.WithDetail("a node cannot send files of kind " + strconv.Quote(string(in.Kind)))
	case in.MIME != mime:
		return ErrInvalidMIMEType.WithDetail("a " + string(in.Kind) + " file must be " + string(mime))
	case in.Size < 1:
		return ErrFileRefused.WithDetail("the file is empty")
	case in.Size > MaxSize(in.Kind):
		return ErrFileTooBig.WithDetail("the file has " + strconv.FormatInt(in.Size, 10) + " bytes, at most " +
			strconv.FormatInt(MaxSize(in.Kind), 10) + " for " + string(in.Kind))
	case len(in.SHA256) != sha256.Size:
		return ErrFileRefused.WithDetail("invalid SHA-256 digest")
	case !modePattern.MatchString(in.Mode):
		return ErrFileRefused.WithDetail("invalid mode")
	case in.FrequencyHz < 1:
		return ErrFileRefused.WithDetail("the reception frequency is required")
	case in.ReceivedStart.IsZero():
		return ErrFileRefused.WithDetail("the reception start is required")
	case !in.ReceivedEnd.IsZero() && in.ReceivedEnd.Before(in.ReceivedStart):
		return ErrFileRefused.WithDetail("the reception ends before it starts")
	case len(in.Metadata) > MaxMetadata:
		return ErrFileRefused.WithDetail("the metadata exceeds 4 KiB")
	}

	if len(in.Metadata) > 0 {
		var obj map[string]any
		if err := json.Unmarshal(in.Metadata, &obj); err != nil || obj == nil {
			return ErrFileRefused.WithDetail("the metadata must be a JSON object")
		}
	}

	return nil
}

// Name returns the generated display and download name of the file:
// <PFX>-<yymmdd>-<HHMMSS>-<kHz>.<ext>, from the reception start in UTC.
// Nothing the node sent but the values checked by Check goes into it.
func (in Incoming) Name() string {
	return namePrefix[in.Kind] + "-" + in.ReceivedStart.UTC().Format("060102-150405") + "-" +
		strconv.FormatInt(in.FrequencyHz/1000, 10) + "." + in.MIME.Extension()
}

// metadataWithSkew returns the metadata to store: the node's object, plus
// clock_skew_ms when the node clock is off by more than ClockSkewLimit.
func (in Incoming) metadataWithSkew(skewMS int64) (string, error) {
	obj := map[string]any{}

	if len(in.Metadata) > 0 {
		dec := json.NewDecoder(bytes.NewReader(in.Metadata))
		dec.UseNumber()

		if err := dec.Decode(&obj); err != nil {
			return "", ErrFileRefused.WithDetail("the metadata must be a JSON object")
		}
	}

	delete(obj, "clock_skew_ms")

	if skewMS > ClockSkewLimit.Milliseconds() || skewMS < -ClockSkewLimit.Milliseconds() {
		obj["clock_skew_ms"] = skewMS
	}

	b, err := json.Marshal(obj)
	if err != nil {
		return "", ErrFileRefused.WithDetail("invalid metadata")
	}

	return string(b), nil
}

func sha256Sum(b []byte) []byte {
	s := sha256.Sum256(b)

	return s[:]
}

// Entry is a complete file the nodes sent, as the gallery, the detail view
// and the download read it.
type Entry struct {
	ID               shared.UUID
	Kind             Kind
	Name             string
	MIME             MIMEType
	Size             int64
	SHA256           []byte
	NodeID           string
	DeviceID         string
	PresetID         shared.UUID
	DecoderSessionID shared.UUID
	Mode             string
	FrequencyHz      int64
	ReceivedStart    time.Time
	ReceivedEnd      time.Time // zero when unknown
	Width, Height    int
	// Metadata are the per-kind details, as stored (a JSON object).
	Metadata     map[string]any
	HasThumbnail bool
	CreatedAt    time.Time
}

// IsImage reports whether the file is an image.
func (e Entry) IsImage() bool { return e.MIME == MIMEPNG || e.MIME == MIMEJPEG || e.MIME == MIMEWebP }

// IsAudio reports whether the file is audio.
func (e Entry) IsAudio() bool { return e.MIME == "audio/mpeg" || e.MIME == "audio/ogg" }

// IsText reports whether the file is text.
func (e Entry) IsText() bool { return e.MIME == MIMEText }

// Media is a media type family of the gallery filter.
type Media string

// Media families.
const (
	MediaImage Media = "image"
	MediaAudio Media = "audio"
	MediaText  Media = "text"
)

// Filter selects the files of the gallery (FIL-001). Zero fields do not
// filter.
type Filter struct {
	Media    Media
	DeviceID string
	Mode     string
	From, To time.Time // reception start, UTC; To is exclusive
	FreqMin  int64     // Hz
	FreqMax  int64     // Hz
}

// Access tells which files a viewer may see (listen_policy, ADR 0026).
type Access struct {
	// Denied hides every file (an anonymous visitor under the registered
	// global policy).
	Denied bool
	// HiddenDevices are the devices whose files the viewer may not see.
	HiddenDevices []string
}

// Allows reports whether the viewer may see a file of device.
func (v Access) Allows(device string) bool {
	if v.Denied {
		return false
	}

	for _, d := range v.HiddenDevices {
		if d == device {
			return false
		}
	}

	return true
}
