// Package gateway builds the Caddy JSON config of TECHNICAL_SPEC §4.6 and
// drives an embedded Caddy instance.
package gateway

import (
	"encoding/json"
	"time"

	"github.com/yohang/mesh-sdr/spikes/spk-03-caddy/pki"
)

type obj = map[string]any
type arr = []any

// Routing selects how per-node routes are expressed.
type Routing string

const (
	// RoutingPerNode: one route per node with "@id": "node-<id>" (spec §4.6
	// rules 1-4). Any add/remove is a Caddy config change, i.e. a full reload.
	RoutingPerNode Routing = "per-node"
	// RoutingDynamic: one static route /nodes/{id}/ws whose upstream is resolved
	// per request from the hub registry (custom upstream source). Enroll/remove
	// never touches the Caddy config.
	RoutingDynamic Routing = "dynamic"
)

// TLSMode of the browser-facing listener (INT-001).
type TLSMode string

const (
	TLSInternal TLSMode = "internal" // Caddy local CA (stands in for ACME in tests)
	TLSFiles    TLSMode = "files"    // operator-provided cert/key paths
	TLSACME     TLSMode = "acme"     // config only, validated with caddy.Validate
)

type Options struct {
	Listen       string // e.g. "127.0.0.1:18443"
	HTTPSPort    int
	Routing      Routing
	TLS          TLSMode
	Domain       string // public host name (ACME/internal subjects)
	ACMEEmail    string
	ACMECA       string
	OperatorCert string
	OperatorKey  string
	StorageDir   string // Caddy storage (certs, local CA). Must be on the /data volume.
	AdminSocket  string // "" = admin API disabled (embedded); unix socket path = sidecar-like driving

	HubHandlerName string // binding name for the in-process hub handler
	HubAuthzDial   string // e.g. "unix//run/meshsdr/hub.sock"
	NodeLookupName string // binding name for dynamic upstreams
	LoggerName     string // binding name for the slog bridge ("" = Caddy default stderr logger)

	CAFile, GatewayCert, GatewayKey string

	StreamCloseDelay time.Duration
	StreamTimeout    time.Duration
}

// Node is what the gateway needs to know about an enrolled node.
type Node struct {
	ID   string
	Addr string
}

func dur(d time.Duration) any {
	if d == 0 {
		return nil
	}
	return d.String()
}

func clean(m obj) obj {
	for k, v := range m {
		if v == nil {
			delete(m, k)
		}
	}
	return m
}

// nodeHandlers is the handler chain of a node route: strip client X-Rx-*,
// forward auth to the hub (token injection on 2xx, response passed through on
// non-2xx), rewrite to /ws, proxy over mTLS with the gateway client cert.
func (o Options) nodeHandlers(nodeIDExpr, serverName string, upstream obj) arr {
	proxy := clean(obj{
		"handler":            "reverse_proxy",
		"flush_interval":     -1,
		"stream_close_delay": dur(o.StreamCloseDelay),
		"stream_timeout":     dur(o.StreamTimeout),
		"headers":            obj{"request": obj{"set": obj{"X-Rx-Node-Id": arr{nodeIDExpr}}}},
		"transport": obj{
			"protocol":     "http",
			"versions":     arr{"1.1"},
			"dial_timeout": "3s",
			"tls": obj{
				"ca":                          obj{"provider": "file", "pem_files": arr{o.CAFile}},
				"client_certificate_file":     o.GatewayCert,
				"client_certificate_key_file": o.GatewayKey,
				"server_name":                 serverName,
			},
		},
	})
	for k, v := range upstream {
		proxy[k] = v
	}
	return arr{
		obj{"handler": "headers", "request": obj{"delete": arr{"X-Rx-Access-Token", "X-Rx-Node-Id", "X-Rx-Cid"}}},
		obj{
			"handler":   "reverse_proxy",
			"upstreams": arr{obj{"dial": o.HubAuthzDial}},
			"rewrite":   obj{"method": "GET", "uri": "/internal/gateway/authz?node=" + nodeIDExpr},
			"headers": obj{"request": obj{"set": obj{
				"X-Forwarded-Method": arr{"{http.request.method}"},
				"X-Forwarded-Uri":    arr{"{http.request.uri}"},
			}}},
			"handle_response": arr{obj{
				"match": obj{"status_code": arr{2}},
				"routes": arr{obj{"handle": arr{obj{
					"handler": "headers",
					"request": obj{"set": obj{
						"X-Rx-Access-Token": arr{"{http.reverse_proxy.header.X-Rx-Access-Token}"},
						"X-Rx-Cid":          arr{"{http.reverse_proxy.header.X-Rx-Cid}"},
					}},
				}}}},
			}},
		},
		obj{"handler": "rewrite", "uri": "/ws"},
		proxy,
	}
}

// NodeRoute is the per-node route object (spec §4.6 example).
func (o Options) NodeRoute(n Node) obj {
	return obj{
		"@id":      "node-" + n.ID,
		"match":    arr{obj{"path": arr{"/nodes/" + n.ID + "/ws"}}},
		"handle":   o.nodeHandlers(n.ID, n.ID+pki.NodeDNSSuffix, obj{"upstreams": arr{obj{"dial": n.Addr}}}),
		"terminal": true,
	}
}

func (o Options) dynamicNodeRoute() obj {
	h := arr{obj{"handler": "vars", "rx_node": "{http.request.uri.path.1}"}}
	h = append(h, o.nodeHandlers("{http.vars.rx_node}", "{http.vars.rx_node}"+pki.NodeDNSSuffix,
		obj{"dynamic_upstreams": obj{"source": "meshsdr_nodes", "name": o.NodeLookupName}})...)
	return obj{
		"@id": "node-dynamic",
		// slug validation (§4.1) before the id is used in a server name
		"match":    arr{obj{"path_regexp": obj{"pattern": `^/nodes/[a-z0-9][a-z0-9-]{1,62}/ws$`}}},
		"handle":   h,
		"terminal": true,
	}
}

// Config returns the complete Caddy config (loaded at hub start, §4.6 rule 1).
func (o Options) Config(nodes []Node) ([]byte, error) {
	var nodeRoutes arr
	if o.Routing == RoutingDynamic {
		nodeRoutes = arr{o.dynamicNodeRoute()}
	} else {
		nodeRoutes = arr{}
		for _, n := range nodes {
			nodeRoutes = append(nodeRoutes, o.NodeRoute(n))
		}
	}
	routes := arr{
		// dedicated subroute addressable as /id/node-routes (sidecar mode, §4.6)
		obj{"handle": arr{obj{"handler": "subroute", "@id": "node-routes", "routes": nodeRoutes}}},
		obj{"match": arr{obj{"path": arr{"/nodes/*"}}}, "handle": arr{obj{"handler": "static_response", "status_code": 404}}, "terminal": true},
		obj{"handle": arr{obj{"handler": "meshsdr_hub", "name": o.HubHandlerName}}},
	}
	server := obj{
		"listen":          arr{o.Listen},
		"protocols":       arr{"h1", "h2"}, // no h3: no UDP listener in M0
		"routes":          routes,
		"automatic_https": obj{"disable_redirects": true},
	}
	tlsApp := obj{}
	apps := obj{
		"http": obj{"https_port": o.HTTPSPort, "servers": obj{"rx": server}},
		"tls":  tlsApp,
	}
	switch o.TLS {
	case TLSFiles:
		tlsApp["certificates"] = obj{"load_files": arr{obj{"certificate": o.OperatorCert, "key": o.OperatorKey}}}
		server["tls_connection_policies"] = arr{obj{}}
		server["automatic_https"] = obj{"disable": true}
	case TLSInternal:
		tlsApp["certificates"] = obj{"automate": arr{o.Domain}}
		tlsApp["automation"] = obj{"policies": arr{obj{"subjects": arr{o.Domain}, "issuers": arr{obj{"module": "internal"}}}}}
		apps["pki"] = obj{"certificate_authorities": obj{"local": obj{"install_trust": false}}}
		server["tls_connection_policies"] = arr{obj{}}
	case TLSACME:
		tlsApp["certificates"] = obj{"automate": arr{o.Domain}}
		tlsApp["automation"] = obj{"policies": arr{obj{"subjects": arr{o.Domain}, "issuers": arr{obj{
			"module": "acme", "email": o.ACMEEmail, "ca": o.ACMECA,
		}}}}}
		server["tls_connection_policies"] = arr{obj{}}
	}
	admin := obj{"disabled": true, "config": obj{"persist": false}}
	if o.AdminSocket != "" {
		admin = obj{"listen": "unix/" + o.AdminSocket, "config": obj{"persist": false}}
	}
	cfg := obj{
		"admin":   admin,
		"storage": obj{"module": "file_system", "root": o.StorageDir},
		"apps":    apps,
	}
	if o.LoggerName != "" {
		cfg["logging"] = obj{
			// stdlib "log" output (Caddy redirects it), e.g. module cleanup errors
			"sink": obj{"writer": obj{"output": "slog", "name": o.LoggerName}},
			"logs": obj{"default": obj{
				"writer":  obj{"output": "slog", "name": o.LoggerName},
				"encoder": obj{"format": "json"},
				// DEBUG only to count records in the spike; the app would map LOG_LEVEL here
				"level": "DEBUG",
			}},
		}
	}
	return json.MarshalIndent(cfg, "", "  ")
}
