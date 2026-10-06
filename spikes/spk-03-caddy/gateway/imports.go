package gateway

// Minimal Caddy module set for the gateway (instead of modules/standard).
import (
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp"              // http app, matchers, subroute, vars, static_response
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp/headers"      // headers handler
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy" // reverse_proxy + http transport
	_ "github.com/caddyserver/caddy/v2/modules/caddyhttp/rewrite"      // rewrite handler
	_ "github.com/caddyserver/caddy/v2/modules/caddypki"               // pki app + internal issuer
	_ "github.com/caddyserver/caddy/v2/modules/caddytls"               // tls app, load_files, automate, acme issuer
	_ "github.com/caddyserver/caddy/v2/modules/filestorage"            // caddy.storage.file_system
	_ "github.com/caddyserver/caddy/v2/modules/logging"                // json encoder
)
