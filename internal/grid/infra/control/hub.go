package control

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/wsconn"
)

// HubOptions configures the Manager.
type HubOptions struct {
	HubID             string
	CA                *pki.CA
	Client            *pki.CertSource
	Nodes             domain.NodeRepository
	Revocations       domain.RevocationRepository
	Control           *app.Control
	HeartbeatInterval time.Duration
	Now               func() time.Time
	Logger            *slog.Logger
	// Keys are the access-token verification keys pushed with
	// ctl.keys.update; Issuer is the token issuer (hub.url). Without Keys
	// no ctl.keys.update is sent and nodes refuse media connects.
	Keys   app.KeySource
	Issuer string
	// State pushes the desired state of the devices (ADR 0020); nil sends
	// none.
	State *app.States
	// Logs receives the device logs (device.log, SRC-005); nil drops them.
	Logs *app.DeviceLogs

	// Tunables; zero values take the spec defaults.
	ReconcileEvery time.Duration
	MinBackoff     time.Duration
	MaxBackoff     time.Duration
	PingInterval   time.Duration
	PongTimeout    time.Duration
	// RenewCheckEvery is the period of the certificate renewal check of an
	// open channel (daily, ADR 0008).
	RenewCheckEvery time.Duration
	// RenewalDue decides whether a node certificate must be renewed.
	RenewalDue func(leaf *x509.Certificate, now time.Time) bool
}

// Manager keeps exactly one control channel per enrolled, enabled node
// (§4.4) and implements app.Links.
type Manager struct {
	o HubOptions

	mu    sync.Mutex
	links map[domain.NodeID]*link
	wake  chan struct{}

	revMu       sync.Mutex
	revSessions map[string]time.Time
	revUsers    map[string]time.Time

	// heartbeat is the interval sent in ctl.hello (ns); it follows the
	// grid.heartbeat_interval_s setting.
	heartbeat atomic.Int64
}

// SetHeartbeatInterval changes the heartbeat interval sent to nodes in
// ctl.hello: a node takes it when its control channel (re)connects.
func (m *Manager) SetHeartbeatInterval(d time.Duration) { m.heartbeat.Store(int64(d)) }

// RevocationMemory is how long revoked sessions and users are re-pushed to
// nodes that (re)connect: longer than any access token lives (TTL ≤ 600 s
// plus the 30 s leeway).
const RevocationMemory = 15 * time.Minute

var _ app.RevocationBroadcaster = (*Manager)(nil)

type link struct {
	cancel context.CancelFunc
	done   chan struct{}

	mu      sync.Mutex
	session *hubSession
}

func (l *link) current() *hubSession {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.session
}

func (l *link) set(s *hubSession) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.session = s
}

// NewManager returns the manager.
func NewManager(o HubOptions) *Manager {
	if o.ReconcileEvery == 0 {
		o.ReconcileEvery = 5 * time.Second
	}

	if o.MinBackoff == 0 {
		o.MinBackoff = time.Second
	}

	if o.MaxBackoff == 0 {
		o.MaxBackoff = time.Minute
	}

	if o.PingInterval == 0 {
		o.PingInterval = PingInterval
	}

	if o.PongTimeout == 0 {
		o.PongTimeout = PongTimeout
	}

	if o.RenewCheckEvery == 0 {
		o.RenewCheckEvery = 24 * time.Hour
	}

	if o.RenewalDue == nil {
		o.RenewalDue = pki.RenewalDue
	}

	m := &Manager{
		o: o, links: map[domain.NodeID]*link{}, wake: make(chan struct{}, 1),
		revSessions: map[string]time.Time{}, revUsers: map[string]time.Time{},
	}
	m.heartbeat.Store(int64(o.HeartbeatInterval))

	return m
}

var _ app.Links = (*Manager)(nil)

// Run reconciles the channels until ctx is done, then closes them all. Key
// changes are pushed to every open channel.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(m.o.ReconcileEvery)
	defer t.Stop()

	var keysChanged <-chan struct{}
	if m.o.Keys != nil {
		keysChanged = m.o.Keys.Changed()
	}

	for {
		m.reconcile(ctx)

		select {
		case <-ctx.Done():
			m.stopAll()

			return
		case <-t.C:
		case <-m.wake:
		case <-keysChanged:
			for _, s := range m.sessions() {
				s.pushKeys(ctx)
			}
		}
	}
}

// sessions returns the open channels.
func (m *Manager) sessions() []*hubSession {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]*hubSession, 0, len(m.links))

	for _, l := range m.links {
		if s := l.current(); s != nil {
			out = append(out, s)
		}
	}

	return out
}

// BroadcastRevocations implements app.RevocationBroadcaster: the sessions
// and users revoked at (hub clock; zero means now) go to every open channel
// now, and to every channel that opens within RevocationMemory. An entry
// keeps its first revocation time: a later push never moves it.
func (m *Manager) BroadcastRevocations(ctx context.Context, at time.Time, sessions, users []string) {
	if len(sessions) == 0 && len(users) == 0 {
		return
	}

	if at.IsZero() {
		at = m.o.Now()
	}

	m.revMu.Lock()
	rev := ctl.Revocations{Sessions: remember(m.revSessions, sessions, at), Users: remember(m.revUsers, users, at), CertSerials: []string{}}
	m.revMu.Unlock()

	for _, s := range m.sessions() {
		_ = send(s.conn, rxv1.TypeCtlRevocations, rxv1.CorrelationID{}, rev)
	}

	m.o.Logger.DebugContext(ctx, "revocations pushed to the nodes", slog.Int("sessions", len(sessions)), slog.Int("users", len(users)))
}

// remember records ids revoked at, keeping the latest revocation of each
// (a second revocation also refuses the tokens issued since the first),
// and returns the entries to push.
func remember(known map[string]time.Time, ids []string, at time.Time) []ctl.Revoked {
	out := make([]ctl.Revoked, 0, len(ids))

	for _, id := range ids {
		if last, ok := known[id]; !ok || at.After(last) {
			known[id] = at
		}

		out = append(out, ctl.Revoked{ID: id, At: known[id].UnixMilli()})
	}

	return out
}

// recentRevocations returns the sessions and users revoked within
// RevocationMemory, with their revocation times, forgetting older ones.
func (m *Manager) recentRevocations() (sessions, users []ctl.Revoked) {
	cutoff := m.o.Now().Add(-RevocationMemory)

	m.revMu.Lock()
	defer m.revMu.Unlock()

	list := func(known map[string]time.Time) []ctl.Revoked {
		out := []ctl.Revoked{}

		for id, at := range known {
			if at.Before(cutoff) {
				delete(known, id)

				continue
			}

			out = append(out, ctl.Revoked{ID: id, At: at.UnixMilli()})
		}

		return out
	}

	return list(m.revSessions), list(m.revUsers)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}

	return s
}

// Wake implements app.Links.
func (m *Manager) Wake() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) reconcile(ctx context.Context) {
	nodes, err := m.o.Nodes.List(ctx)
	if err != nil {
		if ctx.Err() == nil {
			m.o.Logger.ErrorContext(ctx, "list nodes", slog.Any("error", err))
		}

		return
	}

	want := map[domain.NodeID]bool{}

	for _, n := range nodes {
		if n.Active() {
			want[n.ID()] = true
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// A node removed, revoked or disabled by another process (the CLI while
	// the hub runs) is dropped like through Drop: revocations, then 4403.
	for id, l := range m.links {
		if !want[id] {
			delete(m.links, id)

			go m.drop(context.WithoutCancel(ctx), l)
		}
	}

	for id := range want {
		if _, ok := m.links[id]; ok {
			continue
		}

		lctx, cancel := context.WithCancel(ctx)
		l := &link{cancel: cancel, done: make(chan struct{})}
		m.links[id] = l

		go func() {
			defer close(l.done)
			m.supervise(lctx, id, l)
		}()
	}
}

func (m *Manager) stopAll() {
	m.mu.Lock()
	links := m.links
	m.links = map[domain.NodeID]*link{}
	m.mu.Unlock()

	for _, l := range links {
		l.cancel()
	}

	for _, l := range links {
		<-l.done
	}
}

// Drop implements app.Links: the channel is closed with 4403 after the
// revocation list is pushed.
func (m *Manager) Drop(ctx context.Context, id domain.NodeID) {
	m.mu.Lock()
	l, ok := m.links[id]
	delete(m.links, id)
	m.mu.Unlock()

	if !ok {
		return
	}

	m.drop(ctx, l)
}

// drop pushes the revocation list on the open channel of l, closes it with
// 4403 and waits for the close handshake before cancelling it.
func (m *Manager) drop(ctx context.Context, l *link) {
	if s := l.current(); s != nil {
		s.pushRevocations(ctx)
		s.conn.Close(rxv1.CloseForbidden, "node removed, disabled or re-enrolled")

		// Let the revocations and the 4403 reach the node before the
		// session context is cancelled.
		select {
		case <-s.conn.Finished():
		case <-time.After(DropTimeout):
		}
	}

	l.cancel()
}

// Probe implements app.Links.
func (m *Manager) Probe(_ context.Context, id domain.NodeID) error {
	m.mu.Lock()
	l, ok := m.links[id]
	m.mu.Unlock()

	if !ok || l.current() == nil {
		return domain.ErrNodeUnavailable
	}

	return send(l.current().conn, rxv1.TypeCtlCapabilitiesProbe, rxv1.MustCorrelationID("probe"), ctl.Empty{})
}

// SendState implements app.StateSender: ctl.state.apply on the open
// channel of id.
func (m *Manager) SendState(_ context.Context, id domain.NodeID, st ctl.StateApply) error {
	m.mu.Lock()
	l, ok := m.links[id]
	m.mu.Unlock()

	if !ok || l.current() == nil {
		return domain.ErrNodeUnavailable
	}

	return send(l.current().conn, rxv1.TypeCtlStateApply, rxv1.MustCorrelationID("state"), st)
}

// Connected reports whether a channel to id is up.
func (m *Manager) Connected(id domain.NodeID) bool {
	m.mu.Lock()
	l, ok := m.links[id]
	m.mu.Unlock()

	return ok && l.current() != nil
}

// supervise dials id with jittered exponential back-off until ctx is done.
func (m *Manager) supervise(ctx context.Context, id domain.NodeID, l *link) {
	delay := m.o.MinBackoff

	for ctx.Err() == nil {
		start := time.Now()
		err := m.connect(ctx, id, l)

		if ctx.Err() != nil {
			return
		}

		if time.Since(start) > time.Minute {
			delay = m.o.MinBackoff
		}

		m.o.Logger.DebugContext(ctx, "control channel ended", slog.String("node_id", id.String()),
			slog.Duration("retry_in", delay), slog.Any("error", err))

		t := time.NewTimer(app.Jitter(delay))

		select {
		case <-ctx.Done():
			t.Stop()

			return
		case <-t.C:
		}

		delay = min(2*delay, m.o.MaxBackoff)
	}
}

func (m *Manager) connect(ctx context.Context, id domain.NodeID, l *link) error {
	n, err := m.o.Nodes.Get(ctx, id)
	if err != nil {
		return err
	}

	if !n.Active() {
		return errors.New("node is not active")
	}

	pinned, err := m.pin(ctx, n)
	if err != nil {
		return err
	}

	var (
		leafMu sync.Mutex
		leaf   *x509.Certificate
	)

	check := func(c *x509.Certificate) error {
		if err := pinned(c); err != nil {
			return err
		}

		leafMu.Lock()
		leaf = c
		leafMu.Unlock()

		return nil
	}

	tr := &http.Transport{TLSClientConfig: pki.HubDialConfig(m.o.Client, m.o.CA.Pool(), id.String(), check)}
	defer tr.CloseIdleConnections()

	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	ws, err := wsconn.Dial(dctx, n.URL().Endpoint("wss", "/control"), &http.Client{Transport: tr}, rxv1.ControlSubprotocol)

	cancel()

	if err != nil {
		m.o.Control.DialFailed(ctx, id, err)

		return err
	}

	conn := wsconn.New(ctx, ws, wsconn.Options{
		ReadLimit: ReadLimit, MaxQueueBytes: MaxQueueBytes, PingInterval: m.o.PingInterval, PongTimeout: m.o.PongTimeout,
	})

	leafMu.Lock()
	s := &hubSession{m: m, id: id, conn: conn, leaf: leaf, pending: n.PendingCertificate(), logger: m.o.Logger.With(slog.String("node_id", id.String()))}
	leafMu.Unlock()

	err = s.run(ctx, l)

	l.set(nil)
	conn.Close(rxv1.CloseGoingAway, "hub closing")
	m.o.Control.Disconnected(ctx, id, err)

	return err
}

// pin returns the check of node n's certificate: the pinned fingerprint
// (the current certificate, or a renewed one sent but not confirmed yet, so
// a lost acknowledgement never locks the node out) and the revocation list.
func (m *Manager) pin(ctx context.Context, n *domain.Node) (pki.LeafCheck, error) {
	revoked, err := m.revokedSerials(ctx)
	if err != nil {
		return nil, err
	}

	return func(c *x509.Certificate) error {
		if !n.AcceptsFingerprint(pki.Fingerprint(c.Raw)) {
			return fmt.Errorf("%w: node certificate does not match the enrolled one", pki.ErrInvalidCertificate)
		}

		if revoked[pki.SerialString(c)] {
			return pki.ErrRevoked
		}

		return nil
	}, nil
}

// GatewayDialConfig returns the TLS config of a gateway connection to node
// id: TLS 1.3, the gateway client certificate of client, the node server
// name and identity, its pinned certificate and the revocation list. The
// node must be enrolled and enabled.
func (m *Manager) GatewayDialConfig(ctx context.Context, client *pki.CertSource, id string) (*tls.Config, error) {
	nid, err := domain.NewNodeID(id)
	if err != nil {
		return nil, err
	}

	n, err := m.o.Nodes.Get(ctx, nid)
	if err != nil {
		return nil, err
	}

	if !n.Active() {
		return nil, domain.ErrNodeNotFound
	}

	check, err := m.pin(ctx, n)
	if err != nil {
		return nil, err
	}

	return pki.HubDialConfig(client, m.o.CA.Pool(), id, check), nil
}

func (m *Manager) revokedSerials(ctx context.Context) (map[string]bool, error) {
	list, err := m.o.Revocations.List(ctx, m.o.Now())
	if err != nil {
		return nil, err
	}

	out := make(map[string]bool, len(list))
	for _, r := range list {
		out[r.Serial()] = true
	}

	return out, nil
}
