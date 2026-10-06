// Package mail sends the hub's outgoing e-mail (TECHNICAL_SPEC §5.3, SR-09):
// plain-text messages through the configured SMTP relay, with TLS and a
// verified certificate unless explicitly allowed off. Messages go through a
// bounded in-memory queue with retries, so requests never wait for the
// relay; a message lost on restart is recovered by asking again (the links
// it carries are never stored in clear).
package mail

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	gomail "github.com/wneessen/go-mail"
)

// Message is a plain-text e-mail. Body may carry single-use links: it is
// never logged.
type Message struct {
	To      string
	Subject string
	Body    string
}

// Sender delivers a message now.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// TLSMode is the transport security to the relay.
type TLSMode string

// TLS modes (smtp.tls).
const (
	TLSStartTLS TLSMode = "starttls"
	TLSImplicit TLSMode = "implicit"
	TLSNone     TLSMode = "none"
)

// SMTPConfig is the relay configuration.
type SMTPConfig struct {
	Host     string
	Port     int
	TLS      TLSMode
	Username string
	Password string
	From     string
	Timeout  time.Duration
}

// SMTP sends through a relay with go-mail, one connection per message.
type SMTP struct{ cfg SMTPConfig }

// NewSMTP returns the sender.
func NewSMTP(cfg SMTPConfig) *SMTP {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}

	return &SMTP{cfg: cfg}
}

// Send implements Sender.
func (s *SMTP) Send(ctx context.Context, m Message) error {
	msg := gomail.NewMsg()
	if err := msg.From(s.cfg.From); err != nil {
		return fmt.Errorf("sender address: %w", err)
	}

	if err := msg.To(m.To); err != nil {
		return fmt.Errorf("recipient address: %w", err)
	}

	msg.Subject(m.Subject)
	msg.SetDate()
	msg.SetMessageID()
	msg.SetBodyString(gomail.TypeTextPlain, m.Body)

	opts := []gomail.Option{gomail.WithPort(s.cfg.Port), gomail.WithTimeout(s.cfg.Timeout)}

	switch s.cfg.TLS {
	case TLSImplicit:
		opts = append(opts, gomail.WithSSL())
	case TLSNone:
		opts = append(opts, gomail.WithTLSPolicy(gomail.NoTLS))
	default:
		opts = append(opts, gomail.WithTLSPolicy(gomail.TLSMandatory))
	}

	if s.cfg.Username != "" {
		auth := gomail.SMTPAuthAutoDiscover
		if s.cfg.TLS == TLSNone {
			auth = gomail.SMTPAuthPlainNoEnc
		}

		opts = append(opts, gomail.WithSMTPAuth(auth), gomail.WithUsername(s.cfg.Username), gomail.WithPassword(s.cfg.Password))
	}

	c, err := gomail.NewClient(s.cfg.Host, opts...)
	if err != nil {
		return fmt.Errorf("mail client: %w", err)
	}

	if err := c.DialAndSendWithContext(ctx, msg); err != nil {
		return fmt.Errorf("send mail through %s: %w", s.cfg.Host, err)
	}

	return nil
}

// ErrQueueFull means the queue holds too many messages.
var ErrQueueFull = errors.New("mail queue full")

// Queue delivers messages in the background, in order, with retries.
type Queue struct {
	sender  Sender
	ch      chan Message
	backoff []time.Duration
	logger  *slog.Logger
}

// QueueCapacity bounds the number of waiting messages.
const QueueCapacity = 256

// NewQueue returns a queue. Run delivers its messages.
func NewQueue(sender Sender, logger *slog.Logger) *Queue {
	return &Queue{
		sender: sender, ch: make(chan Message, QueueCapacity),
		backoff: []time.Duration{2 * time.Second, 10 * time.Second, time.Minute}, logger: logger,
	}
}

// WithBackoff sets the waits between attempts (tests).
func (q *Queue) WithBackoff(waits ...time.Duration) *Queue {
	q.backoff = waits

	return q
}

// Enqueue queues m; it returns ErrQueueFull rather than blocking.
func (q *Queue) Enqueue(m Message) error {
	select {
	case q.ch <- m:
		return nil
	default:
		return ErrQueueFull
	}
}

// Run delivers queued messages until ctx is done. A message that still
// fails after the retries is dropped and logged at Error.
func (q *Queue) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			if n := len(q.ch); n > 0 {
				q.logger.WarnContext(ctx, "mail queue stopped with undelivered messages", slog.Int("messages", n))
			}

			return
		case m := <-q.ch:
			q.deliver(ctx, m)
		}
	}
}

func (q *Queue) deliver(ctx context.Context, m Message) {
	domain := recipientDomain(m.To)

	for attempt := 0; ; attempt++ {
		sendCtx, cancel := context.WithTimeout(ctx, time.Minute)
		err := q.sender.Send(sendCtx, m)

		cancel()

		if err == nil {
			q.logger.InfoContext(ctx, "mail sent", slog.String("recipient_domain", domain), slog.Int("attempts", attempt+1))

			return
		}

		if attempt >= len(q.backoff) || ctx.Err() != nil {
			q.logger.ErrorContext(ctx, "mail not delivered", slog.String("recipient_domain", domain),
				slog.Int("attempts", attempt+1), slog.Any("error", err))

			return
		}

		q.logger.WarnContext(ctx, "mail delivery failed, retrying", slog.String("recipient_domain", domain),
			slog.Int("attempt", attempt+1), slog.Any("error", err))

		select {
		case <-ctx.Done():
			return
		case <-time.After(q.backoff[attempt]):
		}
	}
}

// recipientDomain is what logs say about a recipient: the address is
// personal data.
func recipientDomain(addr string) string {
	if _, d, ok := strings.Cut(addr, "@"); ok {
		return d
	}

	return ""
}
