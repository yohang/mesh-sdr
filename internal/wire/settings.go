package wire

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/settings"
	"github.com/yohang/mesh-sdr/internal/shared/audit"
)

// newSettings builds the settings store (ADR 0010: DB settings, config
// locking and precedence) and loads the DB settings, and the effective
// configuration view of Admin › System.
func newSettings(ctx context.Context, cfg config.Hub, origins config.Origins, adapter *db.DB, audit audit.Appender,
	now func() time.Time, logger *slog.Logger,
) (*settings.Store, *settings.EffectiveConfig, error) {
	catalog, err := config.NewSettingsCatalog(cfg, origins)
	if err != nil {
		return nil, nil, fmt.Errorf("settings catalog: %w", err)
	}

	store := settings.NewStore(settings.StoreDeps{
		Repo: settings.NewSettings(adapter, config.SchemaVersion), Catalog: catalog, Tx: adapter, Audit: audit, Now: now,
		Logger: component(logger, "settings.app.store"),
	})

	if err := store.Load(ctx); err != nil {
		return nil, nil, fmt.Errorf("load settings: %w", err)
	}

	return store, settings.NewEffectiveConfig(configBootstrap{cfg: cfg, origins: origins}, store, now), nil
}

// storeListenPolicy reads the global listen policy (listen_policy) from the
// settings store for the grid feature summary.
type storeListenPolicy struct {
	store interface{ String(key string) string }
}

// ListenPolicy implements grid/app.GlobalListenPolicy.
func (p storeListenPolicy) ListenPolicy(context.Context) (string, error) {
	return p.store.String("listen_policy"), nil
}

// configBootstrap lists the config-only keys of the loaded hub config for
// the effective configuration view.
type configBootstrap struct {
	cfg     config.Hub
	origins config.Origins
}

func (b configBootstrap) BootstrapEntries() []settings.BootstrapEntry {
	entries := config.BootstrapEntries(b.cfg, b.origins)
	out := make([]settings.BootstrapEntry, 0, len(entries))

	for _, e := range entries {
		out = append(out, settings.BootstrapEntry{
			Key: e.Key, Value: e.Value, Set: e.Secret && string(e.Value) == "true",
			Origin: e.Origin.String(), Locked: e.Origin.Locked(), Secret: e.Secret,
		})
	}

	return out
}
