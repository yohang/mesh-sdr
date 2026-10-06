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
	Gateway Gateway               `toml:"gateway" envPrefix:"GATEWAY__" jsonschema:"description=Embedded gateway (Caddy): the public listeners\\, TLS and the node media routes (ADR 0002\\, ADR 0012)."`
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
	TLSModeACME     = "acme"
	TLSModeFiles    = "files"
	TLSModeInternal = "internal"
	TLSModeOff      = "off"
)

// Gateway is the [gateway] table: the embedded Caddy gateway, the only
// public listener of the hub (ADR 0002, ADR 0012).
type Gateway struct {
	Mode             string   `toml:"mode" env:"MODE" jsonschema:"enum=embedded,description=Gateway mode. Only embedded (Caddy linked in the hub) is implemented; sidecar is deferred (ADR 0002)."`
	HTTPSListen      string   `toml:"https_listen" env:"HTTPS_LISTEN" jsonschema:"description=HTTPS listen address (host:port) of the gateway\\, used unless gateway.tls_mode is off."`
	HTTPListen       string   `toml:"http_listen" env:"HTTP_LISTEN" jsonschema:"description=Plain HTTP listen address (host:port). With TLS it redirects to https and answers ACME HTTP-01 challenges; with gateway.tls_mode = off it serves the hub. Empty: no plain listener."`
	TLSMode          string   `toml:"tls_mode" env:"TLS_MODE" jsonschema:"enum=acme,enum=files,enum=internal,enum=off,description=Public TLS: acme (certificate from an ACME CA for the host of hub.url)\\, files (gateway.tls_cert and gateway.tls_key)\\, internal (certificate from a local CA\\, LAN use) or off (plain HTTP on gateway.http_listen\\, behind a TLS-terminating proxy or for development)."`
	TLSCert          string   `toml:"tls_cert" env:"TLS_CERT" jsonschema:"description=PEM certificate chain of the public listener (tls_mode = files). Relative paths are resolved against the config dir."`
	TLSKey           string   `toml:"tls_key" env:"TLS_KEY" jsonschema:"description=PEM private key of the public listener (tls_mode = files\\, mode 0600). Relative paths are resolved against the config dir."`
	ACMEEmail        string   `toml:"acme_email" env:"ACME_EMAIL" jsonschema:"description=Contact e-mail of the ACME account (tls_mode = acme)."`
	ACMECA           string   `toml:"acme_ca" env:"ACME_CA" jsonschema:"format=uri,description=ACME directory URL (tls_mode = acme). Empty: Let's Encrypt production."`
	StorageDir       string   `toml:"storage_dir" env:"STORAGE_DIR" jsonschema:"description=Directory of the gateway state: ACME account\\, managed certificates and the internal-issuer CA. On the data volume."`
	StreamCloseDelay Duration `toml:"stream_close_delay" env:"STREAM_CLOSE_DELAY" jsonschema:"description=Delay before proxied WebSockets are closed by a gateway reload (§4.6 rule 3)."`
	StreamTimeout    Duration `toml:"stream_timeout" env:"STREAM_TIMEOUT" jsonschema:"description=Maximum lifetime of a proxied WebSocket (§4.6 rule 3)."`
	MaxBody          Size     `toml:"max_body" env:"MAX_BODY" jsonschema:"description=Request body limit of the hub API (§4.6)."`
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
	Log      Log                     `toml:"log" envPrefix:"LOG__" jsonschema:"description=Process logging."`
}

// DeviceConfig is one [devices.<id>] table (TECHNICAL_SPEC §7.4). Driver
// settings arrive with the device epic.
type DeviceConfig struct {
	Name              string    `toml:"name" env:"-" jsonschema:"description=Required. Display name."`
	Type              string    `toml:"type" env:"-" jsonschema:"description=Required. Driver type\\, for example rtl_sdr or soapy:sdrplay."`
	Enabled           *bool     `toml:"enabled" env:"-" jsonschema:"description=Whether the device is used (default true)."`
	FreqRange         FreqRange `toml:"freq_range" env:"-" jsonschema:"description=Required. Tunable frequency range."`
	SampleRates       []int64   `toml:"sample_rates" env:"-" jsonschema:"description=Required. Supported sample rates (S/s)."`
	ListenPolicy      string    `toml:"listen_policy" env:"-" jsonschema:"enum=,enum=anonymous,enum=registered,description=Overrides the global listen policy for this device."`
	OperatorCanRetune bool      `toml:"operator_can_retune" env:"-" jsonschema:"description=Operators may retune the device."`
	AlwaysOn          bool      `toml:"always_on" env:"-" jsonschema:"description=Keep the device running without listeners."`
	SchedulerEnabled  bool      `toml:"scheduler_enabled" env:"-" jsonschema:"description=The hub scheduler may switch presets on this device."`
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
			Mode: "embedded", HTTPSListen: ":443", TLSMode: TLSModeACME, StorageDir: "/var/lib/meshsdr/caddy",
			StreamCloseDelay: MustDuration("2h"), StreamTimeout: MustDuration("24h"), MaxBody: MustSize("1MiB"),
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
		Node: NodeSection{Listen: "0.0.0.0:8074", EventBuffer: EventBuffer{MaxEvents: 10000, MaxBytes: MustSize("16MiB")}},
		Log:  defaultLog(),
	}
}

func defaultLog() Log { return Log{Level: "info", Format: "json"} }
