// Package ctl holds the payloads of the rx-ctl.v1 control channel
// (TECHNICAL_SPEC §4.4, ADR 0008). Like rxv1 it is pure: plain structs with
// their JSON shape, no I/O.
//
// Every node → hub event carries a per-boot monotonically increasing Seq.
// Fields added by ADR 0008 beyond the spec tables are additive and optional.
package ctl

import (
	"encoding/json"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/token"
)

// Hello opens a session (hub → node).
type Hello struct {
	HubID      string   `json:"hub_id"`
	HubVersion string   `json:"hub_version"`
	Protocols  []string `json:"protocols"`
	ServerTime int64    `json:"server_time"`
	// HeartbeatIntervalMS is the node.heartbeat period wanted by the hub
	// (ADR 0008 Q19). Zero means the default (10 s).
	HeartbeatIntervalMS int64 `json:"heartbeat_interval_ms,omitempty"`
}

// Welcome answers Hello (node → hub). It carries no seq.
type Welcome struct {
	NodeID              string   `json:"node_id"`
	BootID              string   `json:"boot_id"`
	Version             string   `json:"version"`
	Protocols           []string `json:"protocols"`
	CapabilitiesHash    string   `json:"capabilities_hash"`
	LastAppliedRevision int64    `json:"last_applied_revision"`
}

// Ack acknowledges every node event up to and including UptoSeq (hub → node).
type Ack struct {
	UptoSeq int64 `json:"upto_seq"`
}

// KeysUpdate is ctl.keys.update (hub → node, §5.8): the public keys that
// verify access tokens (a JWKS), the issuer the tokens carry (hub.url, also
// the only browser Origin the node accepts) and the kids revoked before
// their time. It replaces the node's previous key set; a node that has not
// received one since boot refuses media connects (hub_unavailable).
type KeysUpdate struct {
	Issuer      string      `json:"issuer"`
	Keys        []token.JWK `json:"keys"`
	RevokedKids []string    `json:"revoked_kids"`
}

// Revoked is one revoked session or user, with the hub time of its
// revocation in Unix milliseconds. Tokens issued (iat, hub clock) at or
// before that time are refused; later ones (a new sign-in) are not.
type Revoked struct {
	ID string `json:"id"`
	At int64  `json:"at"`
}

// Revocations lists revoked sessions (token.SessionRef of the session id),
// users (user ids) and certificate serials (hub → node). The node closes the
// media connections whose token matches within 1 s (§5.8).
type Revocations struct {
	Sessions    []Revoked `json:"sessions"`
	Users       []Revoked `json:"users"`
	CertSerials []string  `json:"cert_serials"`
}

// CertRenew carries a renewed node certificate (hub → node): base64 standard
// encoded DER, leaf first.
type CertRenew struct {
	CertificateChain []string `json:"certificate_chain"`
}

// Empty is the payload of ctl.ping, ctl.pong and ctl.capabilities.probe.
type Empty struct{}

// SeqOnly extracts the seq of any node event.
type SeqOnly struct {
	Seq int64 `json:"seq"`
}

// Mem is the memory part of a heartbeat, in bytes.
type Mem struct {
	TotalBytes     uint64 `json:"total_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
}

// Heartbeat is node.heartbeat (node → hub), every heartbeat interval.
type Heartbeat struct {
	Seq           int64      `json:"seq"`
	UptimeS       int64      `json:"uptime_s"`
	CPU           float64    `json:"cpu"`
	Load          [3]float64 `json:"load"`
	TempC         *float64   `json:"temp_c,omitempty"`
	Battery       *float64   `json:"battery,omitempty"`
	Mem           Mem        `json:"mem"`
	Listeners     int        `json:"listeners"`
	Streams       int        `json:"streams"`
	QueueDepth    int        `json:"queue_depth"`
	ClockOffsetMS int64      `json:"clock_offset_ms"`
	NTPSynced     bool       `json:"ntp_synced"`
}

// Platform describes the node host (§4.7).
type Platform struct {
	OS       string   `json:"os"`
	Arch     string   `json:"arch"`
	CPUModel string   `json:"cpu_model,omitempty"`
	CPUCores int      `json:"cpu_cores"`
	SIMD     []string `json:"simd,omitempty"`
	RAMBytes uint64   `json:"ram_bytes"`
	Hostname string   `json:"hostname,omitempty"`
}

// SDRDriver is one supported source type.
type SDRDriver struct {
	Type      string `json:"type"`
	Available bool   `json:"available"`
	Version   string `json:"version,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// Device is one device configured in the node config file.
type Device struct {
	ID                string  `json:"id"`
	Name              string  `json:"name"`
	Type              string  `json:"type"`
	Enabled           bool    `json:"enabled"`
	FreqMin           int64   `json:"freq_min"`
	FreqMax           int64   `json:"freq_max"`
	SampleRates       []int64 `json:"sample_rates"`
	ListenPolicy      string  `json:"listen_policy,omitempty"`
	OperatorCanRetune bool    `json:"operator_can_retune"`
	AlwaysOn          bool    `json:"always_on"`
	SchedulerEnabled  bool    `json:"scheduler_enabled"`
	// Config are the driver values of the node config (SRC-022), shown
	// read-only by the hub; nil when the node does not report them.
	Config *DeviceConfig `json:"config,omitempty"`
	// GPS is the position of the device (MAP-007; additive field): its own
	// devices.<id>.gps, else the node.gps of its node; nil when neither is
	// set.
	GPS *Position `json:"gps,omitempty"`
}

// Position sources of a device.
const (
	PositionDevice = "device"
	PositionNode   = "node"
)

// Position is a position in decimal degrees (WGS 84) and where it comes
// from: the device's own position (device) or its node's (node).
type Position struct {
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
	Source string  `json:"source"`
}

// DeviceConfig are the [devices.<id>.driver] values of a device, as set in
// the node config (SRC-009 … SRC-014). Gain stages, AGC and antennas come
// with the Soapy drivers.
type DeviceConfig struct {
	// RFGain is "auto" or a value in dB.
	RFGain  string `json:"rf_gain"`
	PPM     int    `json:"ppm"`
	BiasTee bool   `json:"bias_tee"`
	// DirectSampling is off, i or q.
	DirectSampling string `json:"direct_sampling"`
	IQSwap         bool   `json:"iqswap"`
	// LFOOffset is the converter offset in Hz (signed).
	LFOOffset int64 `json:"lfo_offset"`
}

// Tool is one external program a decoder needs.
type Tool struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	OK      bool   `json:"ok"`
	// Reason says why a tool is not usable ("jt9 not found", "version
	// 2.2.0 is older than 2.3"; additive field, DIAG-004).
	Reason string `json:"reason,omitempty"`
}

// Decoder is one decoder capability.
type Decoder struct {
	Cap   string   `json:"cap"`
	Tools []Tool   `json:"tools"`
	Modes []string `json:"modes"`
}

// Codecserver describes the codecserver availability.
type Codecserver struct {
	Available bool `json:"available"`
	AMBE      bool `json:"ambe"`
}

// Capabilities is node.capabilities (node → hub, §4.7).
type Capabilities struct {
	Seq             int64       `json:"seq"`
	ProductVersion  string      `json:"product_version"`
	Protocols       []string    `json:"protocols"`
	Platform        Platform    `json:"platform"`
	SDRDrivers      []SDRDriver `json:"sdr_drivers"`
	Devices         []Device    `json:"devices"`
	DevicesDetected []any       `json:"devices_detected"`
	Decoders        []Decoder   `json:"decoders"`
	AudioCodecs     []string    `json:"audio_codecs"`
	FFTCodecs       []string    `json:"fft_codecs"`
	Codecserver     Codecserver `json:"codecserver"`
}

// DeviceState is device.state (node → hub).
type DeviceState struct {
	Seq            int64  `json:"seq"`
	DeviceID       string `json:"device_id"`
	State          string `json:"state"`
	Reason         string `json:"reason,omitempty"`
	ActivePresetID string `json:"active_preset_id,omitempty"`
	CenterFreq     *int64 `json:"center_freq,omitempty"`
	SampleRate     *int64 `json:"sample_rate,omitempty"`
	Listeners      int    `json:"listeners"`
}

// Connection is connection.opened, connection.closed and
// connection.heartbeat (node → hub).
type Connection struct {
	Seq      int64  `json:"seq"`
	CID      string `json:"cid"`
	SID      string `json:"sid,omitempty"`
	UserID   string `json:"user_id,omitempty"`
	DeviceID string `json:"device_id,omitempty"`
	IPHash   string `json:"ip_hash,omitempty"`
	Demod    string `json:"demod,omitempty"`
	Since    int64  `json:"since,omitempty"`
	// Reason is the close reason of connection.closed.
	Reason string `json:"reason,omitempty"`
}

// DecodeBatch is decode.batch (node → hub, §4.4): decoded messages for
// decoded_messages (DEC-047).
type DecodeBatch struct {
	Seq     int64    `json:"seq"`
	Decodes []Decode `json:"decodes"`
}

// Decode is one decoded message of a decode.batch.
type Decode struct {
	DeviceID string `json:"device_id"`
	// SessionID is the decoder session (decoder_session_id).
	SessionID string `json:"session_id,omitempty"`
	// PresetID is the active preset of the device, if any.
	PresetID string `json:"preset_id,omitempty"`
	Mode     string `json:"mode"`
	// Family is the decoder family of the mode (§9.4: paging, wsjt…).
	Family string `json:"family"`
	// Freq is the absolute RF frequency in Hz.
	Freq int64 `json:"freq"`
	// TS is the decode time in Unix milliseconds (the slot start for slot
	// modes).
	TS int64 `json:"ts"`
	// Source is listener or background.
	Source string `json:"source"`
	// CID is the media connection of a listener's decoder.
	CID    string `json:"cid,omitempty"`
	Schema string `json:"schema"`
	// Text is the plain-text rendering (untrusted RF text, at most 4 KiB).
	Text    string          `json:"text,omitempty"`
	Payload json.RawMessage `json:"payload"`
}

// Decode sources.
const (
	SourceListener   = "listener"
	SourceBackground = "background"
)

// EventsDropped is node.events_dropped (node → hub, §4.9).
type EventsDropped struct {
	Seq   int64            `json:"seq"`
	Count int64            `json:"count"`
	Kinds map[string]int64 `json:"kinds"`
}

// DeviceLog is device.log (node → hub, SRC-005): records of the device log
// the node keeps in RAM. It is not an event: it carries no seq, is never
// buffered while the hub is unreachable and is not acknowledged. When the
// control channel opens the node sends the records it holds (its backlog)
// with Reset set on the first message of each device; then it pushes the
// new records as they come.
type DeviceLog struct {
	DeviceID string `json:"device_id"`
	// Reset replaces the records the hub holds for the device; otherwise
	// the records are appended.
	Reset   bool        `json:"reset,omitempty"`
	Records []LogRecord `json:"records"`
}

// LogRecord is one plain-text record of a device log (at most
// MaxLogText bytes of text).
type LogRecord struct {
	// Time is the record time in Unix milliseconds (node clock).
	Time int64 `json:"t"`
	// Source is "connector" (a line the connector wrote to stderr) or
	// "device" (a device lifecycle record).
	Source string `json:"source"`
	// Class is the stderr class of a connector line, or the state of a
	// lifecycle record.
	Class string `json:"class,omitempty"`
	Text  string `json:"text"`
}

// Log record sources.
const (
	LogSourceConnector = "connector"
	LogSourceDevice    = "device"
)

// MaxLogText bounds the text of a log record (the stderr line cap).
const MaxLogText = 4096

// FileBegin is file.begin (node → hub, FIL-005): a file a decoder produced
// on the node, with its reception metadata (FIL-008) stamped by the node
// clock. Its content follows in file.chunk events, then file.end. Times are
// RFC 3339 UTC strings with a Z suffix.
type FileBegin struct {
	Seq    int64  `json:"seq"`
	FileID string `json:"file_id"`
	Kind   string `json:"kind"`
	MIME   string `json:"mime"`
	Size   int64  `json:"size"`
	// SHA256 is the hex digest of the content.
	SHA256           string `json:"sha256"`
	DeviceID         string `json:"device_id"`
	PresetID         string `json:"preset_id,omitempty"`
	DecoderSessionID string `json:"decoder_session_id,omitempty"`
	Mode             string `json:"mode"`
	FrequencyHz      int64  `json:"frequency_hz"`
	ReceivedStartUTC string `json:"received_start_utc"`
	ReceivedEndUTC   string `json:"received_end_utc,omitempty"`
	// Metadata are the per-kind details (SSTV mode, FAX LPM…): a JSON
	// object.
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

// FileChunk is file.chunk (node → hub): the content of a file from Offset,
// in standard base64.
type FileChunk struct {
	Seq     int64  `json:"seq"`
	FileID  string `json:"file_id"`
	Offset  int64  `json:"offset"`
	DataB64 string `json:"data_b64"`
}

// FileEnd is file.end (node → hub): the whole content was sent.
type FileEnd struct {
	Seq    int64  `json:"seq"`
	FileID string `json:"file_id"`
}

// MaxFileMetadata bounds the Metadata of a file.begin, as JSON: with the
// other fields, the message stays far under the 64 KiB inbound limit of the
// control channel.
const MaxFileMetadata = 4 << 10

// FileChunkBytes is the content of a full file.chunk: 45 KiB, 60 KiB in
// base64, so that the message stays under the 64 KiB inbound limit of the
// control channel (§6.9) and under the 256 KiB chunk bound of §4.4.
const FileChunkBytes = 45 << 10
