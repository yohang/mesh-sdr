// Package media holds the session payloads of the node media WebSocket
// (rx.v1, TECHNICAL_SPEC §6.2 "Session handshake", §6.4, §5.8). Like rxv1 it
// is pure: plain structs with their JSON shape, no I/O.
package media

// Client names the client software.
type Client struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ClientCapabilities are the codecs and rates a client accepts.
type ClientCapabilities struct {
	AudioCodecs []string `json:"audio_codecs"`
	FFTCodecs   []string `json:"fft_codecs"`
	AudioRates  []int    `json:"audio_rates"`
	MaxFFTFPS   int      `json:"max_fft_fps"`
}

// Hello is session.hello (client → node), the first message after the
// upgrade.
type Hello struct {
	Client       Client             `json:"client"`
	Capabilities ClientCapabilities `json:"capabilities"`
	Resume       string             `json:"resume,omitempty"`
}

// Server describes the node software.
type Server struct {
	ProductVersion string `json:"product_version"`
	Protocol       string `json:"protocol"`
}

// User is the subject of the connection's access token.
type User struct {
	ID    string   `json:"id,omitempty"`
	Name  string   `json:"name,omitempty"`
	Roles []string `json:"roles"`
}

// Limits are the per-connection limits (§6.9).
type Limits struct {
	MaxMsgBytes int `json:"max_msg_bytes"`
	MsgRate     int `json:"msg_rate"`
	MaxDemods   int `json:"max_demods"`
	MaxFFTFPS   int `json:"max_fft_fps"`
}

// Welcome is session.welcome (node → client).
type Welcome struct {
	CID         string `json:"cid"`
	Server      Server `json:"server"`
	User        User   `json:"user"`
	Limits      Limits `json:"limits"`
	TokenExp    int64  `json:"token_exp,omitempty"`
	ResumeToken string `json:"resume_token,omitempty"`
	ServerTime  int64  `json:"server_time"`
}

// AuthRefresh is auth.refresh (client → node): a new access token for the
// same connection (same cid and audience), sent before the current one
// expires.
type AuthRefresh struct {
	Token string `json:"token"`
}

// DeviceRef extracts the device of device-scoped client messages
// (device.attach, preset.select, device.retune).
type DeviceRef struct {
	DeviceID string `json:"device_id"`
}

// Codec names negotiated on the media WS (§6.4, §6.5).
const (
	CodecFFTU8   = "u8-db"
	CodecFFTF32  = "f32-db"
	CodecPCM     = "pcm-s16le"
	CodecADPCM   = "adpcm-ima"
	CodecOpus    = "opus"
	KindFFT      = "fft"
	KindAudio    = "audio"
	ModeNFM      = "nfm"
	ReasonClosed = "closed"
)

// FFTRequest is the fft part of device.attach.
type FFTRequest struct {
	Codec string `json:"codec"`
	FPS   int    `json:"fps"`
	Size  int    `json:"size,omitempty"`
}

// DeviceAttach is device.attach (client → node).
type DeviceAttach struct {
	DeviceID string     `json:"device_id"`
	FFT      FFTRequest `json:"fft"`
}

// StreamConfigure is stream.configure (client → node).
type StreamConfigure struct {
	StreamID uint16  `json:"stream_id"`
	FPS      *int    `json:"fps,omitempty"`
	Codec    *string `json:"codec,omitempty"`
	Paused   *bool   `json:"paused,omitempty"`
}

// Opus are the Opus options of audio.configure.
type Opus struct {
	Bitrate int `json:"bitrate"`
	FrameMS int `json:"frame_ms"`
}

// AudioConfigure is audio.configure (client → node) and its ack result.
type AudioConfigure struct {
	Codec      string `json:"codec"`
	SampleRate int    `json:"sample_rate"`
	Opus       *Opus  `json:"opus,omitempty"`
}

// Bandpass is a pass band relative to the demodulator offset.
type Bandpass struct {
	LowHz  float64 `json:"low_hz"`
	HighHz float64 `json:"high_hz"`
}

// DemodCreate is demod.create (client → node). NR, AGC, DMR filter and
// audio service are not provided by this node and are ignored.
type DemodCreate struct {
	DeviceID  string    `json:"device_id"`
	Mode      string    `json:"mode"`
	OffsetHz  int64     `json:"offset_hz"`
	Bandpass  *Bandpass `json:"bandpass,omitempty"`
	SquelchDB *float64  `json:"squelch_db,omitempty"`
}

// DemodSet is demod.set (client → node): any field of demod.create but the
// device.
type DemodSet struct {
	DemodID   string    `json:"demod_id"`
	Mode      *string   `json:"mode,omitempty"`
	OffsetHz  *int64    `json:"offset_hz,omitempty"`
	Bandpass  *Bandpass `json:"bandpass,omitempty"`
	SquelchDB *float64  `json:"squelch_db,omitempty"`
}

// DemodRef is demod.remove (client → node).
type DemodRef struct {
	DemodID string `json:"demod_id"`
}

// DeviceRetune is device.retune (client → node) and its ack result.
type DeviceRetune struct {
	DeviceID string `json:"device_id,omitempty"`
	CenterHz int64  `json:"center_hz"`
}

// TimeSync is time.sync (client → node).
type TimeSync struct {
	T0 int64 `json:"t0"`
}

// TimeSyncReply is time.sync.reply (node → client): t1 receipt and t2
// reply times in node milliseconds.
type TimeSyncReply struct {
	T0 int64 `json:"t0"`
	T1 int64 `json:"t1"`
	T2 int64 `json:"t2"`
}

// PresetRef names a preset.
type PresetRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Levels are waterfall levels in dB.
type Levels struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

// Waterfall are the waterfall defaults of device.config.
type Waterfall struct {
	Levels       Levels `json:"levels"`
	AutoMinRange int    `json:"auto_min_range"`
	Scheme       string `json:"scheme"`
}

// FFTConfig is the fft part of device.config.
type FFTConfig struct {
	Size int `json:"size"`
	FPS  int `json:"fps"`
}

// Squelch are the squelch defaults of device.config.
type Squelch struct {
	Initial    float64 `json:"initial"`
	AutoMargin int     `json:"auto_margin"`
}

// Start is the initial demodulator of device.config.
type Start struct {
	Mode     string `json:"mode"`
	OffsetHz int64  `json:"offset_hz"`
}

// FreqLimits are the device limits of device.config.
type FreqLimits struct {
	MinHz int64 `json:"min_hz"`
	MaxHz int64 `json:"max_hz"`
}

// Permissions are the device permissions of the connection.
type Permissions struct {
	Preset bool `json:"preset"`
	Retune bool `json:"retune"`
}

// DeviceConfig is device.config (node → client).
type DeviceConfig struct {
	DeviceID         string      `json:"device_id"`
	Revision         int         `json:"revision"`
	CenterHz         int64       `json:"center_hz"`
	SampleRate       int         `json:"sample_rate"`
	ActivePreset     *PresetRef  `json:"active_preset"`
	PresetsAvailable []PresetRef `json:"presets_available"`
	TuningStepHz     int         `json:"tuning_step_hz"`
	TuningPrecision  int         `json:"tuning_precision"`
	Start            Start       `json:"start"`
	Waterfall        Waterfall   `json:"waterfall"`
	FFT              FFTConfig   `json:"fft"`
	Squelch          Squelch     `json:"squelch"`
	NRInitial        int         `json:"nr_initial"`
	Limits           FreqLimits  `json:"limits"`
	Permissions      Permissions `json:"permissions"`
}

// DeviceConfigPatch is device.config.patch (node → client).
type DeviceConfigPatch struct {
	DeviceID string         `json:"device_id"`
	Revision int            `json:"revision"`
	Set      map[string]any `json:"set"`
	Unset    []string       `json:"unset"`
}

// DeviceState is device.state (node → client).
type DeviceState struct {
	DeviceID string `json:"device_id"`
	State    string `json:"state"`
	Reason   string `json:"reason,omitempty"`
	Hint     string `json:"hint,omitempty"`
}

// StreamFFT is the fft part of stream.open and stream.update.
type StreamFFT struct {
	Size    int     `json:"size"`
	StartHz int64   `json:"start_hz"`
	SpanHz  int     `json:"span_hz"`
	DBMin   float32 `json:"db_min"`
	DBStep  float32 `json:"db_step"`
}

// StreamOpen is stream.open (node → client).
type StreamOpen struct {
	StreamID   uint16     `json:"stream_id"`
	Kind       string     `json:"kind"`
	Codec      string     `json:"codec"`
	SampleRate int        `json:"sample_rate,omitempty"`
	Channels   int        `json:"channels,omitempty"`
	FFT        *StreamFFT `json:"fft,omitempty"`
	DemodID    string     `json:"demod_id,omitempty"`
	// FPS and Paused describe an FFT stream (additive fields).
	FPS    int  `json:"fps,omitempty"`
	Paused bool `json:"paused,omitempty"`
}

// StreamUpdate is stream.update (node → client): the changed fields.
type StreamUpdate struct {
	StreamID   uint16     `json:"stream_id"`
	FPS        int        `json:"fps,omitempty"`
	Codec      string     `json:"codec,omitempty"`
	SampleRate int        `json:"sample_rate,omitempty"`
	FFT        *StreamFFT `json:"fft,omitempty"`
}

// StreamClose is stream.close (node → client).
type StreamClose struct {
	StreamID uint16 `json:"stream_id"`
	Reason   string `json:"reason"`
}

// DemodMeter is demod.meter (node → client).
type DemodMeter struct {
	DemodID     string  `json:"demod_id"`
	LevelDB     float64 `json:"level_db"`
	SquelchOpen bool    `json:"squelch_open"`
}

// AttachResult is the ack result of device.attach.
type AttachResult struct {
	Device  DeviceConfig `json:"device"`
	Streams []StreamOpen `json:"streams"`
}

// DemodCreated is the ack result of demod.create.
type DemodCreated struct {
	DemodID       string  `json:"demod_id"`
	AudioStreamID uint16  `json:"audio_stream_id"`
	Applied       Applied `json:"applied"`
}

// Applied are the demodulator parameters in force.
type Applied struct {
	Mode      string   `json:"mode"`
	OffsetHz  int64    `json:"offset_hz"`
	Bandpass  Bandpass `json:"bandpass"`
	SquelchDB *float64 `json:"squelch_db"`
}

// AppliedResult is the ack result of demod.set.
type AppliedResult struct {
	Applied Applied `json:"applied"`
}

// StreamResult is the ack result of stream.configure.
type StreamResult struct {
	Stream StreamOpen `json:"stream"`
}
