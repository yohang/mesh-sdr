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
	catalog, err := settings.NewConfigCatalog(cfg, origins)
	if err != nil {
		return nil, nil, fmt.Errorf("settings catalog: %w", err)
	}

	store := settings.NewStore(settings.StoreDeps{
		Repo: settings.NewSettings(adapter, config.SchemaVersion), Catalog: catalog, Tx: adapter, Audit: audit, Now: now,
		Logger: component(logger, "settings.store"),
	})

	if err := store.Load(ctx); err != nil {
		return nil, nil, fmt.Errorf("load settings: %w", err)
	}

	return store, settings.NewEffectiveConfig(cfg, origins, store, now), nil
}

// storeListenPolicy reads the raw global listen policy (listen_policy) from
// the settings store for grid/app.ListenPolicies, which validates it.
type storeListenPolicy struct {
	store interface{ String(key string) string }
}

// ListenPolicy implements grid/app.GlobalListenPolicy.
func (p storeListenPolicy) ListenPolicy(context.Context) (string, error) {
	return p.store.String("listen_policy"), nil
}
