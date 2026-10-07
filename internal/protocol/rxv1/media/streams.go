package media

import (
	"context"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/sendq"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/token"
)

// Peer is one media connection as seen by the device stream handler: the
// WebSocket endpoint authenticates the connection and runs the session;
// the handler serves its devices, streams and demodulators.
type Peer interface {
	// Claims returns the current access token claims (scope, limits).
	Claims() token.Claims
	// Hello returns the client's session.hello.
	Hello() Hello
	// Send queues a node → client message.
	Send(typ rxv1.MessageType, payload any)
	// Ack answers a request.
	Ack(req rxv1.Envelope, result any)
	// Fail answers a request with an error frame.
	Fail(req rxv1.Envelope, code rxv1.ErrorCode, reason string)
	// Queue is the connection's send queue for binary frames and meters.
	Queue() *sendq.Queue
}

// Streams opens the stream handler of a media connection.
type Streams interface {
	Open(p Peer) StreamSession
}

// StreamSession handles the device messages of one connection: device.*,
// stream.configure, audio.configure and demod.* (§6.4). Handle runs on the
// connection's read loop; Close releases everything at the end.
type StreamSession interface {
	Handle(ctx context.Context, req rxv1.Envelope)
	// Reauthorize applies the claims of a refreshed token: what they no
	// longer allow is detached or removed.
	Reauthorize()
	Close()
}
