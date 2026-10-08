package media

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/agent"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/media"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/sendq"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/token"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/wsconn"
)

// session is one media connection.
type session struct {
	s      *Server
	logger *slog.Logger

	mu      sync.Mutex
	conn    *wsconn.Conn
	cur     token.Claims
	pending *closeReq

	// forbidden counts the forbidden messages of the last minute (read
	// loop only). It is separate from the inbound rate limiter of run
	// (msgLimiter): a scope violation closes with 4403, a flood with 4429.
	forbidden strikes

	hello   media.Hello
	queue   *sendq.Queue
	streams media.StreamSession

	// device and demod are what the connection listens to (Attached),
	// carried by its presence events.
	device, demod string
}

// strikes is a sliding one-minute window of violations.
type strikes struct{ at []time.Time }

// add records a violation at now and returns the count of the last minute.
func (s *strikes) add(now time.Time) int {
	keep := s.at[:0]

	for _, t := range s.at {
		if now.Sub(t) < time.Minute {
			keep = append(keep, t)
		}
	}

	s.at = append(keep, now)

	return len(s.at)
}

type closeReq struct {
	code   rxv1.CloseCode
	reason string
}

func (ss *session) claims() token.Claims {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	return ss.cur
}

// kid returns the key id of the current token: a key update that no longer
// publishes it closes the session.
func (ss *session) kid() string {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	return ss.cur.KeyID
}

// close closes the connection, or as soon as it exists.
func (ss *session) close(code rxv1.CloseCode, reason string) {
	ss.mu.Lock()
	conn := ss.conn

	if conn == nil {
		ss.pending = &closeReq{code: code, reason: reason}
	}
	ss.mu.Unlock()

	if conn != nil {
		conn.Close(code, reason)
	}
}

func (ss *session) attach(conn *wsconn.Conn) bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	ss.conn = conn

	if ss.pending != nil {
		conn.Close(ss.pending.code, ss.pending.reason)

		return false
	}

	return true
}

func (ss *session) send(typ rxv1.MessageType, id rxv1.CorrelationID, payload any) {
	env, err := rxv1.NewEnvelope(typ, id, time.Now().UnixMilli(), payload)
	if err != nil {
		ss.logger.Error("encode media message", slog.String("type", string(typ)), slog.Any("error", err))

		return
	}

	_ = ss.conn.Send(env)
}

func (ss *session) reply(env rxv1.Envelope, code rxv1.ErrorCode, reason string) {
	e := &rxv1.Error{Code: code, Reason: reason}
	if id, ok := env.ID(); ok {
		e.ID = id
	}

	ss.sendError(e)
}

func (ss *session) sendError(err error) {
	if env, eerr := rxv1.NewErrorEnvelope(time.Now().UnixMilli(), rxv1.ErrorPayloadFrom(err, "")); eerr == nil {
		_ = ss.conn.Send(env)
	}
}

func (ss *session) ack(env rxv1.Envelope) {
	if id, ok := env.ID(); ok {
		if a, err := rxv1.NewAckEnvelope(time.Now().UnixMilli(), id, nil); err == nil {
			_ = ss.conn.Send(a)
		}
	}
}

// expiry returns when the session must close without a refresh: exp plus
// the grace (§5.8).
func (ss *session) expiry() time.Duration {
	return ss.claims().ExpiresAt.Add(token.Leeway).Sub(ss.s.o.Now())
}

func (ss *session) run(ctx context.Context) {
	o := ss.s.o
	c := ss.claims()

	ss.emit(rxv1.TypeConnectionOpened, c, "")
	ss.logger.InfoContext(ctx, "media connection opened", slog.String("sub", c.Subject))

	beats := ss.heartbeats(ctx)

	hello := time.AfterFunc(HelloTimeout, func() { ss.conn.Close(rxv1.CloseHandshakeTimeout, "session.hello not received in time") })
	expire := time.AfterFunc(ss.expiry(), func() { ss.conn.Close(rxv1.CloseUnauthenticated, "token_expired") })

	defer hello.Stop()
	defer expire.Stop()

	started := false
	reason := "client"
	limiter := newMsgLimiter(defaultMsgRate)

	for {
		env, err := ss.conn.Read(ctx)

		var pe *rxv1.Error
		if err == nil || errors.As(err, &pe) {
			if ok, retry, escalate := limiter.allow(o.Now()); !ok {
				if escalate {
					ss.logger.WarnContext(ctx, "media connection closed: inbound message rate exceeded repeatedly")
					ss.conn.Close(rxv1.CloseRateLimited, "rate_limited")

					continue
				}

				ss.rateLimited(env, retry)

				continue
			}
		}

		if err != nil {
			if pe != nil {
				ss.sendError(err)

				continue
			}

			if code := wsconn.CloseStatus(err); code != 0 && code != rxv1.CloseNormal && code != rxv1.CloseGoingAway {
				reason = "policy"
			}

			break
		}

		t := env.Type()

		if err := rxv1.MediaClientToNode().Check(t); err != nil {
			ss.reply(env, rxv1.CodeUnsupportedType, "message type not supported on the media connection")

			continue
		}

		if !started && t != rxv1.TypeSessionHello {
			ss.reply(env, rxv1.CodeInvalidEnvelope, "session.hello expected first")

			continue
		}

		switch t {
		case rxv1.TypeSessionHello:
			if started {
				ss.reply(env, rxv1.CodeInvalidEnvelope, "session.hello already received")

				continue
			}

			h, err := decode[media.Hello](env)
			if err != nil {
				ss.sendError(err)

				continue
			}

			hello.Stop()

			ss.mu.Lock()
			ss.hello = h
			ss.mu.Unlock()

			if o.Streams != nil {
				ss.streams = o.Streams.Open(ss)
				defer ss.streams.Close()
			}

			started = true
			cur := ss.claims()
			user := media.User{Roles: nonNil(cur.Roles)}

			if cur.Subject != token.AnonymousSubject {
				user.ID = cur.Subject
			}

			ss.send(rxv1.TypeSessionWelcome, rxv1.CorrelationID{}, media.Welcome{
				CID:    cur.ConnectionID,
				Server: media.Server{ProductVersion: o.Version, Protocol: rxv1.Subprotocol},
				User:   user,
				Limits: media.Limits{
					MaxMsgBytes: rxv1.MaxInboundTextBytes, MsgRate: defaultMsgRate, MaxDemods: cur.Limits.MaxDemods,
				},
				TokenExp: cur.ExpiresAt.Unix(), ServerTime: o.Now().UnixMilli(),
			})
		case rxv1.TypeAuthRefresh:
			ss.refresh(ctx, env, expire)
		case rxv1.TypeBye:
			ss.conn.Close(rxv1.CloseNormal, "bye")
		case rxv1.TypeTimeSync:
			ss.timeSync(env)
		case rxv1.TypeDeviceAttach:
			ss.scoped(ctx, env, token.PermListen)
		case rxv1.TypePresetSelect:
			ss.scoped(ctx, env, token.PermPreset)
		case rxv1.TypeDeviceRetune:
			ss.scoped(ctx, env, token.PermRetune)
		case rxv1.TypeDemodCreate:
			ss.scoped(ctx, env, token.PermDemod)
		case rxv1.TypeDeviceDetach, rxv1.TypeStreamConfigure, rxv1.TypeAudioConfigure, rxv1.TypeDemodSet, rxv1.TypeDemodRemove:
			ss.stream(ctx, env)
		case rxv1.TypeAck, rxv1.TypeError:
		default:
			ss.reply(env, rxv1.CodeUnsupportedType, "not implemented by this node yet")
		}
	}

	beats()
	ss.emit(rxv1.TypeConnectionClosed, ss.claims(), reason)
	ss.logger.InfoContext(ctx, "media connection closed", slog.String("reason", reason))
}

// heartbeats reports the session alive to the hub every HeartbeatInterval
// until the returned stop function is called (§7.3 rule 2: the hub reaps
// presence rows silent for presence.stale_after). Successive heartbeats of
// one session coalesce in the event buffer while the control channel is
// down.
func (ss *session) heartbeats(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)

		t := time.NewTicker(ss.s.heartbeatInterval())
		defer t.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				ss.emit(rxv1.TypeConnectionHeart, ss.claims(), "")
			}
		}
	}()

	return func() {
		cancel()
		<-done
	}
}

// refresh replaces the token of the connection (§5.8): same cid, same
// audience, new scopes.
func (ss *session) refresh(ctx context.Context, env rxv1.Envelope, expire *time.Timer) {
	p, err := decode[media.AuthRefresh](env)
	if err != nil {
		ss.sendError(err)

		return
	}

	c, err := ss.s.verify(p.Token)

	switch {
	case errors.Is(err, token.ErrExpired):
		ss.reply(env, rxv1.CodeTokenExpired, "access token expired")

		return
	case errors.Is(err, errRevoked):
		ss.conn.Close(rxv1.CloseForbidden, "revoked")

		return
	case err != nil:
		ss.logger.WarnContext(ctx, "refresh refused: invalid access token", slog.Any("error", err))
		ss.reply(env, rxv1.CodeTokenInvalid, "invalid access token")

		return
	case c.ConnectionID != ss.claims().ConnectionID:
		ss.reply(env, rxv1.CodeTokenInvalid, "access token of another connection")

		return
	case !sameHolder(ss.claims(), c):
		ss.logger.WarnContext(ctx, "refresh refused: access token of another user or session")
		ss.reply(env, rxv1.CodeTokenInvalid, "access token of another user or session")

		return
	}

	ss.mu.Lock()
	ss.cur = c
	ss.mu.Unlock()

	ss.s.extendUsed(c.ConnectionID, c.ExpiresAt)

	if ss.streams != nil {
		ss.streams.Reauthorize()
	}

	expire.Reset(ss.expiry())
	ss.logger.DebugContext(ctx, "access token refreshed", slog.Time("exp", c.ExpiresAt))
	ss.ack(env)
}

// sameHolder reports whether next may replace cur: same subject and
// session, or an anonymous connection whose visitor signed in.
func sameHolder(cur, next token.Claims) bool {
	if cur.Subject == token.AnonymousSubject {
		return true
	}

	return next.Subject == cur.Subject && next.SessionID == cur.SessionID
}

// scoped checks a device-scoped message against the token scope, then
// hands it to the stream handler.
func (ss *session) scoped(ctx context.Context, env rxv1.Envelope, perm string) {
	ref, err := decode[media.DeviceRef](env)
	if err != nil {
		ss.sendError(err)

		return
	}

	reason := ""

	switch c := ss.claims(); {
	case !c.Allows(ref.DeviceID, perm):
		reason = "the access token does not grant " + perm + " on this device"
	case c.Subject == token.AnonymousSubject && !ss.s.anonymousAllowed(ref.DeviceID):
		// SRC-023: the node re-checks the listen policy itself, so an
		// anonymous token scoped by mistake grants nothing on a device
		// that requires registration.
		reason = "this device requires a signed-in listener"
	}

	if reason != "" {
		ss.reply(env, rxv1.CodeForbidden, reason)

		// §5.9: repeated violations (more than 10 per minute) close 4403.
		if ss.forbidden.add(ss.s.o.Now()) > MaxForbiddenPerMinute {
			ss.logger.Warn("media connection closed after repeated forbidden messages")
			ss.conn.Close(rxv1.CloseForbidden, "repeated forbidden messages")
		}

		return
	}

	ss.stream(ctx, env)
}

// stream hands a device message to the stream handler.
func (ss *session) stream(ctx context.Context, env rxv1.Envelope) {
	if ss.streams == nil {
		ss.reply(env, rxv1.CodeUnsupportedType, "device streaming is not available on this node")

		return
	}

	ss.streams.Handle(ctx, env)
}

// rateLimited answers a message over the inbound rate (§6.9).
func (ss *session) rateLimited(env rxv1.Envelope, retry time.Duration) {
	ms := retry.Milliseconds()
	p := rxv1.ErrorPayload{Code: rxv1.CodeRateLimited, Message: "too many messages", Retryable: true, RetryAfterMS: &ms}

	if id, ok := env.ID(); ok {
		p.Re = &id
	}

	if e, err := rxv1.NewErrorEnvelope(time.Now().UnixMilli(), p); err == nil {
		_ = ss.conn.Send(e)
	}
}

// timeSync answers time.sync with the node clock (§6.4).
func (ss *session) timeSync(env rxv1.Envelope) {
	t1 := ss.s.o.Now().UnixMilli()

	p, err := decode[media.TimeSync](env)
	if err != nil {
		ss.sendError(err)

		return
	}

	id, _ := env.ID()
	ss.send(rxv1.TypeTimeSyncReply, id, media.TimeSyncReply{T0: p.T0, T1: t1, T2: ss.s.o.Now().UnixMilli()})
}

// Claims implements media.Peer: the token claims, without the scopes of
// the devices an anonymous token may no longer use under their effective
// listen policy (SRC-023). The stream handler checks them at every
// message and drops what they no longer allow at the next auth.refresh
// (Reauthorize), so an anonymous listener leaves a device that became
// registered within one token lifetime.
func (ss *session) Claims() token.Claims {
	c := ss.claims()
	if c.Subject != token.AnonymousSubject || ss.s.o.Policy == nil {
		return c
	}

	c.Scopes = slices.DeleteFunc(slices.Clone(c.Scopes), func(s token.Scope) bool { return !ss.s.anonymousAllowed(s.Device) })

	return c
}

// Hello implements media.Peer.
func (ss *session) Hello() media.Hello {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	return ss.hello
}

// Send implements media.Peer.
func (ss *session) Send(typ rxv1.MessageType, payload any) {
	ss.send(typ, rxv1.CorrelationID{}, payload)
}

// Ack implements media.Peer.
func (ss *session) Ack(req rxv1.Envelope, result any) {
	id, ok := req.ID()
	if !ok {
		return
	}

	if a, err := rxv1.NewAckEnvelope(time.Now().UnixMilli(), id, result); err == nil {
		_ = ss.conn.Send(a)
	}
}

// Fail implements media.Peer.
func (ss *session) Fail(req rxv1.Envelope, code rxv1.ErrorCode, reason string) {
	ss.reply(req, code, reason)
}

// RateLimited implements media.Peer.
func (ss *session) RateLimited(req rxv1.Envelope, retry time.Duration) { ss.rateLimited(req, retry) }

// Queue implements media.Peer.
func (ss *session) Queue() *sendq.Queue { return ss.queue }

// Attached implements media.Peer: the hub learns at once which device the
// connection listens to (a connection.heartbeat), then with every
// heartbeat.
func (ss *session) Attached(deviceID, demod string) {
	ss.mu.Lock()
	ss.device, ss.demod = deviceID, demod
	ss.mu.Unlock()

	ss.emit(rxv1.TypeConnectionHeart, ss.claims(), "")
}

// emit reports the connection to the hub over the control channel
// (presence, §7.3).
func (ss *session) emit(typ rxv1.MessageType, c token.Claims, reason string) {
	a := ss.s.o.Agent
	if a == nil {
		return
	}

	user := ""
	if c.Subject != token.AnonymousSubject {
		user = c.Subject
	}

	key := ""
	if typ == rxv1.TypeConnectionHeart {
		key = string(typ) + ":" + c.ConnectionID
	}

	ss.mu.Lock()
	device, demod := ss.device, ss.demod
	ss.mu.Unlock()

	a.Emit(typ, key, agent.ClassState, func(seq int64) any {
		return ctl.Connection{
			Seq: seq, CID: c.ConnectionID, SID: "", UserID: user, DeviceID: device, Demod: demod, Reason: reason,
			Since: c.IssuedAt.UnixMilli(),
		}
	})
}

func decode[T any](env rxv1.Envelope) (T, error) {
	var v T

	err := env.DecodePayload(&v, false)

	return v, err
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}

	return s
}
