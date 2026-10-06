// Binary size with Caddy's full standard module set (what `caddy` itself ships),
// i.e. roughly the sidecar binary.
package main

import (
	"os"

	"github.com/caddyserver/caddy/v2"
	_ "github.com/caddyserver/caddy/v2/modules/standard"
)

func main() {
	if os.Getenv("RUN") != "" {
		_ = caddy.Load([]byte(`{}`), true)
	}
}
