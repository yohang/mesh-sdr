// Package config loads the hub and node configuration (TECHNICAL_SPEC §7.4):
// TOML v1.0 files (<role>.toml then <role>.d/*.toml drop-ins, lexical order)
// overridden by MESHSDR_* env vars, with per-key origin tracking, validation
// at load and a JSON Schema generated from the structs below.
//
// The structs are the source of truth. Every leaf has a toml tag (key name),
// an env tag (its upper-cased name; nested tables use envPrefix "<TABLE>__")
// and a jsonschema description.
package config

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

	Hub HubSection `toml:"hub" envPrefix:"HUB__" jsonschema:"description=Hub bootstrap."`
	DB  DB         `toml:"db" envPrefix:"DB__" jsonschema:"description=Database (through the DB adapter)."`
	Log Log        `toml:"log" envPrefix:"LOG__" jsonschema:"description=Process logging."`
}

// HubSection is the [hub] table.
type HubSection struct {
	Listen           string `toml:"listen" env:"LISTEN" jsonschema:"description=Hub HTTP listen address (host:port). The address decides IPv4 or IPv6."`
	URL              string `toml:"url" env:"URL" jsonschema:"format=uri,description=Required (file or env). Public base URL of the hub (links in e-mails and WebSocket Origin check). Must be https unless hub.allow_insecure_url is true."`
	AllowInsecureURL bool   `toml:"allow_insecure_url" env:"ALLOW_INSECURE_URL" jsonschema:"description=Allow a non-https hub.url (development only)."`
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

	Node NodeSection `toml:"node" envPrefix:"NODE__" jsonschema:"description=Node bootstrap."`
	Log  Log         `toml:"log" envPrefix:"LOG__" jsonschema:"description=Process logging."`
}

// NodeSection is the [node] table.
type NodeSection struct {
	ID     string `toml:"id" env:"ID" jsonschema:"pattern=^[a-z0-9][a-z0-9-]{1\\,62}$,description=Required (file or env). Stable node id (slug). Must not change after enrollment."`
	Listen string `toml:"listen" env:"LISTEN" jsonschema:"description=Node API and WebSocket listen address (host:port). TLS only."`
}

// DefaultHub returns the hub defaults.
func DefaultHub() Hub {
	return Hub{
		Hub: HubSection{Listen: "0.0.0.0:8073"},
		DB:  DB{DSN: "sqlite:///var/lib/meshsdr/hub.db", MaxReadConnections: 4},
		Log: defaultLog(),
	}
}

// DefaultNode returns the node defaults.
func DefaultNode() Node {
	return Node{
		Node: NodeSection{Listen: "0.0.0.0:8074"},
		Log:  defaultLog(),
	}
}

func defaultLog() Log { return Log{Level: "info", Format: "json"} }
