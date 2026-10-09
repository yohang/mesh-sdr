package control

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/wsconn"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

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

	interval := time.Duration(s.m.heartbeat.Load())
	hello := ctl.Hello{
		HubID: o.HubID, HubVersion: o.Control.HubVersion(), Protocols: []string{rxv1.ControlSubprotocol},
		ServerTime: o.Now().UnixMilli(), HeartbeatIntervalMS: interval.Milliseconds(),
	}
	if err := send(s.conn, rxv1.TypeCtlHello, rxv1.CorrelationID{}, hello); err != nil {
		return err
	}

	o.Control.HelloSent(s.id, interval)

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

		if o.State != nil {
			if err := o.State.Welcomed(ctx, s.id, welcome.LastAppliedRevision); err != nil {
				s.logger.ErrorContext(ctx, "push desired state", slog.Any("error", err))
			}
		}
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
	case rxv1.TypeCtlStateApplied:
		s.onStateApplied(ctx, env)

		return app.Event{}, false
	case rxv1.TypeDeviceLog:
		s.onDeviceLog(ctx, env)

		return app.Event{}, false
	}

	seq, err := decode[ctl.SeqOnly](env)
	if err != nil || seq.Seq <= 0 {
		replyError(s.conn, env, rxv1.CodeInvalidPayload, "node events need a positive seq")

		return app.Event{}, false
	}

	return app.Event{Seq: seq.Seq, Type: t, Payload: env.Payload()}, true
}

// onStateApplied records the node's answer to a desired state; it is not
// an event (no seq).
func (s *hubSession) onStateApplied(ctx context.Context, env rxv1.Envelope) {
	o := s.m.o

	a, err := decode[ctl.StateApplied](env)
	if err != nil {
		sendError(s.conn, err)

		return
	}

	if o.State != nil {
		o.State.Applied(ctx, s.id, a)
		o.Control.Touch(ctx, s.id)
	}
}

// onDeviceLog hands device log records to the hub; they are not events
// (no seq, no ack). A restricted channel (incompatible node) carries none.
func (s *hubSession) onDeviceLog(ctx context.Context, env rxv1.Envelope) {
	o := s.m.o

	l, err := decode[ctl.DeviceLog](env)
	if err != nil {
		sendError(s.conn, err)

		return
	}

	if o.Logs != nil && !s.restricted {
		o.Logs.Receive(ctx, s.id, l)
	}
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
