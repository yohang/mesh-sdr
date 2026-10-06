//go:build !nogateway

package gateway

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// NodeRoutePattern matches the node media route (node id slug, §4.1).
const NodeRoutePattern = `^/nodes/[a-z0-9][a-z0-9-]{1,62}/ws$`

type obj = map[string]any

// buildConfig returns the Caddy JSON config of c; binding names the
// dependencies of the custom modules.
func buildConfig(c Config, binding string) ([]byte, error) {
	pub, err := url.Parse(c.PublicURL)
	if err != nil || pub.Hostname() == "" {
		return nil, fmt.Errorf("gateway: invalid public URL %q", c.PublicURL)
	}

	host := pub.Hostname()

	if c.DialTimeout == 0 {
		c.DialTimeout = 3 * time.Second
	}

	routes := []any{
		nodeRoute(c, binding),
		obj{
			"match":    []any{obj{"path": []string{"/nodes/*", "/internal/*"}}},
			"handle":   []any{obj{"handler": "static_response", "status_code": 404}},
			"terminal": true,
		},
		obj{"handle": []any{obj{"handler": "meshsdr_hub", "binding": binding}}},
	}

	// No HTTP/3 (no UDP listener); HTTP/2 needs TLS.
	timeouts := func(s obj) obj {
		s["protocols"] = []string{"h1", "h2"}
		if _, ok := s["tls_connection_policies"]; !ok {
			s["protocols"] = []string{"h1"}
		}

		s["read_header_timeout"] = "10s"
		s["idle_timeout"] = "2m"

		return s
	}

	servers := obj{}
	httpApp := obj{"servers": servers, "grace_period": "10s"}
	apps := obj{"http": httpApp}

	if c.TLSMode == TLSOff {
		servers["hub"] = timeouts(obj{
			"listen":          []string{c.HTTPListen},
			"routes":          routes,
			"automatic_https": obj{"disable": true},
		})
	} else {
		srv := timeouts(obj{
			"listen":                  []string{c.HTTPSListen},
			"routes":                  routes,
			"tls_connection_policies": []any{obj{}},
			"automatic_https":         obj{"disable_redirects": true},
		})
		servers["hub"] = srv

		if p, err := port(c.HTTPSListen); err == nil {
			httpApp["https_port"] = p
		}

		if c.HTTPListen != "" {
			if p, err := port(c.HTTPListen); err == nil {
				httpApp["http_port"] = p
			}

			servers["redirect"] = timeouts(obj{
				"listen": []string{c.HTTPListen},
				"routes": []any{obj{"handle": []any{obj{
					"handler":     "static_response",
					"status_code": 308,
					"headers":     obj{"Location": []string{strings.TrimRight(pub.Scheme+"://"+pub.Host, "/") + "{http.request.uri}"}},
				}}}},
				"automatic_https": obj{"disable": true},
			})
		}

		switch c.TLSMode {
		case TLSFiles:
			srv["automatic_https"] = obj{"disable": true}
			apps["tls"] = obj{"certificates": obj{"load_files": []any{obj{"certificate": c.CertFile, "key": c.KeyFile}}}}
		case TLSACME, TLSInternal:
			issuer := obj{"module": "internal"}

			if c.TLSMode == TLSACME {
				issuer = obj{"module": "acme"}
				if c.ACMEEmail != "" {
					issuer["email"] = c.ACMEEmail
				}

				if c.ACMECA != "" {
					issuer["ca"] = c.ACMECA
				}

				if c.HTTPListen == "" {
					issuer["challenges"] = obj{"http": obj{"disabled": true}}
				}
			} else {
				apps["pki"] = obj{"certificate_authorities": obj{"local": obj{"install_trust": false}}}
			}

			apps["tls"] = obj{
				"certificates": obj{"automate": []string{host}},
				"automation":   obj{"policies": []any{obj{"subjects": []string{host}, "issuers": []any{issuer}}}},
			}
		default:
			return nil, fmt.Errorf("gateway: unknown TLS mode %q", c.TLSMode)
		}
	}

	writer := obj{"output": "slog", "binding": binding}

	cfg := obj{
		"admin":   obj{"disabled": true, "config": obj{"persist": false}},
		"storage": obj{"module": "file_system", "root": c.StorageDir},
		"logging": obj{
			"sink": obj{"writer": writer},
			"logs": obj{"default": obj{
				"writer":  writer,
				"encoder": obj{"format": "json"},
				"level":   caddyLevel(c.LogLevel),
			}},
		},
		"apps": apps,
	}

	return json.Marshal(cfg)
}

// nodeRoute is the static /nodes/{id}/ws route (§4.6, ADR 0002 option C).
func nodeRoute(c Config, binding string) obj {
	return obj{
		"match": []any{obj{"path_regexp": obj{"pattern": NodeRoutePattern}}},
		"handle": []any{
			obj{"handler": "headers", "request": obj{"delete": []string{"X-Rx-*"}}},
			obj{"handler": "meshsdr_node_authz", "binding": binding},
			obj{"handler": "rewrite", "uri": "/ws"},
			obj{
				"handler":            "reverse_proxy",
				"dynamic_upstreams":  obj{"source": "meshsdr_nodes"},
				"transport":          obj{"protocol": "meshsdr_node", "binding": binding, "dial_timeout": c.DialTimeout.String()},
				"flush_interval":     -1,
				"stream_close_delay": c.StreamCloseDelay.String(),
				"stream_timeout":     c.StreamTimeout.String(),
				"headers": obj{"request": obj{
					"set":    obj{HeaderNodeID: []string{"{http.vars.rx_node}"}},
					"delete": []string{HeaderUpstream},
				}},
			},
		},
		"terminal": true,
	}
}

func port(listen string) (int, error) {
	_, p, err := net.SplitHostPort(listen)
	if err != nil {
		return 0, err
	}

	var n int
	if _, err := fmt.Sscanf(p, "%d", &n); err != nil {
		return 0, err
	}

	return n, nil
}

// caddyLevel maps log.level to Caddy's level names.
func caddyLevel(level string) string {
	switch level {
	case "debug":
		return "DEBUG"
	case "warn":
		return "WARN"
	case "error":
		return "ERROR"
	default:
		return "INFO"
	}
}
