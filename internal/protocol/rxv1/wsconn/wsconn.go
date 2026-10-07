// Package wsconn adapts github.com/coder/websocket to the rx.v1 and rx-ctl.v1
// channels (ADR 0004, ADR 0008): the 426 subprotocol pre-check, envelope
// reads, a bounded outbound queue written by one goroutine, and WS pings.
//
// coder/websocket closes the connection when a read or write context
// expires. The writer therefore uses the connection context, never a
// per-write timeout, and the queue bound replaces write deadlines: a send
// that would exceed it closes the connection with 4413.
package wsconn

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/sendq"
)

// Errors returned by Conn.
var (
	// ErrClosed is returned by Send and Read once the connection is closed.
	ErrClosed = errors.New("wsconn: connection closed")
	// ErrSlowConsumer is returned by Send when the queue bound is exceeded;
	// the connection is closed with 4413.
	ErrSlowConsumer = errors.New("wsconn: slow consumer")
	// ErrBinaryFrame is returned by Read on a binary frame; the connection
	// is closed with 1003.
	ErrBinaryFrame = errors.New("wsconn: unexpected binary frame")
)

// Accept upgrades r when it offers subprotocol. Without it, it answers 426
// (§6.1) and returns an error. Compression is disabled.
func Accept(w http.ResponseWriter, r *http.Request, subprotocol string) (*websocket.Conn, error) {
	return accept(w, r, subprotocol, false)
}

// AcceptOriginChecked is Accept for a caller that has checked the Origin
// itself against its own allow-list: the library's same-host Origin check is
// skipped (behind the gateway, the Host is not the browser's).
func AcceptOriginChecked(w http.ResponseWriter, r *http.Request, subprotocol string) (*websocket.Conn, error) {
	return accept(w, r, subprotocol, true)
}

func accept(w http.ResponseWriter, r *http.Request, subprotocol string, originChecked bool) (*websocket.Conn, error) {
	if !offers(r, subprotocol) {
		w.Header().Set("Sec-WebSocket-Protocol", subprotocol)
		http.Error(w, "subprotocol "+subprotocol+" required", http.StatusUpgradeRequired)

		return nil, fmt.Errorf("wsconn: subprotocol %s not offered", subprotocol)
	}

	// The server's ReadTimeout deadline would survive the hijack and cut
	// the long-lived connection: clear it (best effort).
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:       []string{subprotocol},
		CompressionMode:    websocket.CompressionDisabled,
		InsecureSkipVerify: originChecked,
	})
	if err != nil {
		return nil, fmt.Errorf("wsconn: accept: %w", err)
	}

	return ws, nil
}

func offers(r *http.Request, subprotocol string) bool {
	for _, h := range r.Header.Values("Sec-WebSocket-Protocol") {
		for p := range strings.SplitSeq(h, ",") {
			if strings.TrimSpace(p) == subprotocol {
				return true
			}
		}
	}

	return false
}

// Dial opens a WebSocket to url with subprotocol through client and checks
// that the server selected it. Compression is disabled.
//
// Redirects are never followed: the peer is the one at url.
func Dial(ctx context.Context, url string, client *http.Client, subprotocol string) (*websocket.Conn, error) {
	c := http.Client{}
	if client != nil {
		c = *client
	}

	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	ws, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPClient:      &c,
		Subprotocols:    []string{subprotocol},
		CompressionMode: websocket.CompressionDisabled,
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}

	if err != nil {
		if resp != nil {
			return nil, &DialError{Status: resp.StatusCode, Err: err}
		}

		return nil, fmt.Errorf("wsconn: dial %s: %w", url, err)
	}

	if ws.Subprotocol() != subprotocol {
		_ = ws.Close(websocket.StatusPolicyViolation, "subprotocol required")

		return nil, &DialError{Status: http.StatusUpgradeRequired, Err: fmt.Errorf("server selected subprotocol %q", ws.Subprotocol())}
	}

	return ws, nil
}

// DialError is a handshake answered with an HTTP status.
type DialError struct {
	Status int
	Err    error
}

func (e *DialError) Error() string {
	return fmt.Sprintf("wsconn: handshake: HTTP %d: %v", e.Status, e.Err)
}

func (e *DialError) Unwrap() error { return e.Err }

// Options configures a Conn.
type Options struct {
	// ReadLimit is the inbound message limit; above it the peer is closed
	// with 1009.
	ReadLimit int64
	// MaxQueueBytes bounds the pending outbound bytes. Exceeding it closes
	// the connection with 4413.
	MaxQueueBytes int
	// PingInterval is the WS ping period; zero disables pings.
	PingInterval time.Duration
	// PongTimeout closes the connection when a pong does not arrive in time.
	PongTimeout time.Duration
	// Queue, when set, replaces the JSON queue: the §6.8 media send queue
	// (JSON, audio, meters, FFT). MaxQueueBytes is then unused; a queue
	// failure closes the connection with 4413.
	Queue *sendq.Queue
}

// Conn is a WebSocket carrying rx.v1 envelopes.
type Conn struct {
	ws   *websocket.Conn
	opts Options

	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	queue   [][]byte
	pending int
	wake    chan struct{}

	closed    chan struct{}
	finished  chan struct{}
	closeOnce sync.Once
	closeErr  error
}

// FlushTimeout bounds the flush of the queued messages before a local Close
// sends its close frame.
const FlushTimeout = time.Second

// New wraps ws and starts its writer and pinger goroutines. The connection
// lives until Close, a transport error or ctx is done.
func New(ctx context.Context, ws *websocket.Conn, opts Options) *Conn {
	if opts.ReadLimit > 0 {
		ws.SetReadLimit(opts.ReadLimit)
	}

	if opts.MaxQueueBytes <= 0 {
		opts.MaxQueueBytes = 1 << 20
	}

	cctx, cancel := context.WithCancel(ctx)
	c := &Conn{
		ws:       ws,
		opts:     opts,
		ctx:      cctx,
		cancel:   cancel,
		wake:     make(chan struct{}, 1),
		closed:   make(chan struct{}),
		finished: make(chan struct{}),
	}

	go c.writeLoop()

	if opts.PingInterval > 0 {
		go c.pingLoop()
	}

	go func() {
		select {
		case <-cctx.Done():
			c.Close(rxv1.CloseGoingAway, "shutting down")
		case <-c.closed:
		}
	}()

	return c
}

// Send enqueues env. It never blocks.
func (c *Conn) Send(env rxv1.Envelope) error {
	b, err := env.MarshalJSON()
	if err != nil {
		return fmt.Errorf("wsconn: encode %s: %w", env.Type(), err)
	}

	select {
	case <-c.closed:
		return ErrClosed
	default:
	}

	if q := c.opts.Queue; q != nil {
		if err := q.PushJSON(b); err != nil {
			c.closeWith(rxv1.CloseSlowConsumer, "slow consumer", ErrSlowConsumer)

			return ErrSlowConsumer
		}

		return nil
	}

	c.mu.Lock()
	if c.pending+len(b) > c.opts.MaxQueueBytes {
		c.mu.Unlock()
		c.closeWith(rxv1.CloseSlowConsumer, "slow consumer", ErrSlowConsumer)

		return ErrSlowConsumer
	}

	c.queue = append(c.queue, b)
	c.pending += len(b)
	c.mu.Unlock()

	select {
	case c.wake <- struct{}{}:
	default:
	}

	return nil
}

// Pending returns the number of queued outbound bytes.
func (c *Conn) Pending() int {
	if q := c.opts.Queue; q != nil {
		return q.Pending()
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	return c.pending
}

func (c *Conn) writeLoop() {
	if c.opts.Queue != nil {
		c.queueLoop()

		return
	}

	for {
		select {
		case <-c.closed:
			return
		case <-c.wake:
		}

		for {
			c.mu.Lock()
			if len(c.queue) == 0 {
				c.mu.Unlock()

				break
			}

			b := c.queue[0]
			c.queue[0] = nil
			c.queue = c.queue[1:]
			c.mu.Unlock()

			err := c.ws.Write(c.ctx, websocket.MessageText, b)

			c.mu.Lock()
			c.pending -= len(b)
			c.mu.Unlock()

			if err != nil {
				c.closeWith(rxv1.CloseGoingAway, "write failed", fmt.Errorf("wsconn: write: %w", err))

				return
			}
		}
	}
}

// queueLoop writes the items of the media send queue in its priority order.
func (c *Conn) queueLoop() {
	q := c.opts.Queue

	for {
		select {
		case <-c.closed:
			return
		case <-q.Ready():
		}

		if q.Err() != nil {
			c.closeWith(rxv1.CloseSlowConsumer, "slow consumer", ErrSlowConsumer)

			return
		}

		for {
			it, ok := q.Pop()
			if !ok {
				break
			}

			if err := c.writeItem(c.ctx, it); err != nil {
				c.closeWith(rxv1.CloseGoingAway, "write failed", fmt.Errorf("wsconn: write: %w", err))

				return
			}
		}
	}
}

// writeItem writes one queue item; a binary frame is its header followed
// by its shared payload, in one message and without copying the payload.
func (c *Conn) writeItem(ctx context.Context, it sendq.Item) error {
	if !it.Binary() {
		return c.ws.Write(ctx, websocket.MessageText, it.Text)
	}

	w, err := c.ws.Writer(ctx, websocket.MessageBinary)
	if err != nil {
		return err
	}

	if _, err := w.Write(it.Header); err != nil {
		_ = w.Close()

		return err
	}

	if _, err := w.Write(it.Payload); err != nil {
		_ = w.Close()

		return err
	}

	return w.Close()
}

func (c *Conn) pingLoop() {
	t := time.NewTicker(c.opts.PingInterval)
	defer t.Stop()

	for {
		select {
		case <-c.closed:
			return
		case <-t.C:
		}

		timeout := c.opts.PongTimeout
		if timeout <= 0 {
			timeout = 2 * c.opts.PingInterval
		}

		pctx, cancel := context.WithTimeout(c.ctx, timeout)
		err := c.ws.Ping(pctx)

		cancel()

		if err != nil {
			c.closeWith(rxv1.CloseGoingAway, "pong timeout", fmt.Errorf("wsconn: ping: %w", err))

			return
		}
	}
}

// RTT sends a WS ping and returns the time to its pong. A concurrent Read
// loop must be running (coder/websocket reads pongs in Read).
func (c *Conn) RTT(ctx context.Context) (time.Duration, error) {
	start := time.Now()

	if err := c.ws.Ping(ctx); err != nil {
		return 0, fmt.Errorf("wsconn: ping: %w", err)
	}

	return time.Since(start), nil
}

// Read returns the next envelope. A malformed text frame returns an
// *rxv1.Error (the connection stays open, the caller answers with an error
// frame); a transport failure returns another error and the connection is
// closed.
func (c *Conn) Read(ctx context.Context) (rxv1.Envelope, error) {
	typ, b, err := c.ws.Read(ctx)
	if err != nil {
		c.closeWith(rxv1.CloseGoingAway, "read failed", err)

		return rxv1.Envelope{}, fmt.Errorf("wsconn: read: %w", err)
	}

	if typ != websocket.MessageText {
		c.closeWith(rxv1.CloseUnsupportedData, "binary frames are not accepted", ErrBinaryFrame)

		return rxv1.Envelope{}, ErrBinaryFrame
	}

	return rxv1.DecodeEnvelope(b)
}

// Close sends the queued messages (within FlushTimeout), then a close frame
// with code and reason, and tears the connection down. It is safe to call
// several times and from any goroutine.
func (c *Conn) Close(code rxv1.CloseCode, reason string) {
	c.closeWith(code, reason, nil)
}

func (c *Conn) closeWith(code rxv1.CloseCode, reason string, err error) {
	c.closeOnce.Do(func() {
		c.closeErr = err
		close(c.closed)

		go func() {
			defer close(c.finished)

			if err == nil {
				c.flush()
			}

			_ = c.ws.Close(websocket.StatusCode(code), reason)
			c.cancel()
		}()
	})
}

// flush writes what is still queued, best effort.
func (c *Conn) flush() {
	ctx, cancel := context.WithTimeout(c.ctx, FlushTimeout)
	defer cancel()

	if q := c.opts.Queue; q != nil {
		for {
			it, ok := q.Pop()
			if !ok || c.writeItem(ctx, it) != nil {
				return
			}
		}
	}

	for {
		c.mu.Lock()
		if len(c.queue) == 0 {
			c.mu.Unlock()

			return
		}

		b := c.queue[0]
		c.queue[0] = nil
		c.queue = c.queue[1:]
		c.pending -= len(b)
		c.mu.Unlock()

		if c.ws.Write(ctx, websocket.MessageText, b) != nil {
			return
		}
	}
}

// Done is closed when the connection is closed.
func (c *Conn) Done() <-chan struct{} { return c.closed }

// Finished is closed when the close handshake is over (or failed) and the
// connection is torn down.
func (c *Conn) Finished() <-chan struct{} { return c.finished }

// Err returns why the connection closed, nil for a local Close.
func (c *Conn) Err() error {
	select {
	case <-c.closed:
		return c.closeErr
	default:
		return nil
	}
}

// CloseStatus returns the close code carried by a read error, or 0.
func CloseStatus(err error) rxv1.CloseCode {
	s := websocket.CloseStatus(err)
	if s < 0 {
		return 0
	}

	return rxv1.CloseCode(s)
}
