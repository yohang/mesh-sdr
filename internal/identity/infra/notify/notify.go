// Package notify renders the identity e-mails (plain text, SR-09) and
// queues them for delivery. Links come from the caller, built from hub.url.
package notify

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"net/url"
	"strings"
	"text/template"
	"time"

	"github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
	"github.com/yohang/mesh-sdr/internal/mail"
)

//go:embed templates/*.txt
var files embed.FS

var templates = template.Must(template.New("").Funcs(template.FuncMap{
	"date": func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") },
}).ParseFS(files, "templates/*.txt"))

// Outbox queues messages for delivery.
type Outbox interface {
	Enqueue(m mail.Message) error
}

// Mailer implements app.Notifier.
type Mailer struct {
	outbox Outbox // nil: mail disabled
	site   string
	hubURL string
}

var _ app.Notifier = (*Mailer)(nil)

// New returns the notifier. outbox nil means mail is not configured.
func New(outbox Outbox, hubURL string) *Mailer {
	site := hubURL
	if u, err := url.Parse(hubURL); err == nil && u.Host != "" {
		site = u.Host
	}

	return &Mailer{outbox: outbox, site: site, hubURL: strings.TrimRight(hubURL, "/")}
}

// Enabled implements app.Notifier.
func (m *Mailer) Enabled() bool { return m.outbox != nil }

type data struct {
	Site    string
	HubURL  string
	Link    string
	Role    string
	Expires time.Time
	At      time.Time
	Email   string
}

func (m *Mailer) send(to domain.Email, name, subject string, d data) error {
	if m.outbox == nil {
		return app.ErrMailDisabled
	}

	if to.IsZero() {
		return domain.ErrInvalidEmail
	}

	d.Site, d.HubURL = m.site, m.hubURL

	var b bytes.Buffer
	if err := templates.ExecuteTemplate(&b, name+".txt", d); err != nil {
		return fmt.Errorf("render %s mail: %w", name, err)
	}

	if err := m.outbox.Enqueue(mail.Message{To: to.String(), Subject: subject + " · " + m.site, Body: b.String()}); err != nil {
		return fmt.Errorf("queue %s mail: %w", name, err)
	}

	return nil
}

// Invitation implements app.Notifier.
func (m *Mailer) Invitation(_ context.Context, to domain.Email, link string, role domain.Role, expires time.Time) error {
	return m.send(to, "invitation", "Your invitation", data{Link: link, Role: role.String(), Expires: expires})
}

// PasswordReset implements app.Notifier.
func (m *Mailer) PasswordReset(_ context.Context, to domain.Email, link string, expires time.Time) error {
	return m.send(to, "password_reset", "Reset your password", data{Link: link, Expires: expires})
}

// EmailConfirmation implements app.Notifier.
func (m *Mailer) EmailConfirmation(_ context.Context, to domain.Email, link string, expires time.Time) error {
	return m.send(to, "email_confirmation", "Confirm your e-mail address", data{Link: link, Expires: expires})
}

// PasswordChanged implements app.Notifier.
func (m *Mailer) PasswordChanged(_ context.Context, to domain.Email, at time.Time) error {
	return m.send(to, "password_changed", "Your password was changed", data{At: at})
}

// EmailChanged implements app.Notifier.
func (m *Mailer) EmailChanged(_ context.Context, to, next domain.Email, at time.Time) error {
	return m.send(to, "email_changed", "Your e-mail address was changed", data{At: at, Email: next.String()})
}

// Test implements app.Notifier.
func (m *Mailer) Test(_ context.Context, to domain.Email) error {
	return m.send(to, "test", "Test message", data{At: time.Now()})
}
