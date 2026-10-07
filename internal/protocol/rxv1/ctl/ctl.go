// Package ctl holds the payloads of the rx-ctl.v1 control channel
// (TECHNICAL_SPEC §4.4, ADR 0008). Like rxv1 it is pure: plain structs with
// their JSON shape, no I/O.
//
// Every node → hub event carries a per-boot monotonically increasing Seq.
// Fields added by ADR 0008 beyond the spec tables are additive and optional.
package ctl

import "github.com/yohang/mesh-sdr/internal/protocol/rxv1/token"

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
}

// Tool is one external program a decoder needs.
type Tool struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	OK      bool   `json:"ok"`
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

// EventsDropped is node.events_dropped (node → hub, §4.9).
type EventsDropped struct {
	Seq   int64            `json:"seq"`
	Count int64            `json:"count"`
	Kinds map[string]int64 `json:"kinds"`
}
