package wire

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/yohang/mesh-sdr/internal/bookmarks"
	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/decodes"
	"github.com/yohang/mesh-sdr/internal/events"
	"github.com/yohang/mesh-sdr/internal/files"
	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	gridhttp "github.com/yohang/mesh-sdr/internal/grid/http"
	"github.com/yohang/mesh-sdr/internal/grid/infra/control"
	"github.com/yohang/mesh-sdr/internal/grid/infra/gateway"
	httpserver "github.com/yohang/mesh-sdr/internal/http"
	"github.com/yohang/mesh-sdr/internal/http/api"
	"github.com/yohang/mesh-sdr/internal/http/clientip"
	"github.com/yohang/mesh-sdr/internal/identity"
	identitydomain "github.com/yohang/mesh-sdr/internal/identity/domain"
	identitysqlite "github.com/yohang/mesh-sdr/internal/identity/infra/sqlite"
	"github.com/yohang/mesh-sdr/internal/jobs"
	"github.com/yohang/mesh-sdr/internal/presets"
	"github.com/yohang/mesh-sdr/internal/schedules"
	"github.com/yohang/mesh-sdr/internal/settings"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
	"github.com/yohang/mesh-sdr/internal/shell"
	"github.com/yohang/mesh-sdr/internal/web/layout"
)

// hubModules are the parts of the hub its builders share.
type hubModules struct {
	cfg     config.Hub
	adapter *db.DB
	logger  *slog.Logger
	now     func() time.Time

	g        *hubGrid
	settings *settings.Store
	broker   *events.Broker
	// listen decides who may listen to what (the listen policies): one
	// source for the media authz, the desired state, the events, files,
	// decodes, bookmarks and the feature summary.
	listen   *gridapp.ListenPolicies
	files    *fileAccess
	images   *files.Branding
	shell    shell.Wired
	idm      *identity.Module
	sch      *scheduling
	features *gridapp.Features

	// The gates open to the visitors holding a role once identity is
	// wired (the shell is built before it).
	adminGate, operatorGate, listenerGate *roleGate

	workers []func(context.Context)
}

func newHub(ctx context.Context, cfg config.Hub, origins config.Origins, logger *slog.Logger, adapter *db.DB,
	now func() time.Time, timings gridapp.Timings, tweaks ...func(*control.HubOptions),
) (*Process, *hubGrid, error) {
	g, err := newHubGrid(cfg, logger, adapter, now, timings, tweaks...)
	if err != nil {
		return nil, nil, err
	}

	settingsStore, effective, err := newSettings(ctx, cfg, origins, adapter, g.audit, now, logger)
	if err != nil {
		return nil, nil, fmt.Errorf("settings: %w", err)
	}

	// grid.heartbeat_interval_s and grid.offline_after_s apply live.
	g.applySettings(timings, settingsStore.Snapshot())
	settingsStore.Subscribe(func(s *settings.Snapshot) { g.applySettings(timings, s) })

	h := &hubModules{
		cfg: cfg, adapter: adapter, logger: logger, now: now, g: g, settings: settingsStore, broker: events.NewBroker(),
		images:    branding(adapter, g.audit),
		adminGate: &roleGate{role: identitydomain.RoleAdmin}, operatorGate: &roleGate{role: identitydomain.RoleOperator},
		listenerGate: &roleGate{role: identitydomain.RoleListener},
	}

	h.listen = gridapp.NewListenPolicies(g.deviceRepo, storeListenPolicy{store: settingsStore}, component(logger, "grid.app.listen"))
	h.files = &fileAccess{signedIn: h.listenerGate.Allows, policies: h.listen, logger: component(logger, "wire.files")}
	h.newShell()

	if err := h.newIdentity(ctx); err != nil {
		return nil, nil, err
	}

	// Presets and schedules (ADR 0020), wired to the grid.
	h.sch = newScheduling(adapter, g, settingsStore, h.listen, g.audit, now, logger)

	if g.states != nil {
		// The desired state carries listen_policy: a settings change is
		// pushed to the nodes, coalesced.
		settingsStore.Subscribe(func(*settings.Snapshot) { g.states.Changed() })
		h.workers = append(h.workers, g.states.Run)
	}

	h.features = gridapp.NewFeatures(gridapp.FeaturesDeps{
		Devices: g.deviceRepo, Caps: g.capRepo, Listen: h.listen, Links: g.links(), Nodes: g.nodeRepo, Listeners: g.presence,
		Telemetry: g.history, PresetName: h.sch.presetName,
	})

	eventsModule := h.newEvents()
	gallery, filesRetention, filesPolicy := h.newFiles()

	// Decoded messages (DEC-047): stored from the control channels, shown
	// on the Decodes page.
	decoded := newDecodes(decodesDeps{
		adapter: adapter, grid: g, broker: h.broker, identity: h.idm.HTTP, features: h.features, store: settingsStore,
		render: h.shell.Renderer, now: now, logger: logger,
	})

	scheduler, retention, err := newJobs(adapter, g.connRepo, h.idm, h.sch, decoded, settingsStore, g.audit, filesRetention, filesPolicy, logger)
	if err != nil {
		return nil, nil, fmt.Errorf("jobs: %w", err)
	}

	bm, err := newBookmarks(bookmarksDeps{
		adapter: adapter, features: h.features, presets: h.sch.presets,
		region: func() string { return settingsStore.String("bandplan.region") }, audit: g.audit, broker: h.broker,
		policies: h.listen, idm: h.idm.HTTP, render: h.shell.Renderer, isAdmin: h.adminGate.Allows, now: now, logger: logger,
	})
	if err != nil {
		return nil, nil, err
	}

	router, err := h.newRouter(hubPages{
		effective: effective, retention: retention, gallery: gallery, bookmarks: bm, events: eventsModule, decodes: decoded,
	})
	if err != nil {
		return nil, nil, err
	}

	setupURL, err := h.idm.Setup.Begin(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("identity setup: %w", err)
	}

	front, err := newGateway(cfg, logger, router, g)
	if err != nil {
		return nil, nil, err
	}

	return &Process{
		addr:     gatewayAddr(cfg.Gateway),
		server:   httpserver.NewServer("", router),
		front:    front,
		logger:   component(logger, "http.server"),
		startup:  g.startup,
		workers:  append(append(h.workers, scheduler.Run), g.workers...),
		setupURL: setupURL,
	}, g, nil
}

// newShell builds the app shell. The top bar shows the signed-in user,
// which the identity module, built after the shell (it renders its pages
// with the shell), fills in.
func (h *hubModules) newShell() {
	userOf := func(r *http.Request) *layout.User {
		if h.idm == nil {
			return nil
		}

		return h.idm.HTTP.ShellUser(r)
	}

	// Who sees the files the nodes sent follows the listen policies.
	filesGate := shell.GateFunc(func(ctx context.Context) bool { return !h.files.visibility(ctx).Denied() })

	h.shell = shell.New(shell.Deps{
		Settings: h.settings, AdminGate: h.adminGate, FilesGate: filesGate, User: operatorLinks(userOf, h.operatorGate.Allows),
		Logger:    h.logger,
		Bookmarks: &shell.BookmarksLink{Gate: h.operatorGate, Path: bookmarks.ManagePath},
		Images:    stationImages{b: h.images, logger: component(h.logger, "wire.station_images")},
	})
}

// newIdentity wires the identity module to the grid (ACC-007,
// GRID-011/012): revoked sessions and users go to the nodes and end their
// events sockets at once (ADR 0016 decision 4), and POST /auth/token
// refreshes only connections the gateway authz issued to the caller.
func (h *hubModules) newIdentity(ctx context.Context) error {
	g := h.g

	ideps := IdentityDeps(h.cfg, h.logger, h.adapter)
	ideps.Settings = h.settings
	ideps.Devices = gridDevices{g.devices}
	ideps.Binder = connectionBinder{repo: g.connRepo}

	rev := revocations{broker: h.broker, now: h.now}
	if g.manager != nil {
		rev.nodes = g.manager
	}

	ideps.Revocations = rev

	if q := mailQueue(ctx, h.cfg.SMTP, h.logger); q != nil {
		ideps.Mail = q
		h.workers = append(h.workers, q.Run)
	}

	idm, err := identity.Wire(ctx, ideps, pages{h.shell.Renderer})
	if err != nil {
		return fmt.Errorf("identity: %w", err)
	}

	g.keys.attach(idm.Keys)

	h.workers = append(h.workers, func(ctx context.Context) {
		idm.RunKeyMaintenance(ctx, h.now, component(h.logger, "identity.infra.keyring"))
	})

	h.adminGate.authz, h.operatorGate.authz, h.listenerGate.authz = idm.HTTP, idm.HTTP, idm.HTTP
	h.idm = idm

	return nil
}

// newEvents builds the hub events WebSocket (ADR 0016, ADR 0018) and
// connects its grid producers.
func (h *hubModules) newEvents() *events.Module {
	reload := reloadListen(h.listen, h.broker, component(h.logger, "wire.events"))
	m := newEventsModule(h.cfg.Hub.URL, h.broker, h.idm.HTTP, h.listen, h.g.presence, h.now, h.logger)
	h.settings.Subscribe(func(*settings.Snapshot) { reload(context.Background()) })
	ge := h.g.publishEvents(h.broker, h.listen, reload, h.now, h.logger)
	h.workers = append(h.workers, m.Run, ge.runPresence)

	return m
}

// newFiles builds the files the nodes send (FIL-005): their ingest from the
// control channels, their retention and the Files pages.
func (h *hubModules) newFiles() (*files.Gallery, *files.Retention, func() files.RetentionPolicy) {
	store := h.settings
	repo := files.NewFiles(h.adapter)
	policy := func() files.RetentionPolicy {
		return files.RetentionPolicy{
			Count: store.Int("files.retention_count"), MaxAge: time.Duration(store.Int("files.retention_days")) * 24 * time.Hour,
			MaxBytes: int64(store.Int("files.max_total_bytes")),
		}
	}
	retention := files.NewRetention(h.adapter, policy, h.now)

	if h.g.control != nil {
		fileEvents{
			ingest: files.NewIngest(files.IngestDeps{
				Repo: repo, Tx: h.adapter, Processor: files.NewProcessor(), Retention: retention,
				Published: h.files.published(h.broker), Logger: component(h.logger, "files.app.ingest"),
			}),
			devices: h.g.devices, logger: component(h.logger, "wire.files"),
		}.register(h.g.control)
	}

	gallery := files.NewGallery(files.GalleryDeps{
		Repo: repo, Tx: h.adapter, Audit: h.g.audit, Render: h.shell.Renderer, Visibility: h.files.visibility,
		Listener: h.idm.HTTP.Require(identitydomain.RoleListener), Operator: h.idm.HTTP.Require(identitydomain.RoleOperator),
		Admin: h.idm.HTTP.Require(identitydomain.RoleAdmin), CanDelete: h.operatorGate.Allows, CanBulkDelete: h.adminGate.Allows,
		Policy: policy, DeviceNames: deviceNames(h.g.deviceRepo),
		Logger: component(h.logger, "files.http.gallery"),
	})

	return gallery, retention, policy
}

// hubPages are the modules the router serves besides those of
// hubModules.
type hubPages struct {
	effective *settings.EffectiveConfig
	retention *jobs.Retention
	gallery   *files.Gallery
	bookmarks *bookmarks.Module
	events    *events.Module
	decodes   *decodes.Module
}

// newAPI builds the /api/v1 handler, and the handlers of the status alias.
func (h *hubModules) newAPI(p hubPages) (http.Handler, api.StatusHandlers, error) {
	status := api.NewStatusHandlers(stationStatus{settings: h.settings, features: h.features, presets: h.sch.presets},
		component(h.logger, "http.api.status"))

	handler, err := api.NewHandler(api.Server{
		StatusHandlers:   status,
		HealthHandlers:   api.NewHealthHandlers(h.adapter, component(h.logger, "http.api.health")),
		AuthHandlers:     api.NewAuthHandlers(h.idm.HTTP),
		ConfigHandlers:   api.NewConfigHandlers(p.effective),
		BrandingHandlers: api.NewBrandingHandlers(h.images),
		TokenHandlers:    api.NewTokenHandlers(h.idm.HTTP, h.idm.HTTP, h.idm.Tokens),
		FeatureHandlers:  api.NewFeatureHandlers(h.idm.HTTP, h.features),
		BookmarkHandlers: p.bookmarks,
		FileHandlers:     api.NewFileHandlers(p.gallery),
	}, h.idm.HTTP, h.cfg.Gateway.MaxBody.Bytes(), component(h.logger, "http.api"))
	if err != nil {
		return nil, api.StatusHandlers{}, fmt.Errorf("api: %w", err)
	}

	return handler, status, nil
}

// newRouter builds the hub router: the API and every module.
func (h *hubModules) newRouter(p hubPages) (http.Handler, error) {
	apiHandler, status, err := h.newAPI(p)
	if err != nil {
		return nil, err
	}

	g, idm, rd := h.g, h.idm.HTTP, h.shell.Renderer
	admin, operator := idm.Require(identitydomain.RoleAdmin), idm.Require(identitydomain.RoleOperator)

	access, err := g.mediaAccess(h.cfg, h.listen, h.logger)
	if err != nil {
		return nil, err
	}

	authz := gridhttp.NewAuthzHandler(access, func(r *http.Request) gridapp.Subject { return subjectOf(idm.Principal(r.Context())) },
		func(r *http.Request) string { return clientip.From(r.Context()).String() }, component(h.logger, "grid.http.authz"))

	imagesHTTP := files.New(h.images, admin, currentUser, rd.Error, component(h.logger, "files.http"))
	schedulesView := deviceSchedules{schedules: h.sch.schedules, presets: h.sch.presets}

	return httpserver.NewRouter(
		component(h.logger, "http.router"),
		h.cfg.Hub.URL,
		apiHandler,
		// Every request body, API and forms, is capped (gateway.max_body)
		// before any module reads it.
		bodyLimit(h.cfg.Gateway.MaxBody.Bytes()),
		idm,
		settings.New(settings.Deps{
			Render: rd, Guard: admin, Store: h.settings, Config: p.effective, Retention: retentionRows{r: p.retention},
			User: currentUser, Images: imagesHTTP, Schedules: schedulesView, Logger: component(h.logger, "settings.http"),
		}),
		imagesHTTP,
		presets.NewPages(presets.PagesDeps{
			Render: rd, Guard: admin, Service: h.sch.presets, Logger: component(h.logger, "presets.http"),
		}),
		p.bookmarks,
		p.gallery,
		schedules.NewPages(schedules.PagesDeps{
			Render: rd, Guard: admin, Service: h.sch.schedules, PresetName: h.sch.presetName,
			Logger: component(h.logger, "schedules.http"),
		}),
		gridhttp.NewAdminModule(gridhttp.AdminDeps{
			Render: rd, Devices: g.devices, Nodes: g.nodes, History: g.history, Capabilities: g.caps,
			Connections: g.presence, Users: userNames{users: identitysqlite.NewUsers(h.adapter, shared.NewUUIDv7Generator())},
			Schedules: schedulesView, PresetName: h.sch.presetName, PresetBand: h.sch.presetBand, Audit: g.audit, Logs: g.deviceLogs,
			BandAt: func(_ context.Context, hz int64) string {
				// Like the receiver's Info tab: every band holding hz.
				var names []string
				for _, b := range p.bookmarks.Bandplan().Bands(hz, hz) {
					names = append(names, b.Name)
				}

				return strings.Join(names, ", ")
			},
			MaskIPs:  func(context.Context) bool { return h.settings.Bool("privacy.mask_ips") },
			Operator: operator, Admin: admin,
			IsAdmin: func(r *http.Request) bool { return h.adminGate.Allows(r.Context()) }, Now: h.now,
			Logger: component(h.logger, "grid.http.admin"),
		}),
		routes(func(r chi.Router) {
			r.Method(http.MethodGet, gateway.AuthzPath, authz)
			// Alias of GET /api/v1/status for receiver directory sites.
			r.Method(http.MethodGet, "/status.json", status.Alias())
		}),
		p.events,
		p.decodes,
		h.shell.HTTP,
	), nil
}
