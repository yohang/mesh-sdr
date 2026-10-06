// Binary size with the minimal gateway module set (gateway/imports.go) + custom modules.
package main

import (
	"os"

	"github.com/caddyserver/caddy/v2"

	"github.com/yohang/mesh-sdr/spikes/spk-03-caddy/gateway"
)

func main() {
	cfg, _ := gateway.Options{Routing: gateway.RoutingDynamic}.Config(nil)
	if os.Getenv("RUN") != "" {
		_ = caddy.Load(cfg, true)
	}
}
