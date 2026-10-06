package control

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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

	// Tunables; zero values take the spec defaults.
	ReconcileEvery time.Duration
	MinBackoff     time.Duration
	MaxBackoff     time.Duration
	PingInterval   time.Duration
	PongTimeout    time.Duration
}

// Manager keeps exactly one control channel per enrolled, enabled node
// (§4.4) and implements app.Links.
type Manager struct {
	o HubOptions

	mu    sync.Mutex
	links map[domain.NodeID]*link
	wake  chan struct{}
}

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

	return &Manager{o: o, links: map[domain.NodeID]*link{}, wake: make(chan struct{}, 1)}
}

var _ app.Links = (*Manager)(nil)

// Run reconciles the channels until ctx is done, then closes them all.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(m.o.ReconcileEvery)
	defer t.Stop()

	for {
		m.reconcile(ctx)

		select {
		case <-ctx.Done():
			m.stopAll()

			return
		case <-t.C:
		case <-m.wake:
		}
	}
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

	pin := n.Certificate()

	var (
		leafMu sync.Mutex
		leaf   *x509.Certificate
	)

	check := func(c *x509.Certificate) error {
		if pki.Fingerprint(c.Raw) != pin.Fingerprint() {
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
	s := &hubSession{m: m, id: id, conn: conn, leaf: leaf, logger: m.o.Logger.With(slog.String("node_id", id.String()))}
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
	renewal    *domain.CertInfo
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

	s.pushRevocations(ctx)

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

	_ = send(s.conn, rxv1.TypeCtlRevocations, rxv1.CorrelationID{}, ctl.Revocations{Sessions: []string{}, Users: []string{}, CertSerials: list})
}

// maybeRenew sends a renewed certificate when 2/3 of the current one's
// validity has elapsed (§4.1).
func (s *hubSession) maybeRenew(ctx context.Context) {
	o := s.m.o
	if s.leaf == nil || !pki.RenewalDue(s.leaf, o.Now()) {
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

	s.renewal = &info
	chain := []string{b64(der), b64(o.CA.Certificate().Raw)}

	if err := send(s.conn, rxv1.TypeCtlCertRenew, rxv1.MustCorrelationID("cert-renew"), ctl.CertRenew{CertificateChain: chain}); err != nil {
		s.renewal = nil
	}
}

func (s *hubSession) loop(ctx context.Context, reads <-chan readResult) error {
	o := s.m.o
	tick := time.NewTicker(BatchWindow)

	defer tick.Stop()

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

	if ack.Re.String() == "cert-renew" && s.renewal != nil {
		if err := s.m.o.Control.RecordRenewal(ctx, s.id, *s.renewal); err != nil {
			s.logger.ErrorContext(ctx, "record renewed certificate", slog.Any("error", err))
		}

		s.renewal = nil
	}
}
