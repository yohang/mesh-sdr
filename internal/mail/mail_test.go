package mail_test

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/mail"
)

// relay is a minimal SMTP server without TLS: it records the DATA of each
// message.
type relay struct {
	ln       net.Listener
	mu       sync.Mutex
	messages []string
}

func newRelay(t *testing.T) *relay {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	r := &relay{ln: ln}

	t.Cleanup(func() { _ = ln.Close() })

	go r.serve()

	return r
}

func (r *relay) serve() {
	for {
		c, err := r.ln.Accept()
		if err != nil {
			return
		}

		go r.session(c)
	}
}

func (r *relay) session(c net.Conn) {
	defer func() { _ = c.Close() }()

	tp := textproto.NewConn(c)
	_ = tp.PrintfLine("220 relay ready")

	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}

		switch cmd := strings.ToUpper(strings.Fields(line + " x")[0]); cmd {
		case "EHLO", "HELO":
			_ = tp.PrintfLine("250-relay")
			_ = tp.PrintfLine("250 8BITMIME")
		case "DATA":
			_ = tp.PrintfLine("354 go ahead")

			b, err := tp.ReadDotBytes()
			if err != nil {
				return
			}

			r.mu.Lock()
			r.messages = append(r.messages, string(b))
			r.mu.Unlock()

			_ = tp.PrintfLine("250 queued")
		case "QUIT":
			_ = tp.PrintfLine("221 bye")

			return
		default:
			_ = tp.PrintfLine("250 ok")
		}
	}
}

func (r *relay) port() int { return r.ln.Addr().(*net.TCPAddr).Port }

func (r *relay) received() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.messages...)
}

func TestSMTPPlainRelay(t *testing.T) {
	r := newRelay(t)
	s := mail.NewSMTP(mail.SMTPConfig{Host: "127.0.0.1", Port: r.port(), TLS: mail.TLSNone, From: "WebSDR <sdr@example.org>", Timeout: 5 * time.Second})

	if err := s.Send(context.Background(), mail.Message{To: "alice@example.org", Subject: "Hello", Body: "Line one\nhttps://hub.example/x"}); err != nil {
		t.Fatal(err)
	}

	got := r.received()
	if len(got) != 1 || !strings.Contains(got[0], "Subject: Hello") || !strings.Contains(got[0], "alice@example.org") ||
		!strings.Contains(got[0], "https://hub.example/x") || !strings.Contains(got[0], "text/plain") {
		t.Errorf("received = %q", got)
	}
}

func TestSMTPRequiresTLS(t *testing.T) {
	r := newRelay(t)
	s := mail.NewSMTP(mail.SMTPConfig{Host: "127.0.0.1", Port: r.port(), TLS: mail.TLSStartTLS, From: "sdr@example.org", Timeout: 5 * time.Second})

	if err := s.Send(context.Background(), mail.Message{To: "a@example.org", Subject: "x", Body: "x"}); err == nil {
		t.Error("a relay without STARTTLS was used")
	}

	if len(r.received()) != 0 {
		t.Error("message sent in clear")
	}
}

type flaky struct {
	mu    sync.Mutex
	fails int
	sent  []mail.Message
}

func (f *flaky) Send(_ context.Context, m mail.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.fails > 0 {
		f.fails--

		return errors.New("relay down")
	}

	f.sent = append(f.sent, m)

	return nil
}

func (f *flaky) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.sent)
}

func TestQueueRetriesAndNeverLogsBodies(t *testing.T) {
	var logs strings.Builder

	var mu sync.Mutex

	logger := slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()

		return logs.Write(p)
	}), nil))

	f := &flaky{fails: 2}
	q := mail.NewQueue(f, logger).WithBackoff(time.Millisecond, time.Millisecond, time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() { q.Run(ctx); close(done) }()

	if err := q.Enqueue(mail.Message{To: "a@example.org", Subject: "s", Body: "secret-link"}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for f.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	<-done

	mu.Lock()
	out := logs.String()
	mu.Unlock()

	if f.count() != 1 || strings.Contains(out, "secret-link") || strings.Contains(out, "a@example.org") || !strings.Contains(out, "mail sent") {
		t.Errorf("sent %d, logs:\n%s", f.count(), out)
	}
}

func TestQueueIsBounded(t *testing.T) {
	q := mail.NewQueue(&flaky{}, slog.New(slog.DiscardHandler))

	for range mail.QueueCapacity {
		if err := q.Enqueue(mail.Message{To: "a@example.org"}); err != nil {
			t.Fatal(err)
		}
	}

	if err := q.Enqueue(mail.Message{To: "a@example.org"}); !errors.Is(err, mail.ErrQueueFull) {
		t.Errorf("overflow: %v", err)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
