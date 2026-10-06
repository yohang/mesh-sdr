package media

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/agent"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/media"
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

	hello := time.AfterFunc(HelloTimeout, func() { ss.conn.Close(rxv1.CloseHandshakeTimeout, "session.hello not received in time") })
	expire := time.AfterFunc(ss.expiry(), func() { ss.conn.Close(rxv1.CloseUnauthenticated, "token_expired") })

	defer hello.Stop()
	defer expire.Stop()

	started := false
	reason := "client"

	for {
		env, err := ss.conn.Read(ctx)
		if err != nil {
			var pe *rxv1.Error
			if errors.As(err, &pe) {
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

			if _, err := decode[media.Hello](env); err != nil {
				ss.sendError(err)

				continue
			}

			hello.Stop()

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
		case rxv1.TypeDeviceAttach:
			ss.scoped(env, token.PermListen)
		case rxv1.TypePresetSelect:
			ss.scoped(env, token.PermPreset)
		case rxv1.TypeDeviceRetune:
			ss.scoped(env, token.PermRetune)
		case rxv1.TypeAck, rxv1.TypeError:
		default:
			ss.reply(env, rxv1.CodeUnsupportedType, "not implemented by this node yet")
		}
	}

	ss.emit(rxv1.TypeConnectionClosed, ss.claims(), reason)
	ss.logger.InfoContext(ctx, "media connection closed", slog.String("reason", reason))
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
	}

	ss.mu.Lock()
	ss.cur = c
	ss.mu.Unlock()

	expire.Reset(ss.expiry())
	ss.logger.DebugContext(ctx, "access token refreshed", slog.Time("exp", c.ExpiresAt))
	ss.ack(env)
}

// scoped checks a device-scoped message against the token scope. Device
// streaming is not implemented yet: an allowed message is answered
// unsupported_type.
func (ss *session) scoped(env rxv1.Envelope, perm string) {
	ref, err := decode[media.DeviceRef](env)
	if err != nil {
		ss.sendError(err)

		return
	}

	if !ss.claims().Allows(ref.DeviceID, perm) {
		ss.reply(env, rxv1.CodeForbidden, "the access token does not grant "+perm+" on this device")

		return
	}

	ss.reply(env, rxv1.CodeUnsupportedType, "device streaming is not implemented by this node yet")
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

	a.Emit(typ, "", agent.ClassState, func(seq int64) any {
		return ctl.Connection{Seq: seq, CID: c.ConnectionID, SID: "", UserID: user, Reason: reason, Since: c.IssuedAt.UnixMilli()}
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
