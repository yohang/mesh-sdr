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
