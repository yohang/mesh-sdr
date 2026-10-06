package control

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/agent"
	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
	"github.com/yohang/mesh-sdr/internal/http/problem"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/wsconn"
)

func b64(der []byte) string { return base64.StdEncoding.EncodeToString(der) }

// Renewer installs a certificate chain renewed by the hub.
type Renewer interface {
	Renew(chain [][]byte) error
}

// Media is the node's media side, driven by the control channel.
type Media interface {
	// UpdateKeys installs the access-token keys of ctl.keys.update.
	UpdateKeys(u ctl.KeysUpdate) error
	// Revoke closes the media connections named by ctl.revocations.
	Revoke(r ctl.Revocations)
	// Withdrawn is called when the hub closed the control channel with
	// 4403: the node was removed, disabled or re-enrolled.
	Withdrawn()
}

// NodeOptions configures a NodeServer.
type NodeOptions struct {
	Agent *agent.Agent
	// Media receives keys, revocations and withdrawals; nil ignores them.
	Media Media
	// HubIdentity is the expected hub id; empty accepts any hub URI SAN.
	HubIdentity  string
	Revoked      *pki.RevokedSet
	Renewer      Renewer
	HelloTimeout time.Duration
	// QueueBytes bounds the outbound queue of a channel (default
	// MaxQueueBytes). Event replay is paged to half of it.
	QueueBytes int
	Now        func() time.Time
	Logger     *slog.Logger
}

// NodeServer serves /control on an enrolled node: one hub channel at a
// time, the newest authenticated one wins (§4.9).
type NodeServer struct {
	o NodeOptions

	mu      sync.Mutex
	current *wsconn.Conn
	ctx     context.Context
	ready   chan struct{}
	once    sync.Once
}

// NewNodeServer returns the server.
func NewNodeServer(o NodeOptions) *NodeServer {
	if o.HelloTimeout == 0 {
		o.HelloTimeout = HelloTimeout
	}

	if o.QueueBytes == 0 {
		o.QueueBytes = MaxQueueBytes
	}

	return &NodeServer{o: o, ready: make(chan struct{})}
}

// Run binds the sessions to ctx; when ctx is done the current channel is
// closed with 1001.
func (s *NodeServer) Run(ctx context.Context) {
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
	s.once.Do(func() { close(s.ready) })

	<-ctx.Done()

	s.mu.Lock()
	cur := s.current
	s.mu.Unlock()

	if cur != nil {
		cur.Close(rxv1.CloseGoingAway, "node shutting down")
	}
}

func (s *NodeServer) baseContext() context.Context {
	<-s.ready

	s.mu.Lock()
	defer s.mu.Unlock()

	return s.ctx
}

// ServeHTTP implements http.Handler for GET /control.
func (s *NodeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	kind, id, ok := pki.PeerIdentity(r)
	if !ok || kind != pki.KindHub || (s.o.HubIdentity != "" && id != s.o.HubIdentity) {
		s.o.Logger.WarnContext(r.Context(), "control channel refused: not the hub",
			slog.String("peer_kind", kind), slog.String("peer_id", id), slog.String("remote_addr", r.RemoteAddr))
		problem.Write(w, problem.New(http.StatusForbidden, problem.CodeForbidden, "only the hub may open the control channel"))

		return
	}

	ws, err := wsconn.Accept(w, r, rxv1.ControlSubprotocol)
	if err != nil {
		s.o.Logger.DebugContext(r.Context(), "control upgrade failed", slog.Any("error", err))

		return
	}

	ctx := s.baseContext()
	conn := wsconn.New(ctx, ws, wsconn.Options{ReadLimit: ReadLimit, MaxQueueBytes: s.o.QueueBytes})

	s.mu.Lock()
	old := s.current
	s.current = conn
	s.mu.Unlock()

	if old != nil {
		old.Close(rxv1.CloseNormal, "replaced by a newer control channel")
	}

	s.o.Logger.InfoContext(ctx, "control channel opened", slog.String("hub_id", id))

	err = (&nodeSession{s: s, conn: conn}).run(ctx)

	s.mu.Lock()
	if s.current == conn {
		s.current = nil
	}
	s.mu.Unlock()

	conn.Close(rxv1.CloseGoingAway, "node closing")
	s.o.Logger.InfoContext(ctx, "control channel closed", slog.Any("cause", err))

	if wsconn.CloseStatus(err) == rxv1.CloseForbidden && s.o.Media != nil {
		s.o.Logger.WarnContext(ctx, "the hub withdrew this node (removed, disabled or re-enrolled): media connections closed")
		s.o.Media.Withdrawn()
	}
}

type nodeSession struct {
	s        *NodeServer
	conn     *wsconn.Conn
	started  bool
	lastSent int64
	// more is set when flush stopped with events left to send.
	more bool
}

// pageRetry is the delay before sending the next page of a backlog.
const pageRetry = 20 * time.Millisecond

func (n *nodeSession) run(ctx context.Context) error {
	o := n.s.o
	hello := time.AfterFunc(o.HelloTimeout, func() { n.conn.Close(rxv1.CloseHandshakeTimeout, "ctl.hello not received in time") })

	defer hello.Stop()

	reads := reader(ctx, n.conn)
	buf := o.Agent.Buffer()
	page := time.NewTicker(pageRetry)

	defer page.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-buf.Notify():
			n.flush()
		case <-page.C:
			if n.more {
				n.flush()
			}
		case r, ok := <-reads:
			if !ok {
				return n.conn.Err()
			}

			if r.err != nil {
				var pe *rxv1.Error
				if errors.As(r.err, &pe) {
					sendError(n.conn, r.err)

					continue
				}

				return r.err
			}

			n.handle(ctx, r.env, hello)
		}
	}
}

// flush sends the buffered events not sent on this channel yet, one page
// at a time: it stops while the outbound queue holds half its bound, so a
// large backlog after an outage never overflows the queue (4413); the rest
// goes as the hub reads.
func (n *nodeSession) flush() {
	n.more = false

	if !n.started {
		return
	}

	window := n.s.o.QueueBytes / 2

	for _, e := range n.s.o.Agent.Buffer().After(n.lastSent) {
		if n.conn.Pending() > 0 && n.conn.Pending()+e.Size > window {
			n.more = true

			return
		}

		env, err := rxv1.NewEnvelope(e.Type, rxv1.CorrelationID{}, time.Now().UnixMilli(), e.Payload)
		if err != nil {
			n.s.o.Logger.Error("encode node event", slog.Any("error", err))

			continue
		}

		if err := n.conn.Send(env); err != nil {
			return
		}

		n.lastSent = e.Seq
	}
}

func (n *nodeSession) handle(ctx context.Context, env rxv1.Envelope, hello *time.Timer) {
	o := n.s.o
	t := env.Type()

	if err := rxv1.CtlHubToNode().Check(t); err != nil {
		replyError(n.conn, env, rxv1.CodeUnsupportedType, "message type not supported on the control channel")

		return
	}

	if !n.started && t != rxv1.TypeCtlHello {
		replyError(n.conn, env, rxv1.CodeInvalidEnvelope, "ctl.hello expected first")

		return
	}

	switch t {
	case rxv1.TypeCtlHello:
		n.onHello(ctx, env, hello)
	case rxv1.TypeCtlAck:
		if ack, err := decode[ctl.Ack](env); err == nil {
			o.Agent.Buffer().Ack(ack.UptoSeq)
		}
	case rxv1.TypeCtlPing:
		_ = send(n.conn, rxv1.TypeCtlPong, rxv1.CorrelationID{}, ctl.Empty{})
	case rxv1.TypeCtlCapabilitiesProbe:
		o.Agent.EmitCapabilities(ctx)
		sendAck(n.conn, env)
	case rxv1.TypeCtlRevocations:
		if rev, err := decode[ctl.Revocations](env); err == nil {
			o.Revoked.Add(rev.CertSerials...)

			if o.Media != nil {
				o.Media.Revoke(rev)
			}

			sendAck(n.conn, env)
		} else {
			sendError(n.conn, err)
		}
	case rxv1.TypeCtlKeysUpdate:
		n.onKeys(ctx, env)
	case rxv1.TypeCtlCertRenew:
		n.onRenew(ctx, env)
	case rxv1.TypeAck, rxv1.TypeError:
		o.Logger.DebugContext(ctx, "hub answer", slog.String("type", string(t)), slog.String("payload", string(env.Payload())))
	default:
		if _, ok := env.ID(); ok {
			replyError(n.conn, env, rxv1.CodeUnsupportedType, "not implemented by this node yet")
		} else {
			o.Logger.WarnContext(ctx, "control message not implemented yet: ignored", slog.String("type", string(t)))
		}
	}
}

func (n *nodeSession) onHello(ctx context.Context, env rxv1.Envelope, hello *time.Timer) {
	o := n.s.o

	if n.started {
		replyError(n.conn, env, rxv1.CodeInvalidEnvelope, "ctl.hello already received")

		return
	}

	h, err := decode[ctl.Hello](env)
	if err != nil {
		sendError(n.conn, err)

		return
	}

	hello.Stop()

	received := o.Now()
	w := o.Agent.Welcome(h, received)

	if err := send(n.conn, rxv1.TypeCtlWelcome, rxv1.CorrelationID{}, w); err != nil {
		return
	}

	n.started = true
	n.lastSent = o.Agent.Buffer().Acked()

	o.Agent.EmitDropped()
	o.Agent.EmitCapabilities(ctx)
	n.flush()

	// Refine the clock offset with the RTT (§4.5).
	go func() {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		if rtt, err := n.conn.RTT(rctx); err == nil {
			o.Agent.SetClock(h.ServerTime, received, rtt)
		}
	}()
}

func (n *nodeSession) onKeys(ctx context.Context, env rxv1.Envelope) {
	o := n.s.o

	u, err := decode[ctl.KeysUpdate](env)
	if err != nil {
		sendError(n.conn, err)

		return
	}

	if o.Media == nil {
		sendAck(n.conn, env)

		return
	}

	if err := o.Media.UpdateKeys(u); err != nil {
		o.Logger.ErrorContext(ctx, "access-token keys rejected", slog.Any("error", err))
		replyError(n.conn, env, rxv1.CodeInvalidPayload, "invalid access-token keys")

		return
	}

	o.Logger.DebugContext(ctx, "access-token keys installed", slog.Int("keys", len(u.Keys)))
	sendAck(n.conn, env)
}

func (n *nodeSession) onRenew(ctx context.Context, env rxv1.Envelope) {
	o := n.s.o

	req, err := decode[ctl.CertRenew](env)
	if err != nil {
		sendError(n.conn, err)

		return
	}

	chain := make([][]byte, 0, len(req.CertificateChain))

	for _, c := range req.CertificateChain {
		der, err := base64.StdEncoding.DecodeString(c)
		if err != nil {
			replyError(n.conn, env, rxv1.CodeInvalidPayload, "certificate_chain must be base64 DER")

			return
		}

		chain = append(chain, der)
	}

	if o.Renewer == nil || len(chain) == 0 {
		replyError(n.conn, env, rxv1.CodeConflict, "certificate renewal is not possible")

		return
	}

	if err := o.Renewer.Renew(chain); err != nil {
		o.Logger.ErrorContext(ctx, "install renewed certificate", slog.Any("error", err))
		replyError(n.conn, env, rxv1.CodeConflict, "renewed certificate rejected")

		return
	}

	o.Logger.InfoContext(ctx, "node certificate renewed")
	sendAck(n.conn, env)
}
