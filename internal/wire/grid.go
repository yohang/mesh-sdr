package wire

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"slices"
	"time"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/events"
	"github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/grid/infra/control"
	"github.com/yohang/mesh-sdr/internal/grid/infra/enroll"
	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
	gridsqlite "github.com/yohang/mesh-sdr/internal/grid/infra/sqlite"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	"github.com/yohang/mesh-sdr/internal/settings"
	"github.com/yohang/mesh-sdr/internal/version"
)

// errGridDisabled is returned by grid features that need the hub CA.
var errGridDisabled = domain.ErrNodeUnavailable.WithDetail("the hub internal CA is not configured (tls.ca_cert): the grid is disabled")

// caInfo adapts the optional hub CA to app.CAInfo.
type caInfo struct{ ca *pki.CA }

func (c caInfo) Fingerprint() (string, error) {
	if c.ca == nil {
		return "", errGridDisabled
	}

	return pki.FormatFingerprint(c.ca.Fingerprint()), nil
}

// hubGrid is the hub side of the grid module.
type hubGrid struct {
	keys          *hubKeys
	caPath        string
	revocations   domain.RevocationRepository
	nodeRepo      domain.NodeRepository
	deviceRepo    domain.DeviceRepository
	gatewayClient *pki.CertSource
	ca            *pki.CA
	hubID         string
	nodes         *app.Nodes
	history       *app.History
	enrollment    *app.Enrollment
	tracker       *app.Tracker
	control       *app.Control
	status        *app.Status
	caps          *app.Capabilities
	devices       *app.Devices
	deviceLogs    *app.DeviceLogs
	presence      *app.Presence
	manager       *control.Manager
	// states pushes the desired state of the devices (ADR 0020), from
	// desired, filled by the scheduling modules; nil without the grid.
	states  *app.States
	desired *lazyDesired
	startup []func(ctx context.Context) error
	workers []func(ctx context.Context)
}

// HubID returns the hub id: the host of hub.url (ADR 0008 Q6).
func HubID(hubURL string) (string, error) {
	u, err := url.Parse(hubURL)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("hub.url %q has no host", hubURL)
	}

	return u.Hostname(), nil
}

// LoadCA reads the hub CA from the [tls] config, or returns nil when the
// grid is disabled.
func LoadCA(cfg config.HubTLS) (*pki.CA, error) {
	if cfg.CACert == "" {
		return nil, nil
	}

	certPEM, err := os.ReadFile(cfg.CACert)
	if err != nil {
		return nil, fmt.Errorf("tls.ca_cert: %w", err)
	}

	ca, err := pki.ParseCA(certPEM, []byte(cfg.CAKey.Reveal()))
	if err != nil {
		return nil, fmt.Errorf("tls.ca_cert / tls.ca_key: %w", err)
	}

	return ca, nil
}

// DeclaredNodes converts the [nodes.<id>] tables.
func DeclaredNodes(cfg map[string]config.ConfigNode) ([]app.DeclaredNode, error) {
	out := make([]app.DeclaredNode, 0, len(cfg))

	for _, id := range slices.Sorted(maps.Keys(cfg)) {
		c := cfg[id]

		nid, err := domain.NewNodeID(id)
		if err != nil {
			return nil, fmt.Errorf("nodes.%s: %w", id, err)
		}

		nameStr := c.Name
		if nameStr == "" {
			nameStr = id
		}

		name, err := domain.NewNodeName(nameStr)
		if err != nil {
			return nil, fmt.Errorf("nodes.%s.name: %w", id, err)
		}

		u, err := domain.NewNodeURL(c.URL)
		if err != nil {
			return nil, fmt.Errorf("nodes.%s.url: %w", id, err)
		}

		d := app.DeclaredNode{ID: nid, Name: name, URL: u}

		if c.EnrollmentToken.IsSet() {
			tok, err := domain.ParseEnrollmentToken(c.EnrollmentToken.Reveal())
			if err != nil {
				return nil, fmt.Errorf("nodes.%s.enrollment_token: %w", id, err)
			}

			d.Token = &tok
		}

		out = append(out, d)
	}

	return out, nil
}

func newHubGrid(cfg config.Hub, logger *slog.Logger, adapter *db.DB, now func() time.Time, timings app.Timings,
	tweaks ...func(*control.HubOptions),
) (*hubGrid, error) {
	ca, err := LoadCA(cfg.TLS)
	if err != nil {
		return nil, err
	}

	hubID, err := HubID(cfg.Hub.URL)
	if err != nil {
		return nil, err
	}

	declared, err := DeclaredNodes(cfg.Nodes)
	if err != nil {
		return nil, err
	}

	// The identity keyring is built after the grid; it is attached to keys
	// once identity is wired (newHub).
	keys := newHubKeys(now)

	audit := newAuditAppender(adapter, now)
	nodeRepo := gridsqlite.NewNodeRepository(adapter)
	revocations := gridsqlite.NewRevocationRepository(adapter)

	g := &hubGrid{
		keys:    keys,
		ca:      ca,
		hubID:   hubID,
		history: app.NewHistory(),
		nodes:   app.NewNodes(nodeRepo, revocations, adapter, audit, caInfo{ca: ca}, timings, now, component(logger, "grid.app.nodes")),
	}

	gridLogger := component(logger, "grid.wire")
	capRepo := gridsqlite.NewCapabilityRepository(adapter)
	connRepo := gridsqlite.NewConnectionRepository(adapter)
	deviceRepo := gridsqlite.NewDeviceRepository(adapter)
	g.nodeRepo, g.deviceRepo, g.revocations, g.caPath = nodeRepo, deviceRepo, revocations, cfg.TLS.CACert
	g.devices = app.NewDevices(deviceRepo, audit, component(logger, "grid.app.devices"))
	g.deviceLogs = app.NewDeviceLogs(deviceRepo, component(logger, "grid.app.devicelogs"))
	g.devices.OnForget(g.deviceLogs.Forget)

	if ca != nil {
		g.tracker = app.NewTracker()
		g.control = app.NewControl(nodeRepo, revocations, gridsqlite.NewCursorRepository(adapter), adapter, audit, g.tracker,
			version.String(), now, component(logger, "grid.app.control"))
		g.desired = &lazyDesired{}
		g.states = app.NewStates(g.desired, nil, g.tracker, component(logger, "grid.app.states"))
		hubOpts := control.HubOptions{
			HubID: hubID, CA: ca, Client: pki.NewClientSource(ca, pki.KindHub, hubID, now),
			Nodes: nodeRepo, Revocations: revocations, Control: g.control,
			HeartbeatInterval: timings.HeartbeatInterval, Now: now, Logger: component(logger, "grid.infra.control"),
			Keys: keys, Issuer: cfg.Hub.URL, State: g.states, Logs: g.deviceLogs,
		}
		for _, t := range tweaks {
			t(&hubOpts)
		}

		g.manager = control.NewManager(hubOpts)
		g.nodes.SetLinks(g.manager)
		g.states.SetSender(g.manager)

		g.status = app.NewStatus(nodeRepo, adapter, g.tracker, g.history, timings, now, component(logger, "grid.app.status"))
		g.control.Handle(rxv1.TypeNodeHeartbeat, g.status.HeartbeatHandler())
		g.control.OnLinkChange(g.status.Refresh)

		statesLogger := component(logger, "grid.app.states")
		g.control.OnLinkChange(func(ctx context.Context, id domain.NodeID) {
			// After an ingested batch that changed the node's devices
			// (MarkChanged below): heartbeats cost nothing.
			if err := g.states.PublishChanged(ctx, id); err != nil && ctx.Err() == nil {
				statesLogger.ErrorContext(ctx, "push desired state", slog.String("node_id", id.String()), slog.Any("error", err))
			}
		})
		g.workers = append(g.workers, g.status.Run)

		g.caps = app.NewCapabilities(capRepo, nodeRepo, g.manager, component(logger, "grid.app.capabilities"))
		g.control.Handle(rxv1.TypeNodeCapabilities, g.caps.Handler())
		g.caps.OnReport(g.devices.Sync)
		g.caps.OnReport(func(_ context.Context, n *domain.Node, _ ctl.Capabilities, _ time.Time) error {
			g.states.MarkChanged(n.ID())

			return nil
		})
		g.control.Handle(rxv1.TypeDeviceState, g.devices.StateHandler())
		g.status.Listen(g.devices.NodeStatusChanged)

		g.presence = app.NewPresence(connRepo, deviceRepo, g.tracker, timings, now, component(logger, "grid.app.presence"))
		for _, t := range []rxv1.MessageType{rxv1.TypeConnectionOpened, rxv1.TypeConnectionHeart, rxv1.TypeConnectionClosed} {
			g.control.Handle(t, g.presence.Handler())
		}

		g.control.OnBoot(g.presence.NodeRestarted)

		enrollment := app.NewEnrollment(nodeRepo, adapter, enroll.NewHubClient(ca, now), audit, now,
			5*time.Second, component(logger, "grid.app.enrollment"))
		enrollment.Enrolled = func(context.Context, domain.NodeID) { g.manager.Wake() }
		g.enrollment = enrollment
		g.workers = append(g.workers, enrollment.Run, g.manager.Run)
	} else {
		g.caps = app.NewCapabilities(capRepo, nodeRepo, nil, component(logger, "grid.app.capabilities"))
		g.presence = app.NewPresence(connRepo, deviceRepo, nil, timings, now, component(logger, "grid.app.presence"))
	}

	g.workers = append(g.workers, g.presence.Run)

	g.startup = append(g.startup, func(ctx context.Context) error {
		if ca == nil {
			gridLogger.WarnContext(ctx, "hub internal CA not configured (tls.ca_cert): node enrollment and control channels are disabled; run `meshsdr hub ca init`")
		} else {
			gridLogger.InfoContext(ctx, "hub internal CA loaded",
				slog.String("hub_id", hubID), slog.String("ca_fingerprint", pki.FormatFingerprint(ca.Fingerprint())))
		}

		if err := g.presence.CloseAtStart(ctx); err != nil {
			return err
		}

		return g.nodes.SyncConfig(ctx, declared)
	})

	return g, nil
}

// HubNodes builds the node registry service for the admin CLI.
func HubNodes(cfg config.Hub, logger *slog.Logger, adapter *db.DB) (*app.Nodes, error) {
	ca, err := LoadCA(cfg.TLS)
	if err != nil {
		return nil, err
	}

	audit := newAuditAppender(adapter, time.Now)

	return app.NewNodes(gridsqlite.NewNodeRepository(adapter), gridsqlite.NewRevocationRepository(adapter), adapter,
		audit, caInfo{ca: ca}, app.DefaultTimings(), time.Now, component(logger, "grid.app.nodes")), nil
}

// gridSettings reads the grid DB settings.
type gridSettings interface {
	Get(key string) (settings.Effective, bool)
	Int(key string) int
}

// applySettings applies the grid timings of the settings snapshot: a value
// set in the DB or the config replaces the base timing, a default keeps it
// (the defaults are the FEATURE_SPEC ones; tests run with faster bases).
func (g *hubGrid) applySettings(base app.Timings, s gridSettings) {
	t := base

	if e, ok := s.Get("grid.heartbeat_interval_s"); ok && e.Source() != settings.SourceDefault {
		t.HeartbeatInterval = time.Duration(s.Int("grid.heartbeat_interval_s")) * time.Second
	}

	if e, ok := s.Get("grid.offline_after_s"); ok && e.Source() != settings.SourceDefault {
		t.OfflineAfter = time.Duration(s.Int("grid.offline_after_s")) * time.Second
	}

	if g.status != nil {
		g.status.SetTimings(t)
	}

	if g.manager != nil {
		g.manager.SetHeartbeatInterval(t.HeartbeatInterval)
	}

	g.presence.SetTimings(t)
}

// publishEvents connects the grid producers of hub events: status
// transitions (after the device registry listener), committed node event
// batches, admin node changes, enrollments, forgotten devices and presence
// changes.
func (g *hubGrid) publishEvents(b *events.Broker, policies *app.ListenPolicies, reload func(context.Context),
	now func() time.Time, logger *slog.Logger,
) *gridEvents {
	ge := newGridEvents(b, g, policies, now, logger)

	if g.status != nil {
		g.status.Listen(ge.statusChanged)
	}

	if g.control != nil {
		// Policies first: the events of the batch use the new view.
		g.control.OnApplied(refreshOnDevices(reload))
		g.control.OnApplied(ge.applied)
	}

	if g.enrollment != nil {
		enrolled := g.enrollment.Enrolled
		g.enrollment.Enrolled = func(ctx context.Context, id domain.NodeID) {
			if enrolled != nil {
				enrolled(ctx, id)
			}

			ge.node(ctx, id)
		}
	}

	g.nodes.OnChange(ge.node)
	g.devices.OnForget(ge.forgotten)
	g.deviceLogs.OnRecords(ge.deviceLog)
	g.devices.OnForget(func(ctx context.Context, _ *domain.Device) { reload(ctx) })
	g.presence.OnChange(ge.presenceChanged)

	return ge
}

// links returns the node links of the features summary: the tracker, or
// nil when no node can connect (no hub CA).
func (g *hubGrid) links() app.NodeLinks {
	if g.tracker == nil {
		return nil
	}

	return g.tracker
}
