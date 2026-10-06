package control

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/wsconn"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// HubOptions configures the Manager.
type HubOptions struct {
	HubID             string
	CA                *pki.CA
	Client            *pki.ClientSource
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

	revMu  sync.Mutex
	recent []recentRevocation
}

// RevocationMemory is how long revoked sessions and users are re-pushed to
// nodes that (re)connect: longer than any access token lives (TTL ≤ 600 s
// plus the 30 s leeway).
const RevocationMemory = 15 * time.Minute

type recentRevocation struct {
	at       time.Time
	sessions []string
	users    []string
}

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

	return &Manager{o: o, links: map[domain.NodeID]*link{}, wake: make(chan struct{}, 1)}
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

// BroadcastRevocations implements app.RevocationBroadcaster: the revoked
// sessions and users go to every open channel now, and to every channel
// that opens within RevocationMemory.
func (m *Manager) BroadcastRevocations(ctx context.Context, sessions, users []string) {
	if len(sessions) == 0 && len(users) == 0 {
		return
	}

	now := m.o.Now()

	m.revMu.Lock()
	m.recent = append(m.recent, recentRevocation{at: now, sessions: slices.Clone(sessions), users: slices.Clone(users)})
	m.revMu.Unlock()

	rev := ctl.Revocations{Sessions: nonNil(sessions), Users: nonNil(users), CertSerials: []string{}}

	for _, s := range m.sessions() {
		_ = send(s.conn, rxv1.TypeCtlRevocations, rxv1.CorrelationID{}, rev)
	}

	m.o.Logger.DebugContext(ctx, "revocations pushed to the nodes", slog.Int("sessions", len(sessions)), slog.Int("users", len(users)))
}

// recentRevocations returns the sessions and users revoked within
// RevocationMemory, forgetting older ones.
func (m *Manager) recentRevocations() (sessions, users []string) {
	cutoff := m.o.Now().Add(-RevocationMemory)

	m.revMu.Lock()
	defer m.revMu.Unlock()

	m.recent = slices.DeleteFunc(m.recent, func(r recentRevocation) bool { return r.at.Before(cutoff) })

	sessions, users = []string{}, []string{}
	for _, r := range m.recent {
		sessions = append(sessions, r.sessions...)
		users = append(users, r.users...)
	}

	return sessions, users
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

	for id, l := range m.links {
		if !want[id] {
			l.cancel()
			delete(m.links, id)
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

	revoked, err := m.revokedSerials(ctx)
	if err != nil {
		return err
	}

	var (
		leafMu sync.Mutex
		leaf   *x509.Certificate
	)

	check := func(c *x509.Certificate) error {
		// The pin accepts the current certificate and a renewed one sent
		// but not confirmed yet, so a lost acknowledgement never locks the
		// node out.
		if !n.AcceptsFingerprint(pki.Fingerprint(c.Raw)) {
			return fmt.Errorf("%w: node certificate does not match the enrolled one", pki.ErrInvalidCertificate)
		}

		if revoked[pki.SerialString(c)] {
			return pki.ErrRevoked
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

// hubSession is one established channel.
type hubSession struct {
	m      *Manager
	id     domain.NodeID
	conn   *wsconn.Conn
	leaf   *x509.Certificate
	logger *slog.Logger

	boot       shared.UUID
	restricted bool
	// pending is the renewed certificate proposed to the node, if any.
	pending domain.CertInfo
	// renewing is set while a ctl.cert.renew waits for its ack.
	renewing bool
	// renewedLeaf is the certificate of pending.
	renewedLeaf *x509.Certificate
}

func (s *hubSession) run(ctx context.Context, l *link) error {
	o := s.m.o

	hello := ctl.Hello{
		HubID: o.HubID, HubVersion: o.Control.HubVersion(), Protocols: []string{rxv1.ControlSubprotocol},
		ServerTime: o.Now().UnixMilli(), HeartbeatIntervalMS: o.HeartbeatInterval.Milliseconds(),
	}
	if err := send(s.conn, rxv1.TypeCtlHello, rxv1.CorrelationID{}, hello); err != nil {
		return err
	}

	reads := reader(ctx, s.conn)

	welcome, err := s.awaitWelcome(ctx, reads)
	if err != nil {
		return err
	}

	boot, err := shared.ParseUUID(welcome.BootID)
	if err != nil {
		s.conn.Close(rxv1.CloseProtocolViolations, "invalid boot_id")

		return fmt.Errorf("welcome: %w", err)
	}

	if welcome.NodeID != s.id.String() {
		s.conn.Close(rxv1.CloseForbidden, "node id mismatch")

		return fmt.Errorf("welcome from %q on the channel of %s", welcome.NodeID, s.id)
	}

	decision, err := o.Control.Welcome(ctx, s.id, welcome)
	if err != nil {
		return err
	}

	s.boot, s.restricted = boot, decision.Restricted
	l.set(s)

	s.pushKeys(ctx)
	s.pushRevocations(ctx)
	s.confirmPending(ctx)

	if !s.restricted {
		s.maybeRenew(ctx)
	}

	return s.loop(ctx, reads)
}

func (s *hubSession) awaitWelcome(ctx context.Context, reads <-chan readResult) (ctl.Welcome, error) {
	t := time.NewTimer(WelcomeTimeout)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctl.Welcome{}, ctx.Err()
		case <-t.C:
			s.conn.Close(rxv1.CloseHandshakeTimeout, "ctl.welcome not received")

			return ctl.Welcome{}, errors.New("ctl.welcome timeout")
		case r, ok := <-reads:
			if !ok {
				return ctl.Welcome{}, errors.New("channel closed before ctl.welcome")
			}

			if r.err != nil {
				var pe *rxv1.Error
				if errors.As(r.err, &pe) {
					sendError(s.conn, r.err)

					continue
				}

				return ctl.Welcome{}, r.err
			}

			if r.env.Type() != rxv1.TypeCtlWelcome {
				replyError(s.conn, r.env, rxv1.CodeInvalidEnvelope, "ctl.welcome expected")

				continue
			}

			w, err := decode[ctl.Welcome](r.env)
			if err != nil {
				sendError(s.conn, err)

				continue
			}

			return w, nil
		}
	}
}

func (s *hubSession) pushRevocations(ctx context.Context) {
	serials, err := s.m.revokedSerials(ctx)
	if err != nil {
		s.logger.WarnContext(ctx, "load revocation list", slog.Any("error", err))

		return
	}

	list := make([]string, 0, len(serials))
	for k := range serials {
		list = append(list, k)
	}

	sessions, users := s.m.recentRevocations()

	_ = send(s.conn, rxv1.TypeCtlRevocations, rxv1.CorrelationID{}, ctl.Revocations{Sessions: sessions, Users: users, CertSerials: list})
}

// pushKeys sends ctl.keys.update with the current verification keys.
func (s *hubSession) pushKeys(ctx context.Context) {
	o := s.m.o
	if o.Keys == nil {
		return
	}

	keys, err := o.Keys.VerificationKeys(ctx)
	if err != nil {
		s.logger.ErrorContext(ctx, "load access-token verification keys", slog.Any("error", err))

		return
	}

	_ = send(s.conn, rxv1.TypeCtlKeysUpdate, rxv1.CorrelationID{}, ctl.KeysUpdate{
		Issuer: o.Issuer, Keys: keys.Keys, RevokedKids: nonNil(keys.RevokedKids),
	})
}

// confirmPending promotes a renewed certificate the node presents: it
// installed it but the acknowledgement was lost.
func (s *hubSession) confirmPending(ctx context.Context) {
	if s.pending.IsZero() || s.leaf == nil || pki.Fingerprint(s.leaf.Raw) != s.pending.Fingerprint() {
		return
	}

	if err := s.m.o.Control.RecordRenewal(ctx, s.id, s.pending.Serial()); err != nil {
		s.logger.ErrorContext(ctx, "confirm renewed certificate", slog.Any("error", err))

		return
	}

	s.logger.InfoContext(ctx, "renewed node certificate confirmed by the node connection", slog.String("cert_serial", s.pending.Serial()))
	s.pending = domain.CertInfo{}
}

// maybeRenew sends a renewed certificate when 2/3 of the current one's
// validity has elapsed (§4.1). The new certificate is recorded as pending
// before it is sent.
func (s *hubSession) maybeRenew(ctx context.Context) {
	o := s.m.o
	if s.renewing || s.leaf == nil || !o.RenewalDue(s.leaf, o.Now()) {
		return
	}

	der, err := o.CA.RenewNode(s.leaf, o.Now())
	if err != nil {
		s.logger.ErrorContext(ctx, "renew node certificate", slog.Any("error", err))

		return
	}

	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return
	}

	fp := pki.Fingerprint(der)

	info, err := domain.NewCertInfo(fp[:], pki.SerialString(leaf), leaf.NotAfter)
	if err != nil {
		return
	}

	if err := o.Control.ProposeRenewal(ctx, s.id, info); err != nil {
		s.logger.ErrorContext(ctx, "record renewed certificate", slog.Any("error", err))

		return
	}

	s.pending, s.renewing, s.renewedLeaf = info, true, leaf
	chain := []string{b64(der), b64(o.CA.Certificate().Raw)}

	if err := send(s.conn, rxv1.TypeCtlCertRenew, rxv1.MustCorrelationID("cert-renew"), ctl.CertRenew{CertificateChain: chain}); err != nil {
		s.renewing = false
	}
}

func (s *hubSession) loop(ctx context.Context, reads <-chan readResult) error {
	o := s.m.o
	tick := time.NewTicker(BatchWindow)
	renew := time.NewTicker(o.RenewCheckEvery)

	defer tick.Stop()
	defer renew.Stop()

	var batch []app.Event

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}

		upto, err := o.Control.Apply(ctx, s.id, s.boot, s.restricted, batch)
		batch = batch[:0]

		if err != nil {
			s.conn.Close(rxv1.CloseUnavailable, "hub could not persist events")

			return err
		}

		return send(s.conn, rxv1.TypeCtlAck, rxv1.CorrelationID{}, ctl.Ack{UptoSeq: upto})
	}

	for {
		select {
		case <-ctx.Done():
			_ = flush()

			return ctx.Err()
		case <-tick.C:
			if err := flush(); err != nil {
				return err
			}
		case <-renew.C:
			if !s.restricted {
				s.maybeRenew(ctx)
			}
		case r, ok := <-reads:
			if !ok {
				_ = flush()

				return s.conn.Err()
			}

			if r.err != nil {
				var pe *rxv1.Error
				if errors.As(r.err, &pe) {
					sendError(s.conn, r.err)

					continue
				}

				_ = flush()

				return r.err
			}

			if ev, ok := s.handle(ctx, r.env); ok {
				batch = append(batch, ev)

				if len(batch) >= BatchMax {
					if err := flush(); err != nil {
						return err
					}
				}
			}
		}
	}
}

// handle processes one message; it returns the event to ingest, if any.
func (s *hubSession) handle(ctx context.Context, env rxv1.Envelope) (app.Event, bool) {
	t := env.Type()

	if err := rxv1.CtlNodeToHub().Check(t); err != nil {
		replyError(s.conn, env, rxv1.CodeUnsupportedType, "message type not supported on the control channel")

		return app.Event{}, false
	}

	switch t {
	case rxv1.TypeCtlPong, rxv1.TypeCtlWelcome:
		return app.Event{}, false
	case rxv1.TypeError:
		s.logger.WarnContext(ctx, "node reported an error", slog.String("payload", string(env.Payload())))

		if e, err := decode[rxv1.ErrorPayload](env); err == nil && e.Re != nil && e.Re.String() == "cert-renew" {
			// The node kept its certificate: the pending one stays
			// accepted until the next attempt replaces it.
			s.renewing = false
		}

		return app.Event{}, false
	case rxv1.TypeAck:
		s.onAck(ctx, env)

		return app.Event{}, false
	}

	seq, err := decode[ctl.SeqOnly](env)
	if err != nil || seq.Seq <= 0 {
		replyError(s.conn, env, rxv1.CodeInvalidPayload, "node events need a positive seq")

		return app.Event{}, false
	}

	return app.Event{Seq: seq.Seq, Type: t, Payload: env.Payload()}, true
}

func (s *hubSession) onAck(ctx context.Context, env rxv1.Envelope) {
	var ack struct {
		Re *rxv1.CorrelationID `json:"re"`
	}

	if err := env.DecodePayload(&ack, false); err != nil || ack.Re == nil {
		return
	}

	if ack.Re.String() == "cert-renew" && s.renewing {
		s.renewing = false

		if err := s.m.o.Control.RecordRenewal(ctx, s.id, s.pending.Serial()); err != nil {
			// The certificate stays pending and accepted; the next
			// connection presenting it promotes it.
			s.logger.ErrorContext(ctx, "record renewed certificate", slog.Any("error", err))

			return
		}

		s.pending = domain.CertInfo{}
		s.leaf = s.renewedLeaf
	}
}
