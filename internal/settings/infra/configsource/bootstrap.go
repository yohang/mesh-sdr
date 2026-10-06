// Package configsource adapts the hub config to the settings use cases.
package configsource

import (
	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/settings/app"
)

// Bootstrap lists the config-only keys of a loaded hub config.
type Bootstrap struct {
	cfg     config.Hub
	origins config.Origins
}

var _ app.Bootstrap = Bootstrap{}

// NewBootstrap returns the adapter.
func NewBootstrap(cfg config.Hub, origins config.Origins) Bootstrap {
	return Bootstrap{cfg: cfg, origins: origins}
}

// BootstrapEntries implements app.Bootstrap.
func (b Bootstrap) BootstrapEntries() []app.BootstrapEntry {
	entries := config.BootstrapEntries(b.cfg, b.origins)
	out := make([]app.BootstrapEntry, 0, len(entries))

	for _, e := range entries {
		out = append(out, app.BootstrapEntry{
			Key: e.Key, Value: e.Value, Set: e.Secret && string(e.Value) == "true",
			Origin: e.Origin.String(), Locked: e.Origin.Locked(), Secret: e.Secret,
		})
	}

	return out
}
