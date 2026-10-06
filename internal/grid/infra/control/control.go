// Package control carries the rx-ctl.v1 control channel over mTLS
// WebSockets (TECHNICAL_SPEC §4.4, ADR 0004, ADR 0008): the hub manager
// that dials and supervises one channel per node, and the node /control
// endpoint.
package control

import (
	"context"
	"errors"
	"time"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/wsconn"
)

// Channel limits (§4.4, §4.5, §6.9).
const (
	ReadLimit      = rxv1.MaxInboundControlTextBytes
	MaxQueueBytes  = 4 << 20
	PingInterval   = 15 * time.Second
	PongTimeout    = 30 * time.Second
	HelloTimeout   = 5 * time.Second
	WelcomeTimeout = 10 * time.Second
	BatchWindow    = 250 * time.Millisecond
	BatchMax       = 500
)

// readResult is one Read outcome.
type readResult struct {
	env rxv1.Envelope
	err error
}

// reader pumps conn.Read into a channel until a transport error.
func reader(ctx context.Context, conn *wsconn.Conn) <-chan readResult {
	ch := make(chan readResult, 16)

	go func() {
		defer close(ch)

		for {
			env, err := conn.Read(ctx)

			select {
			case ch <- readResult{env: env, err: err}:
			case <-conn.Done():
				return
			}

			var pe *rxv1.Error
			if err != nil && !errors.As(err, &pe) {
				return
			}
		}
	}()

	return ch
}

// send encodes and enqueues a message.
func send(conn *wsconn.Conn, typ rxv1.MessageType, id rxv1.CorrelationID, payload any) error {
	env, err := rxv1.NewEnvelope(typ, id, time.Now().UnixMilli(), payload)
	if err != nil {
		return err
	}

	return conn.Send(env)
}

// sendError answers a malformed or unsupported message.
func sendError(conn *wsconn.Conn, err error) {
	env, eerr := rxv1.NewErrorEnvelope(time.Now().UnixMilli(), rxv1.ErrorPayloadFrom(err, ""))
	if eerr == nil {
		_ = conn.Send(env)
	}
}

// sendAck answers a request with an id.
func sendAck(conn *wsconn.Conn, env rxv1.Envelope) {
	if id, ok := env.ID(); ok {
		if ack, err := rxv1.NewAckEnvelope(time.Now().UnixMilli(), id, nil); err == nil {
			_ = conn.Send(ack)
		}
	}
}

// replyError answers a request with an error frame carrying its id.
func replyError(conn *wsconn.Conn, env rxv1.Envelope, code rxv1.ErrorCode, reason string) {
	e := &rxv1.Error{Code: code, Reason: reason}
	if id, ok := env.ID(); ok {
		e.ID = id
	}

	sendError(conn, e)
}

// decode decodes a payload, tolerating unknown fields (forward compatible
// newer-minor peers, §4.8).
func decode[T any](env rxv1.Envelope) (T, error) {
	var v T

	err := env.DecodePayload(&v, false)

	return v, err
}
