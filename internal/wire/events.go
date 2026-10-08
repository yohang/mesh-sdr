package wire

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/events"
	gridapp "github.com/yohang/mesh-sdr/internal/grid/app"
	griddomain "github.com/yohang/mesh-sdr/internal/grid/domain"
	gridhttp "github.com/yohang/mesh-sdr/internal/grid/http"
	"github.com/yohang/mesh-sdr/internal/http/clientip"
	identityapp "github.com/yohang/mesh-sdr/internal/identity/app"
	identitydomain "github.com/yohang/mesh-sdr/internal/identity/domain"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/settings"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
	"github.com/yohang/mesh-sdr/internal/version"
)

// The hub events WebSocket (ADR 0016, ADR 0018): the /api/ws module, its
// topic authorisation and session adapters, and the producers that turn
// grid changes into hub events. Producers publish after commit and after
// the derived state is updated.

// nodeHeartbeatEvery throttles the node.status events of heartbeats: one
// per node and period (ADR 0018), on top of status transitions.
const nodeHeartbeatEvery = 30 * time.Second

// presenceDebounce coalesces presence changes into one presence.count.
const presenceDebounce = time.Second

// eventsIdentity is the identity surface the events adapters need.
type eventsIdentity interface {
	Authorize(ctx context.Context, role identitydomain.Role) error
	Principal(ctx context.Context) identitydomain.Principal
	SessionRef(ctx context.Context) string
	CheckSession(ctx context.Context, r *http.Request) (time.Time, error)
}

// policySnapshot is one consistent view of who may listen to what: the
// effective listen policy of every enabled device (ADR 0018).
type policySnapshot struct {
	devices map[string]string
}

// anyAnonymous reports whether some device is anonymous-listenable.
func (p policySnapshot) anyAnonymous() bool {
	for _, lp := range p.devices {
		if lp == gridapp.ListenAnonymous {
			return true
		}
	}

	return false
}

// viewerCanListen reports whether a viewer may listen to a device: any
// enabled device for a signed-in user, the anonymous-listenable ones for a
// visitor.
func (p policySnapshot) viewerCanListen(v events.Viewer, device string) bool {
	policy, ok := p.devices[device]

	return ok && (!v.Anonymous() || policy == gridapp.ListenAnonymous)
}

// canListen reports whether the principal may listen to a device.
func (p policySnapshot) canListen(pr identitydomain.Principal, device string) bool {
	policy, ok := p.devices[device]
	if !ok {
		return false
	}

	lp, err := identitydomain.ParseListenPolicy(policy)

	return err == nil && pr.CanListen(lp)
}

// policyCache holds the current policy snapshot. Topic checks read it
// without touching the database; it is reloaded when the listen policies
// may have changed (a listen_policy setting change, a device report, a
// forgotten device), and the sockets re-authorise their topics only when
// the snapshot actually changed.
type policyCache struct {
	policies *gridapp.ListenPolicies
	broker   *events.Broker
	logger   *slog.Logger

	mu   sync.Mutex
	snap *policySnapshot
}

// get returns the snapshot, loading it on first use.
func (c *policyCache) get(ctx context.Context) (policySnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.snap != nil {
		return *c.snap, nil
	}

	devices, err := c.policies.Effective(ctx)
	if err != nil {
		return policySnapshot{}, err
	}

	c.snap = &policySnapshot{devices: devices}

	return *c.snap, nil
}

// refresh reloads the snapshot; when it changed, every socket re-authorises
// its topics against the new one.
func (c *policyCache) refresh(ctx context.Context) {
	devices, err := c.policies.Effective(ctx)
	if err != nil {
		c.logger.ErrorContext(ctx, "reload listen policies", slog.Any("error", err))

		return
	}

	c.mu.Lock()
	changed := c.snap == nil || !maps.Equal(c.snap.devices, devices)
	c.snap = &policySnapshot{devices: devices}
	c.mu.Unlock()

	if changed {
		c.broker.RecheckAll()
	}
}

// topicAuthz decides topic access (§6.6 "Topic access", ADR 0016 decision
// 7): admin topics, the device logs among them, need admin (and
// admin.allowed_networks); per-device
// topics need listen permission on the device (its effective listen
// policy); the other topics are open to signed-in users, and to anonymous
// visitors when some device is anonymous-listenable (public_map does not
// exist yet).
type topicAuthz struct {
	id       eventsIdentity
	policies *policyCache
}

func (a topicAuthz) AuthorizeTopic(ctx context.Context, t events.Topic) error {
	p := a.id.Principal(ctx)

	if t.IsAdmin() {
		if a.id.Authorize(ctx, identitydomain.RoleAdmin) != nil {
			return events.ErrTopicForbidden
		}

		return nil
	}

	if t.Device() == "" && !p.IsAnonymous() {
		return nil
	}

	snap, err := a.policies.get(ctx)
	if err != nil {
		return err
	}

	switch {
	case t.Device() != "" && !snap.canListen(p, t.Device()):
		return events.ErrTopicForbidden
	case t.Device() == "" && !snap.anyAnonymous():
		return events.ErrTopicForbidden
	}

	return nil
}

// eventsSession adapts identity to the events WS session port.
type eventsSession struct{ id eventsIdentity }

func (s eventsSession) Identify(ctx context.Context) events.Identity {
	p := s.id.Principal(ctx)
	if p.IsAnonymous() {
		return events.Identity{Roles: []string{}}
	}

	name := p.Username().String()
	if d := p.DisplayName(); !d.IsZero() {
		name = d.String()
	}

	roles := make([]string, 0, 3)
	for _, r := range p.Roles() {
		roles = append(roles, r.String())
	}

	user, _ := shared.UUIDFromBytes(p.UserID().Bytes())
	session, _ := shared.ParseUUID(p.SessionID().String())

	return events.Identity{
		Viewer: events.Viewer{UserID: p.UserID().String(), SessionRef: s.id.SessionRef(ctx), Staff: p.Has(identitydomain.RoleOperator),
			Admin: p.Has(identitydomain.RoleAdmin),
		},
		UserID: user, SessionID: session, Name: name, Roles: roles, RoleRank: int(p.Role().ID()),
	}
}

func (s eventsSession) Check(ctx context.Context, r *http.Request) (time.Time, error) {
	until, err := s.id.CheckSession(ctx, r)
	if errors.Is(err, identitydomain.ErrUnauthenticated) {
		return time.Time{}, events.ErrUnauthenticated
	}

	return until, err
}

// eventsPresence records the events sockets in the grid presence registry.
type eventsPresence struct{ p *gridapp.Presence }

func (e eventsPresence) Open(ctx context.Context, c events.Connection) error {
	return e.p.Open(ctx, griddomain.ConnectionInfo{
		ID: c.ID, Kind: griddomain.ConnectionEvents, UserID: c.UserID, SessionID: c.SessionID, RoleID: c.RoleRank,
		IP: c.IP, UserAgent: c.UserAgent,
	})
}

func (e eventsPresence) Heartbeat(ctx context.Context, ids []shared.UUID) error {
	return e.p.Heartbeat(ctx, ids)
}

func (e eventsPresence) Attach(ctx context.Context, id shared.UUID, device string) error {
	return e.p.Attach(ctx, id, device)
}

func (e eventsPresence) Close(ctx context.Context, id shared.UUID, reason events.CloseReason) error {
	return e.p.Close(ctx, id, griddomain.ParseCloseReason(string(reason)))
}

// brokerRevocations ends the events sockets of revoked sessions and users
// (ADR 0016 decision 4).
type brokerRevocations struct{ b *events.Broker }

// PublishRevocation implements identityapp.RevocationPublisher.
func (r brokerRevocations) PublishRevocation(_ context.Context, rv identityapp.Revocation) {
	users := make([]string, 0, len(rv.Users))
	for _, u := range rv.Users {
		users = append(users, u.String())
	}

	r.b.EndSessions(rv.Sessions, users)
}

// revocationFanout tells every publisher (nodes, events sockets).
type revocationFanout []identityapp.RevocationPublisher

// PublishRevocation implements identityapp.RevocationPublisher.
func (f revocationFanout) PublishRevocation(ctx context.Context, rv identityapp.Revocation) {
	for _, p := range f {
		p.PublishRevocation(ctx, rv)
	}
}

// hubOrigin returns scheme://host of hub.url.
func hubOrigin(hubURL string) string {
	u, err := url.Parse(hubURL)
	if err != nil || u.Host == "" {
		return ""
	}

	return u.Scheme + "://" + u.Host
}

// newEventsModule builds the /api/ws module.
func newEventsModule(hubURL string, b *events.Broker, id eventsIdentity, policies *policyCache,
	presence *gridapp.Presence, now func() time.Time, logger *slog.Logger,
) *events.Module {
	return events.New(events.Deps{
		Broker: b, Authz: topicAuthz{id: id, policies: policies}, Session: eventsSession{id: id},
		Presence: eventsPresence{p: presence}, Admission: events.NewAdmission(events.DefaultLimits()),
		ClientIP: func(r *http.Request) string { return clientip.From(r.Context()).String() },
		Origin:   hubOrigin(hubURL), Version: version.String(), Now: now,
		Logger: component(logger, "events.http.ws"),
	})
}

// listenPolicyWatch reloads the listen policies when the global listen
// policy changes (a settings Store.Subscribe callback).
func listenPolicyWatch(c *policyCache, initial string) func(*settings.Snapshot) {
	var (
		mu   sync.Mutex
		last = initial
	)

	return func(s *settings.Snapshot) {
		v := s.String("listen_policy")

		mu.Lock()
		changed := v != last
		last = v
		mu.Unlock()

		if changed {
			c.refresh(context.Background())
		}
	}
}

// nodeStatusEvent is the node.status payload: thin (ADR 0016 decision 2),
// the client refetches the fragment that renders what the viewer may see.
// Listeners and the telemetry are the public subset of §6.6 (listeners,
// cpu, temp_c), set for an up node only: a listener's receiver shows
// them in its Info tab, and learns from status whether its node is up
// (GRID-021).
type nodeStatusEvent struct {
	NodeID    string   `json:"node_id"`
	Status    string   `json:"status"`
	Listeners *int     `json:"listeners,omitempty"`
	CPU       *float64 `json:"cpu,omitempty"`
	TempC     *float64 `json:"temp_c,omitempty"`
}

// deviceStatusEvent is the device.status payload (§6.6): the device state,
// its listeners and its active preset and centre (UI-021 live picker).
type deviceStatusEvent struct {
	DeviceID       string `json:"device_id"`
	NodeID         string `json:"node_id"`
	State          string `json:"state"`
	Listeners      int    `json:"listeners"`
	ActivePresetID string `json:"active_preset_id,omitempty"`
	CenterHz       int64  `json:"center_hz,omitempty"`
}

// presenceCountEvent is the presence.count payload: the listeners, open
// media connections (ADR 0018); by_device comes with per-recipient
// filtering (PRS-002).
type presenceCountEvent struct {
	Total int `json:"total"`
}

var (
	topicNodes    = events.MustTopic("nodes")
	topicDevices  = events.MustTopic("devices")
	topicPresence = events.MustTopic("presence")
)

// gridEvents turns grid changes into hub events.
type gridEvents struct {
	b       events.Publisher
	nodes   griddomain.NodeRepository
	devices *gridapp.Devices
	now     func() time.Time
	logger  *slog.Logger

	mu        sync.Mutex
	lastBeat  map[griddomain.NodeID]time.Time
	presence  chan struct{}
	policies  *policyCache
	listeners listenerCounter
	history   interface {
		Latest(id griddomain.NodeID) (gridapp.LoadSample, bool)
	}
}

// listenerCounter counts the listeners (grid Presence).
type listenerCounter interface {
	Listeners(ctx context.Context) (int, error)
	ListenersByDevice(ctx context.Context) (map[string]int, error)
	NodeListeners(ctx context.Context, id griddomain.NodeID) (int, error)
}

func newGridEvents(b events.Publisher, g *hubGrid, policies *policyCache, now func() time.Time, logger *slog.Logger) *gridEvents {
	return &gridEvents{
		b: b, nodes: g.nodeRepo, devices: g.devices, policies: policies, now: now, logger: component(logger, "events.wire.grid"),
		lastBeat: map[griddomain.NodeID]time.Time{}, presence: make(chan struct{}, 1), listeners: g.presence, history: g.history,
	}
}

// node publishes the current health of a node; a deleted node is "removed".
func (e *gridEvents) node(ctx context.Context, id griddomain.NodeID) {
	status := "removed"
	up := false

	n, err := e.nodes.Get(ctx, id)

	switch {
	case err == nil:
		status, up = n.Health(), n.Up()
	case !errors.Is(err, griddomain.ErrNodeNotFound):
		e.logger.ErrorContext(ctx, "read node for node.status", slog.String("node_id", id.String()), slog.Any("error", err))

		return
	}

	e.publishNode(ctx, id, status, up)
}

// staffOnly are the node states only operators and admins see: they tell
// about the registry, not about something a listener can use.
var staffOnly = []string{"enrolling", "revoked", "removed"}

// publishNode publishes node.status to the viewers who may know the node:
// operators and admins, and the viewers who may listen to one of its
// devices (ADR 0018).
func (e *gridEvents) publishNode(ctx context.Context, id griddomain.NodeID, status string, up bool) {
	audience := staff
	payload := nodeStatusEvent{NodeID: id.String(), Status: status}

	if !slices.Contains(staffOnly, status) {
		snap, err := e.policies.get(ctx)
		if err != nil {
			e.logger.ErrorContext(ctx, "listen policies for node.status", slog.Any("error", err))
		}

		devices, derr := e.devices.ListByNode(ctx, id)
		if derr != nil {
			e.logger.ErrorContext(ctx, "list devices for node.status", slog.String("node_id", id.String()), slog.Any("error", derr))
		}

		if err == nil && derr == nil {
			ids := make([]string, 0, len(devices))
			for _, d := range devices {
				ids = append(ids, d.ID().String())
			}

			audience = func(v events.Viewer) bool {
				if v.Staff {
					return true
				}

				for _, d := range ids {
					if snap.viewerCanListen(v, d) {
						return true
					}
				}

				return false
			}
		}
	}

	if up {
		e.telemetry(ctx, id, &payload)
	}

	e.b.Publish(ctx, events.Event{Topic: topicNodes, Type: rxv1.TypeNodeStatus.String(), Audience: audience, Payload: payload})
}

// telemetry adds the node's listeners and last heartbeat values to p.
func (e *gridEvents) telemetry(ctx context.Context, id griddomain.NodeID, p *nodeStatusEvent) {
	if e.listeners != nil {
		n, err := e.listeners.NodeListeners(ctx, id)
		if err != nil {
			e.logger.ErrorContext(ctx, "count node listeners for node.status", slog.String("node_id", id.String()), slog.Any("error", err))
		} else {
			p.Listeners = &n
		}
	}

	if e.history != nil {
		if s, ok := e.history.Latest(id); ok {
			cpu := s.CPU
			p.CPU, p.TempC = &cpu, s.TempC
		}
	}
}

// admins accepts admins only.
func admins(v events.Viewer) bool { return v.Admin }

// staff accepts operators and admins only.
func staff(v events.Viewer) bool { return v.Staff }

// statusChanged is a grid StatusListener, registered after the device
// registry's: devices are already marked offline when the events go out.
func (e *gridEvents) statusChanged(ctx context.Context, id griddomain.NodeID, status griddomain.Status, _ string) {
	e.mu.Lock()
	e.lastBeat[id] = e.now()
	e.mu.Unlock()

	e.publishNode(ctx, id, string(status), status == griddomain.StatusOnline || status == griddomain.StatusDegraded)
	e.nodeDevices(ctx, id)
}

// applied handles a committed batch of node events (Control.OnApplied).
func (e *gridEvents) applied(ctx context.Context, id griddomain.NodeID, types []rxv1.MessageType) {
	switch {
	case slices.Contains(types, rxv1.TypeNodeCapabilities):
		e.node(ctx, id)
	case slices.Contains(types, rxv1.TypeNodeHeartbeat):
		now := e.now()

		e.mu.Lock()
		due := now.Sub(e.lastBeat[id]) >= nodeHeartbeatEvery
		if due {
			e.lastBeat[id] = now
		}
		e.mu.Unlock()

		if due {
			e.node(ctx, id)
		}
	}

	if slices.Contains(types, rxv1.TypeDeviceState) || slices.Contains(types, rxv1.TypeNodeCapabilities) {
		e.nodeDevices(ctx, id)
	}

	// A heartbeat may attach a connection to another device: the listener
	// counts by device are read again (published only when they changed).
	if slices.Contains(types, rxv1.TypeConnectionOpened) || slices.Contains(types, rxv1.TypeConnectionClosed) ||
		slices.Contains(types, rxv1.TypeConnectionHeart) {
		e.presenceChanged(ctx)
	}
}

// nodeDevices publishes the state of every device of a node.
func (e *gridEvents) nodeDevices(ctx context.Context, id griddomain.NodeID) {
	devices, err := e.devices.ListByNode(ctx, id)
	if err != nil {
		e.logger.ErrorContext(ctx, "list devices for device.status", slog.String("node_id", id.String()), slog.Any("error", err))

		return
	}

	e.publishDevices(ctx, devices, nil)
}

// publishDevices publishes device.status for devices; counts are the
// listeners by device, read when nil.
func (e *gridEvents) publishDevices(ctx context.Context, devices []*griddomain.Device, counts map[string]int) {
	snap, err := e.policies.get(ctx)
	if err != nil {
		e.logger.ErrorContext(ctx, "listen policies for device.status", slog.Any("error", err))

		return
	}

	if counts == nil && e.listeners != nil {
		if counts, err = e.listeners.ListenersByDevice(ctx); err != nil {
			e.logger.ErrorContext(ctx, "count listeners for device.status", slog.Any("error", err))
		}
	}

	for _, d := range devices {
		st, _, _ := d.State()
		device := d.ID().String()
		payload := deviceStatusEvent{DeviceID: device, NodeID: d.Node().String(), State: string(st), Listeners: counts[device]}

		if p := d.ActivePreset(); !p.IsZero() {
			payload.ActivePresetID = p.String()
		}

		if c := d.CenterFreq(); c != nil {
			payload.CenterHz = *c
		}

		// Operators and admins see every device, the others the devices
		// they may listen to (ADR 0018).
		e.b.Publish(ctx, events.Event{
			Topic: topicDevices, Type: rxv1.TypeDeviceStatus.String(),
			Audience: func(v events.Viewer) bool { return v.Staff || snap.viewerCanListen(v, device) },
			Payload:  payload,
		})
	}
}

// deviceLogEvent is the device.log payload of /api/ws (SRC-005): plain
// text records, inserted with textContent by the page.
type deviceLogEvent struct {
	DeviceID string           `json:"device_id"`
	Reset    bool             `json:"reset,omitempty"`
	Records  []deviceLogEntry `json:"records"`
}

type deviceLogEntry struct {
	// T is the record time in Unix milliseconds, Time its display form.
	T      int64  `json:"t"`
	Time   string `json:"time"`
	Source string `json:"source"`
	Class  string `json:"class,omitempty"`
	Text   string `json:"text"`
}

// deviceLog relays device log records to the admins who subscribed to the
// log of the device (admin.device_log:device=<id>).
func (e *gridEvents) deviceLog(ctx context.Context, device shared.DeviceID, reset bool, records []gridapp.LogRecord) {
	topic, err := events.ParseTopic(string(events.KindDeviceLog) + ":device=" + device.String())
	if err != nil {
		e.logger.WarnContext(ctx, "device log topic", slog.String("device_id", device.String()), slog.Any("error", err))

		return
	}

	entries := make([]deviceLogEntry, 0, len(records))
	for _, r := range records {
		entries = append(entries, deviceLogEntry{T: r.Time.UnixMilli(), Time: r.Time.UTC().Format(gridhttp.LogTimeLayout), Source: r.Source, Class: r.Class, Text: r.Text})
	}

	e.b.Publish(ctx, events.Event{
		Topic: topic, Type: rxv1.TypeDeviceLog.String(), Audience: admins,
		Payload: deviceLogEvent{DeviceID: device.String(), Reset: reset, Records: entries},
	})
}

// forgotten publishes a forgotten device.
func (e *gridEvents) forgotten(ctx context.Context, d *griddomain.Device) {
	e.b.Publish(ctx, events.Event{
		Topic: topicDevices, Type: rxv1.TypeDeviceStatus.String(), Audience: staff,
		Payload: deviceStatusEvent{DeviceID: d.ID().String(), NodeID: d.Node().String(), State: "forgotten"},
	})
}

// presenceChanged asks for a presence.count (coalesced).
func (e *gridEvents) presenceChanged(context.Context) {
	select {
	case e.presence <- struct{}{}:
	default:
	}
}

// runPresence publishes presence.count when the listener count changed,
// and device.status for the devices whose listeners changed (UI-021), at
// most once per presenceDebounce (a process worker).
func (e *gridEvents) runPresence(ctx context.Context) {
	last := -1
	lastBy := map[string]int{}

	for {
		select {
		case <-ctx.Done():
			return
		case <-e.presence:
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(presenceDebounce):
		}

		n, err := e.listeners.Listeners(ctx)
		if err != nil {
			e.logger.ErrorContext(ctx, "count listeners", slog.Any("error", err))

			continue
		}

		if n != last {
			last = n
			e.b.Publish(ctx, events.Event{Topic: topicPresence, Type: rxv1.TypePresenceCount.String(), Payload: presenceCountEvent{Total: n}})
		}

		lastBy = e.deviceListeners(ctx, lastBy)
	}
}

// deviceListeners publishes device.status for the devices whose listener
// count changed since last; it returns the new counts (last on error).
func (e *gridEvents) deviceListeners(ctx context.Context, last map[string]int) map[string]int {
	by, err := e.listeners.ListenersByDevice(ctx)
	if err != nil {
		e.logger.ErrorContext(ctx, "count listeners by device", slog.Any("error", err))

		return last
	}

	changed := map[string]bool{}

	for id, n := range by {
		if last[id] != n {
			changed[id] = true
		}
	}

	for id := range last {
		if _, ok := by[id]; !ok {
			changed[id] = true
		}
	}

	if len(changed) == 0 {
		return by
	}

	all, err := e.devices.List(ctx)
	if err != nil {
		e.logger.ErrorContext(ctx, "list devices for device.status", slog.Any("error", err))

		return last
	}

	devices := make([]*griddomain.Device, 0, len(changed))

	for _, d := range all {
		if changed[d.ID().String()] {
			devices = append(devices, d)
		}
	}

	e.publishDevices(ctx, devices, by)

	return by
}

// refreshOnDevices reloads the listen policies when a node reports its
// devices (their listen policy overrides may have changed).
func refreshOnDevices(c *policyCache) func(context.Context, griddomain.NodeID, []rxv1.MessageType) {
	return func(ctx context.Context, _ griddomain.NodeID, types []rxv1.MessageType) {
		if slices.Contains(types, rxv1.TypeNodeCapabilities) {
			c.refresh(ctx)
		}
	}
}
