// Package config loads the hub and node configuration (TECHNICAL_SPEC §7.4):
// TOML v1.0 files (<role>.toml then <role>.d/*.toml drop-ins, lexical order)
// overridden by MESHSDR_* env vars, with per-key origin tracking, validation
// at load and a JSON Schema generated from the structs below.
//
// The structs are the source of truth. Every leaf has a toml tag (key name),
// an env tag (its upper-cased name; nested tables use envPrefix "<TABLE>__")
// and a jsonschema description.
package config

import "path/filepath"

// SchemaVersion is the only config schema_version this binary accepts.
const SchemaVersion = 1

// DefaultDir is the default config directory.
const DefaultDir = "/etc/meshsdr"

// Role is a process role reading its own config files.
type Role string

// Roles.
const (
	RoleHub  Role = "hub"
	RoleNode Role = "node"
)

// Hub is the hub configuration: hub.toml and hub.d/*.toml.
type Hub struct {
	SchemaVersion      int  `toml:"schema_version" env:"-" jsonschema:"required,enum=1,description=Config schema version. Required in every file."`
	AllowInlineSecrets bool `toml:"allow_inline_secrets" env:"-" jsonschema:"description=Accept inline secret values in this file (startup warning). Applies only to the file that sets it."`

	Hub     HubSection            `toml:"hub" envPrefix:"HUB__" jsonschema:"description=Hub bootstrap."`
	Gateway Gateway               `toml:"gateway" envPrefix:"GATEWAY__" jsonschema:"description=Gateway: the public listeners\\, TLS and the node media routes (ADR 0002\\, ADR 0012\\, ADR 0021)."`
	DB      DB                    `toml:"db" envPrefix:"DB__" jsonschema:"description=Database (through the DB adapter)."`
	TLS     HubTLS                `toml:"tls" envPrefix:"TLS__" jsonschema:"description=Hub internal CA for hub <-> node mTLS (ADR 0008). Without it the grid is disabled."`
	Nodes   map[string]ConfigNode `toml:"nodes" env:"-" jsonschema:"description=Nodes declared in the hub config\\, keyed by node id. They are locked in Admin > Nodes. File-only (no env override)."`
	Log     Log                   `toml:"log" envPrefix:"LOG__" jsonschema:"description=Process logging."`

	Settings Settings `toml:"settings" envPrefix:"SETTINGS__" jsonschema_description:"Admin settings (ADR 0010). Each key set in a file or the env is locked (read-only in the admin UI); unset keys fall back to the DB setting, then to the default."`

	Auth  Auth  `toml:"auth" envPrefix:"AUTH__" jsonschema:"description=Authentication (password hashing)."`
	Admin Admin `toml:"admin" envPrefix:"ADMIN__" jsonschema:"description=Admin access restrictions."`
	HTTP  HTTP  `toml:"http" envPrefix:"HTTP__" jsonschema:"description=HTTP front: reverse proxies."`
	SMTP  SMTP  `toml:"smtp" envPrefix:"SMTP__" jsonschema:"description=Outgoing mail (invitations, password reset, security notices). Without smtp.host, links are shown to copy and no mail is sent."`
}

// ConfigNode is one [nodes.<id>] table.
type ConfigNode struct {
	URL             string `toml:"url" env:"-" jsonschema:"description=Required. Base URL the hub dials: https://host:port (node API and control WebSocket)."`
	Name            string `toml:"name" env:"-" jsonschema:"description=Display name (defaults to the node id)."`
	EnrollmentToken Secret `toml:"enrollment_token" env:"-" jsonschema:"description=Enrollment token of the node: 32 random bytes in unpadded base64url (43 characters)\\, used until the node is enrolled."`
}

// SMTP is the [smtp] table (TECHNICAL_SPEC §7.4, SR-09).
type SMTP struct {
	Host          string `toml:"host" env:"HOST" jsonschema:"description=Mail relay host name. Empty: mail is off."`
	Port          int    `toml:"port" env:"PORT" jsonschema:"minimum=1,maximum=65535,description=Mail relay port (587 for STARTTLS, 465 for implicit TLS)."`
	TLS           string `toml:"tls" env:"TLS" jsonschema:"enum=starttls,enum=implicit,enum=none,description=Transport security: starttls or implicit TLS, with a verified certificate. none (plain text) needs smtp.allow_insecure."`
	Username      string `toml:"username" env:"USERNAME" jsonschema:"description=SMTP user name (empty: no authentication)."`
	Password      Secret `toml:"password" env:"PASSWORD" jsonschema:"description=SMTP password."`
	From          string `toml:"from" env:"FROM" jsonschema:"description=Sender address, for example \"WebSDR <sdr@example.org>\". Required with smtp.host."`
	AllowInsecure bool   `toml:"allow_insecure" env:"ALLOW_INSECURE" jsonschema:"description=Allow smtp.tls = none (development relays only): mail and credentials travel in clear."`
}

// Enabled reports whether outgoing mail is configured.
func (s SMTP) Enabled() bool { return s.Host != "" }

// Auth is the [auth] table.
type Auth struct {
	TokenKeyDir     string   `toml:"token_key_dir" env:"TOKEN_KEY_DIR" jsonschema:"description=Directory of the Ed25519 keys that sign access tokens (0700\\, one 0600 file per key\\, written by the hub). On the state volume\\, never in the database. Relative paths are resolved against the working directory."`
	TokenTTL        Duration `toml:"token_ttl" env:"TOKEN_TTL" jsonschema:"description=Access token lifetime (60s to 10m)."`
	KeyRotationDays int      `toml:"key_rotation_days" env:"KEY_ROTATION_DAYS" jsonschema:"minimum=1,maximum=365,description=Age in days at which a new token signing key is introduced."`
	Argon2          Argon2   `toml:"argon2" envPrefix:"ARGON2__" jsonschema:"description=Argon2id password hashing parameters. A stored hash with other parameters is re-hashed at the next successful login."`
}

// Argon2 is the [auth.argon2] table.
type Argon2 struct {
	MemoryKiB   uint32 `toml:"memory_kib" env:"MEMORY_KIB" jsonschema:"minimum=19456,description=Argon2id memory cost in KiB (minimum 19456)."`
	Iterations  uint32 `toml:"iterations" env:"ITERATIONS" jsonschema:"minimum=2,description=Argon2id time cost (minimum 2)."`
	Parallelism uint8  `toml:"parallelism" env:"PARALLELISM" jsonschema:"minimum=1,maximum=255,description=Argon2id lanes (1..255)."`
}

// Argon2 parameter floors (TECHNICAL_SPEC §7.4 "Validation at startup", SR-03).
const (
	Argon2MinMemoryKiB  = 19456
	Argon2MinIterations = 2
)

// Admin is the [admin] table.
type Admin struct {
	AllowedNetworks []string `toml:"allowed_networks" env:"ALLOWED_NETWORKS" jsonschema:"description=Client networks (CIDR) allowed to use admin endpoints and admin WebSocket topics. Checked on every admin request against the resolved client address."`
}

// HTTP is the [http] table.
type HTTP struct {
	TrustedProxies []string `toml:"trusted_proxies" env:"TRUSTED_PROXIES" jsonschema:"description=Reverse proxies (CIDR) trusted for X-Forwarded-For. The client address is the right-most untrusted hop; forwarding headers from other peers are ignored."`
}

// HubTLS is the [tls] table of the hub: the internal CA. The hub client
// certificate is minted in memory from the CA key.
type HubTLS struct {
	CACert string `toml:"ca_cert" env:"CA_CERT" jsonschema:"description=PEM file of the hub internal CA certificate (relative paths are resolved against the config dir). Created by meshsdr hub ca init."`
	CAKey  Secret `toml:"ca_key" env:"CA_KEY" jsonschema:"description=Private key of the hub internal CA (PEM)\\, required with tls.ca_cert. Signs node certificates."`
}

// HubSection is the [hub] table.
type HubSection struct {
	URL              string `toml:"url" env:"URL" jsonschema:"format=uri,description=Required (file or env). Public base URL of the hub (links in e-mails and WebSocket Origin check). Must be https unless hub.allow_insecure_url is true."`
	AllowInsecureURL bool   `toml:"allow_insecure_url" env:"ALLOW_INSECURE_URL" jsonschema:"description=Allow a non-https hub.url (development only)."`
}

// Gateway TLS modes (INT-001).
const (
	TLSModeACME  = "acme"
	TLSModeFiles = "files"
	TLSModeOff   = "off"
)

// Gateway is the [gateway] table: the hub gateway, the only public
// listener of the hub (ADR 0002, ADR 0012, ADR 0021).
type Gateway struct {
	HTTPSListen   string   `toml:"https_listen" env:"HTTPS_LISTEN" jsonschema:"description=HTTPS listen address (host:port) of the gateway\\, used unless gateway.tls_mode is off."`
	HTTPListen    string   `toml:"http_listen" env:"HTTP_LISTEN" jsonschema:"description=Plain HTTP listen address (host:port). With TLS it redirects to https and answers ACME HTTP-01 challenges; with gateway.tls_mode = off it serves the hub. Empty: no plain listener."`
	TLSMode       string   `toml:"tls_mode" env:"TLS_MODE" jsonschema:"enum=acme,enum=files,enum=off,description=Public TLS: acme (certificate from an ACME CA for the host of hub.url)\\, files (gateway.tls_cert and gateway.tls_key)\\, or off (plain HTTP on gateway.http_listen\\, behind a TLS-terminating proxy or for development)."`
	TLSCert       string   `toml:"tls_cert" env:"TLS_CERT" jsonschema:"description=PEM certificate chain of the public listener (tls_mode = files). Relative paths are resolved against the config dir."`
	TLSKey        string   `toml:"tls_key" env:"TLS_KEY" jsonschema:"description=PEM private key of the public listener (tls_mode = files\\, mode 0600). Relative paths are resolved against the config dir."`
	ACMEEmail     string   `toml:"acme_email" env:"ACME_EMAIL" jsonschema:"description=Contact e-mail of the ACME account (tls_mode = acme)."`
	ACMECA        string   `toml:"acme_ca" env:"ACME_CA" jsonschema:"format=uri,description=ACME directory URL (tls_mode = acme). Empty: Let's Encrypt production."`
	StorageDir    string   `toml:"storage_dir" env:"STORAGE_DIR" jsonschema:"description=Directory of the ACME account and certificates (tls_mode = acme). On the data volume."`
	StreamTimeout Duration `toml:"stream_timeout" env:"STREAM_TIMEOUT" jsonschema:"description=Maximum lifetime of a proxied WebSocket (§4.6 rule 3)."`
	MaxBody       Size     `toml:"max_body" env:"MAX_BODY" jsonschema:"description=Request body limit of the hub API (§4.6)."`
}

// DB is the [db] table.
type DB struct {
	DSN                string `toml:"dsn" env:"DSN" jsonschema:"description=Database DSN. Only the sqlite: scheme is supported (sqlite:///abs/path or sqlite:relative/path)."`
	MaxReadConnections int    `toml:"max_read_connections" env:"MAX_READ_CONNECTIONS" jsonschema:"minimum=1,maximum=64,description=Size of the read-only connection pool."`
}

// Log is the [log] table, shared by every role.
type Log struct {
	Level  string `toml:"level" env:"LEVEL" jsonschema:"enum=debug,enum=info,enum=warn,enum=error,description=Log level. --debug overrides it."`
	Format string `toml:"format" env:"FORMAT" jsonschema:"enum=text,enum=json,description=Log format. --json overrides it."`
}

// Node is the node configuration: node.toml and node.d/*.toml.
type Node struct {
	SchemaVersion      int  `toml:"schema_version" env:"-" jsonschema:"required,enum=1,description=Config schema version. Required in every file."`
	AllowInlineSecrets bool `toml:"allow_inline_secrets" env:"-" jsonschema:"description=Accept inline secret values in this file (startup warning). Applies only to the file that sets it."`

	Node     NodeSection             `toml:"node" envPrefix:"NODE__" jsonschema:"description=Node bootstrap."`
	TLS      NodeTLS                 `toml:"tls" envPrefix:"TLS__" jsonschema:"description=Node certificate for hub <-> node mTLS\\, written by meshsdr node enroll."`
	HubTrust HubTrust                `toml:"hub_trust" envPrefix:"HUB_TRUST__" jsonschema:"description=Trust in the hub: CA\\, expected hub identity\\, enrollment token."`
	Devices  map[string]DeviceConfig `toml:"devices" env:"-" jsonschema:"description=SDR devices of this node\\, keyed by device id (^[a-z0-9][a-z0-9_-]{0\\,62}$\\, unique across the hub). Mirrored read-only into the hub device registry. File-only (no env override)."`
	Tools    Tools                   `toml:"tools" envPrefix:"TOOLS__" jsonschema:"description=External programs run by the node (SDR connectors and decoders)."`
	Decoders NodeDecoders            `toml:"decoders" envPrefix:"DECODERS__" jsonschema:"description=Decoder resources of this node."`
	Log      Log                     `toml:"log" envPrefix:"LOG__" jsonschema:"description=Process logging."`
}

// Tools is the [tools] table of the node (TECHNICAL_SPEC §7.4, ADR 0017
// decision 10): a tool is its tools.<name> absolute path when set,
// otherwise found by name in tools.dirs, which is also the child's PATH.
type Tools struct {
	Dirs            []string `toml:"dirs" env:"DIRS" jsonschema:"description=Absolute directories searched for tools without a path; also the PATH of every tool. The node's own PATH is never used."`
	RTLConnector    string   `toml:"rtl_connector" env:"RTL_CONNECTOR" jsonschema:"description=Absolute path of rtl_connector (owrx_connector\\, rtl_sdr devices)."`
	RTLTCPConnector string   `toml:"rtl_tcp_connector" env:"RTL_TCP_CONNECTOR" jsonschema:"description=Absolute path of rtl_tcp_connector (owrx_connector\\, rtl_tcp devices)."`
	JT9             string   `toml:"jt9" env:"JT9" jsonschema:"description=Absolute path of jt9 (WSJT-X decoders)."`
	WSPRD           string   `toml:"wsprd" env:"WSPRD" jsonschema:"description=Absolute path of wsprd (WSJT-X WSPR decoder)."`
	JS8             string   `toml:"js8" env:"JS8" jsonschema:"description=Absolute path of js8 (JS8Call decoder)."`
	Direwolf        string   `toml:"direwolf" env:"DIREWOLF" jsonschema:"description=Absolute path of direwolf (packet and APRS)."`
	MultimonNG      string   `toml:"multimon_ng" env:"MULTIMON_NG" jsonschema:"description=Absolute path of multimon-ng (paging\\, SelCall\\, ZVEI\\, EAS)."`
	RTL433          string   `toml:"rtl_433" env:"RTL_433" jsonschema:"description=Absolute path of rtl_433 (ISM sensors)."`
	CWSkimmer       string   `toml:"csdr_cwskimmer" env:"CSDR_CWSKIMMER" jsonschema:"description=Absolute path of csdr-cwskimmer (CW skimmer)."`
	RTTYSkimmer     string   `toml:"csdr_rttyskimmer" env:"CSDR_RTTYSKIMMER" jsonschema:"description=Absolute path of csdr-rttyskimmer (RTTY skimmer)."`
}

// tool is one [tools] path key: its TOML key, the program name and its
// path.
type tool struct {
	key, name, path string
}

func (t Tools) all() []tool {
	return []tool{
		{"rtl_connector", "rtl_connector", t.RTLConnector},
		{"rtl_tcp_connector", "rtl_tcp_connector", t.RTLTCPConnector},
		{"jt9", "jt9", t.JT9},
		{"wsprd", "wsprd", t.WSPRD},
		{"js8", "js8", t.JS8},
		{"direwolf", "direwolf", t.Direwolf},
		{"multimon_ng", "multimon-ng", t.MultimonNG},
		{"rtl_433", "rtl_433", t.RTL433},
		{"csdr_cwskimmer", "csdr-cwskimmer", t.CWSkimmer},
		{"csdr_rttyskimmer", "csdr-rttyskimmer", t.RTTYSkimmer},
	}
}

// Paths returns the configured tool paths by program name.
func (t Tools) Paths() map[string]string {
	out := map[string]string{}

	for _, tl := range t.all() {
		if tl.path != "" {
			out[tl.name] = tl.path
		}
	}

	return out
}

// DeviceConfig is one [devices.<id>] table (TECHNICAL_SPEC §7.4).
type DeviceConfig struct {
	Name              string    `toml:"name" env:"-" jsonschema:"description=Required. Display name."`
	Type              string    `toml:"type" env:"-" jsonschema:"description=Required. Driver type\\, for example rtl_sdr or soapy:sdrplay."`
	Enabled           *bool     `toml:"enabled" env:"-" jsonschema:"description=Whether the device is used (default true)."`
	FreqRange         FreqRange `toml:"freq_range" env:"-" jsonschema:"description=Required. Tunable frequency range."`
	SampleRates       []int64   `toml:"sample_rates" env:"-" jsonschema:"description=Required. Supported sample rates (S/s)."`
	CenterFreq        Frequency `toml:"center_freq" env:"-" jsonschema:"description=Centre frequency at start\\, within freq_range (default: the capture band starts at the bottom of freq_range)."`
	SampleRate        int64     `toml:"sample_rate" env:"-" jsonschema:"minimum=0,description=Sample rate at start\\, one of sample_rates (default: the first one)."`
	ListenPolicy      string    `toml:"listen_policy" env:"-" jsonschema:"enum=,enum=anonymous,enum=registered,description=Overrides the global listen policy for this device."`
	OperatorCanRetune bool      `toml:"operator_can_retune" env:"-" jsonschema:"description=Operators may retune the device."`
	AlwaysOn          bool      `toml:"always_on" env:"-" jsonschema:"description=Keep the device running without listeners."`
	SchedulerEnabled  bool      `toml:"scheduler_enabled" env:"-" jsonschema:"description=The hub scheduler may switch presets on this device."`
	MaxDemods         int       `toml:"max_demods" env:"-" jsonschema:"minimum=0,maximum=1000,description=Demodulators the device runs at once (0: interim default 16)."`
	AutoRecover       *bool     `toml:"auto_recover" env:"-" jsonschema:"description=Restart a failed device every 15 minutes (default true)."`
	Driver            Driver    `toml:"driver" env:"-" jsonschema:"description=Connector settings (rtl_sdr\\, rtl_tcp)."`
}

// Driver is the [devices.<id>.driver] table (§7.4 example).
type Driver struct {
	Device         string `toml:"device" env:"-" jsonschema:"description=rtl_sdr: device index or serial (default 0); rtl_tcp: host:port of the rtl_tcp server."`
	PPM            int    `toml:"ppm" env:"-" jsonschema:"minimum=-1000,maximum=1000,description=Frequency correction in ppm."`
	RFGain         Gain   `toml:"rf_gain" env:"-" jsonschema:"description=RF gain: auto (default) or a value in dB."`
	IQSwap         bool   `toml:"iqswap" env:"-" jsonschema:"description=Swap I and Q (reversed spectrum)."`
	BiasTee        bool   `toml:"bias_tee" env:"-" jsonschema:"description=Power the bias-tee (active antenna or LNA)\\, if the hardware has one."`
	DirectSampling string `toml:"direct_sampling" env:"-" jsonschema:"enum=,enum=off,enum=i,enum=q,description=Direct sampling input for HF without upconverter: off (default)\\, i or q."`
	LFOOffset      int64  `toml:"lfo_offset" env:"-" jsonschema:"description=Local oscillator offset of an up- or downconverter in Hz (signed): the hardware tunes to the centre frequency + lfo_offset."`
}

// FreqRange is a frequency range.
type FreqRange struct {
	Min Frequency `toml:"min" env:"-" jsonschema:"description=Lowest tunable frequency."`
	Max Frequency `toml:"max" env:"-" jsonschema:"description=Highest tunable frequency."`
}

// NodeTLS is the [tls] table of the node.
type NodeTLS struct {
	Cert string `toml:"cert" env:"CERT" jsonschema:"description=PEM file of the node certificate issued by the hub CA (relative paths are resolved against the config dir). Written by meshsdr node enroll\\, rewritten on renewal."`
	Key  string `toml:"key" env:"KEY" jsonschema:"description=PEM file of the node private key (mode 0600). Written by meshsdr node enroll."`
}

// HubTrust is the [hub_trust] table of the node.
type HubTrust struct {
	CACert          string `toml:"ca_cert" env:"CA_CERT" jsonschema:"description=PEM file of the hub CA certificate. Only hub certificates from this CA are accepted. Written by meshsdr node enroll."`
	CAFingerprint   string `toml:"ca_fingerprint" env:"CA_FINGERPRINT" jsonschema:"description=SHA-256 fingerprint of the hub CA certificate (hex\\, colons optional)\\, shown by the hub with the enrollment token."`
	HubIdentity     string `toml:"hub_identity" env:"HUB_IDENTITY" jsonschema:"description=Expected hub id (host of hub.url) in the hub client certificate. Empty accepts any hub certificate from the CA."`
	EnrollmentToken Secret `toml:"enrollment_token" env:"ENROLLMENT_TOKEN" jsonschema:"description=Single-use enrollment token issued by the hub\\, used by meshsdr node enroll."`
}

// ResolvePath returns path resolved against the config dir dir.
func ResolvePath(dir, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}

	return filepath.Join(dir, path)
}

func (h *Hub) resolvePaths(dir string) {
	h.TLS.CACert = ResolvePath(dir, h.TLS.CACert)
	h.Gateway.TLSCert = ResolvePath(dir, h.Gateway.TLSCert)
	h.Gateway.TLSKey = ResolvePath(dir, h.Gateway.TLSKey)
}

func (n *Node) resolvePaths(dir string) {
	n.TLS.Cert = ResolvePath(dir, n.TLS.Cert)
	n.TLS.Key = ResolvePath(dir, n.TLS.Key)
	n.HubTrust.CACert = ResolvePath(dir, n.HubTrust.CACert)
}

// NodeSection is the [node] table.
type NodeSection struct {
	ID          string      `toml:"id" env:"ID" jsonschema:"pattern=^[a-z0-9][a-z0-9-]{1\\,62}$,description=Required (file or env). Stable node id (slug). Must not change after enrollment."`
	Listen      string      `toml:"listen" env:"LISTEN" jsonschema:"description=Node API and WebSocket listen address (host:port). TLS only."`
	EventBuffer EventBuffer `toml:"event_buffer" envPrefix:"EVENT_BUFFER__" jsonschema:"description=RAM buffer of node events while the hub is unreachable (dropped by priority on overflow)."`
	RuntimeDir  string      `toml:"runtime_dir" env:"RUNTIME_DIR" jsonschema:"description=Private runtime directory (absolute\\, mode 0700\\, owned by the node user): the per-tool workdirs live in sessions/ under it. The only place the node writes."`
	// IPCPortRange is "lo-hi".
	IPCPortRange   string `toml:"ipc_port_range" env:"IPC_PORT_RANGE" jsonschema:"pattern=^[0-9]{4\\,5}-[0-9]{4\\,5}$,description=Loopback ports for the connector IQ and control sockets (lo-hi)."`
	MaxDemods      int    `toml:"max_demods" env:"MAX_DEMODS" jsonschema:"minimum=1,maximum=1000,description=Demodulators the node runs at once\\, all devices together (interim default 32)."`
	WSNotSentLowat Size   `toml:"ws_notsent_lowat" env:"WS_NOTSENT_LOWAT" jsonschema:"description=TCP_NOTSENT_LOWAT of the node API sockets: bounds the kernel send buffering of media WebSockets (ADR 0004)."`
}

// NodeDecoders is the [decoders] table of the node: its decoder resources
// (TECHNICAL_SPEC §8.3 rule 6, DEC-025, DEC-048). The decoding settings
// themselves are hub DB settings pushed in the desired state.
type NodeDecoders struct {
	BatchWorkers  int           `toml:"batch_workers" env:"BATCH_WORKERS" jsonschema:"minimum=0,maximum=256,description=Workers of the batch decoders (WSJT family and JS8)\\, shared by every session; 0: half the CPU cores (at least 1)."`
	QueueLength   int           `toml:"queue_length" env:"QUEUE_LENGTH" jsonschema:"minimum=1,maximum=1000,description=Batch decoder jobs waiting for a worker; on overflow the oldest job is dropped."`
	MaxSessions   int           `toml:"max_sessions" env:"MAX_SESSIONS" jsonschema:"minimum=0,maximum=10000,description=Decoder sessions the node runs at once\\, all listeners together; beyond it a decoder is unavailable (node busy). 0: twice the CPU cores."`
	ProcessLimits ProcessLimits `toml:"process_limits" envPrefix:"PROCESS_LIMITS__" jsonschema:"description=Limits of every external decoder process (DEC-048)\\, applied between fork and exec."`
}

// BatchWorkerCount returns the batch workers: BatchWorkers, or half of
// cores (at least 1) when it is 0.
func (d NodeDecoders) BatchWorkerCount(cores int) int {
	if d.BatchWorkers > 0 {
		return d.BatchWorkers
	}

	return max(1, cores/2)
}

// SessionCap returns the decoder session cap: MaxSessions, or twice cores
// when it is 0.
func (d NodeDecoders) SessionCap(cores int) int {
	if d.MaxSessions > 0 {
		return d.MaxSessions
	}

	return max(1, 2*cores)
}

// ProcessLimits is the [decoders.process_limits] table (DEC-048, ADR 0017
// decision 7).
type ProcessLimits struct {
	Nice      int  `toml:"nice" env:"NICE" jsonschema:"minimum=0,maximum=19,description=CPU niceness of the decoder processes (0 to 19): the DSP keeps the priority."`
	OpenFiles int  `toml:"open_files" env:"OPEN_FILES" jsonschema:"minimum=16,maximum=1048576,description=Open-files limit (RLIMIT_NOFILE) of each decoder process."`
	Memory    Size `toml:"memory" env:"MEMORY" jsonschema:"description=Address-space limit (RLIMIT_AS) of each decoder process; 0: none (the deployment caps the node as a whole). Tools built with Go or with many threads need 1GiB or more."`
}

// EventBuffer is the [node.event_buffer] table.
type EventBuffer struct {
	MaxEvents int  `toml:"max_events" env:"MAX_EVENTS" jsonschema:"minimum=100,maximum=10000000,description=Maximum number of buffered events."`
	MaxBytes  Size `toml:"max_bytes" env:"MAX_BYTES" jsonschema:"description=Maximum size of buffered events."`
}

// DefaultHub returns the hub defaults.
func DefaultHub() Hub {
	return Hub{
		Gateway: Gateway{
			HTTPSListen: ":443", TLSMode: TLSModeACME, StorageDir: "/var/lib/meshsdr/acme",
			StreamTimeout: MustDuration("24h"), MaxBody: MustSize("1MiB"),
		},
		DB:       DB{DSN: "sqlite:///var/lib/meshsdr/hub.db", MaxReadConnections: 4},
		Log:      defaultLog(),
		Settings: DefaultSettings(),
		Auth: Auth{
			TokenKeyDir: "/var/lib/meshsdr/keys", TokenTTL: MustDuration("5m"), KeyRotationDays: 30,
			Argon2: Argon2{MemoryKiB: 65536, Iterations: 3, Parallelism: 1},
		},
		Admin: Admin{AllowedNetworks: []string{"0.0.0.0/0", "::/0"}},
		HTTP:  HTTP{TrustedProxies: []string{}},
		SMTP:  SMTP{Port: 587, TLS: "starttls"},
	}
}

// DefaultNode returns the node defaults.
func DefaultNode() Node {
	return Node{
		Node: NodeSection{
			Listen: "0.0.0.0:8074", EventBuffer: EventBuffer{MaxEvents: 10000, MaxBytes: MustSize("16MiB")},
			RuntimeDir: "/run/meshsdr-node", IPCPortRange: "40000-40999", WSNotSentLowat: MustSize("16KiB"), MaxDemods: 32,
		},
		Tools: Tools{Dirs: []string{"/usr/local/bin", "/usr/bin"}},
		Decoders: NodeDecoders{
			QueueLength:   10,
			ProcessLimits: ProcessLimits{Nice: 10, OpenFiles: 1024},
		},
		Log: defaultLog(),
	}
}

func defaultLog() Log { return Log{Level: "info", Format: "json"} }
