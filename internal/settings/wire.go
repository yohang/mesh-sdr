// Package settings wires the settings module (DB settings store, config
// locking and precedence, effective configuration) for the composition
// root (ADR 0010).
package settings

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/yohang/mesh-sdr/internal/shared/audit"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/settings/app"
	"github.com/yohang/mesh-sdr/internal/settings/infra/configsource"
	"github.com/yohang/mesh-sdr/internal/settings/infra/sqlite"
)

// Deps are the dependencies of the settings module.
type Deps struct {
	Config  config.Hub
	Origins config.Origins
	DB      *db.DB
	// Audit appends the audit records of settings writes (identity's
	// audit_log).
	Audit  audit.Appender
	Now    func() time.Time
	Logger *slog.Logger
}

// Module is the wired settings module.
type Module struct {
	Store     *app.Store
	Effective *app.EffectiveConfig
}

// Wire builds the module and loads the DB settings.
func Wire(ctx context.Context, d Deps) (*Module, error) {
	catalog, err := config.NewSettingsCatalog(d.Config, d.Origins)
	if err != nil {
		return nil, fmt.Errorf("settings catalog: %w", err)
	}

	store := app.NewStore(app.StoreDeps{
		Repo: sqlite.NewSettings(d.DB, config.SchemaVersion), Catalog: catalog, Tx: d.DB, Audit: d.Audit, Now: d.Now,
		Logger: d.Logger.With(slog.String("component", "settings.app.store")),
	})

	if err := store.Load(ctx); err != nil {
		return nil, fmt.Errorf("load settings: %w", err)
	}

	return &Module{
		Store:     store,
		Effective: app.NewEffectiveConfig(configsource.NewBootstrap(d.Config, d.Origins), store, d.Now),
	}, nil
}
