// Package identity wires the identity module (users, sessions, CSRF,
// authorisation, audit log) for the composition root.
package identity

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/identity/app"
	identityhttp "github.com/yohang/mesh-sdr/internal/identity/http"
	"github.com/yohang/mesh-sdr/internal/identity/infra/argon2"
	"github.com/yohang/mesh-sdr/internal/identity/infra/commonpw"
	"github.com/yohang/mesh-sdr/internal/identity/infra/keyring"
	"github.com/yohang/mesh-sdr/internal/identity/infra/memory"
	"github.com/yohang/mesh-sdr/internal/identity/infra/notify"
	"github.com/yohang/mesh-sdr/internal/identity/infra/settings"
	"github.com/yohang/mesh-sdr/internal/identity/infra/settingsrc"
	"github.com/yohang/mesh-sdr/internal/identity/infra/sqlite"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Login rate limit per client address (TECHNICAL_SPEC §5.12): 5 per minute.
// Password hashing runs one hash per CPU, with at most hashQueuePerCPU
// requests per CPU waiting; beyond, logins answer 429.
const (
	loginIPBurst    = 5
	loginIPEvery    = 12 * time.Second
	hashQueuePerCPU = 4
	// First-admin setup requests per client address: 10, then 1 per minute.
	setupIPBurst = 10
	setupIPEvery = time.Minute
	// Invitation checks and acceptances per client address: 10, then 1
	// every 6 minutes (§5.12 sets 3/h; acceptance errors such as a taken
	// username need retries).
	inviteIPBurst = 10
	inviteIPEvery = 6 * time.Minute
	// Password reset requests per client address and per account: 3 per
	// hour (§5.12).
	resetIPBurst = 3
	resetIPEvery = 20 * time.Minute
	// Access token mints per session or connection: 30 per minute (§5.12).
	tokenBurst = 30
	tokenEvery = 2 * time.Second
)

// Deps are the dependencies of the identity module.
type Deps struct {
	Config config.Hub
	Logger *slog.Logger
	DB     db.Adapter
	IDs    *shared.UUIDv7Generator
	Now    func() time.Time
	// Settings reads the identity policies from the settings store. Nil
	// means the built-in defaults (CLI commands).
	Settings settingsrc.Values
	// AcceptsMultipart reports API upload operations (api.AcceptsMultipart).
	AcceptsMultipart func(r *http.Request) bool
	// Mail queues outgoing e-mail; nil when smtp.host is not set.
	Mail notify.Outbox
	// Devices lists the devices of a node, to scope access tokens; nil
	// means no device (no token).
	Devices app.DeviceAccess
	// Binder checks media connection ids (GRID-011); optional.
	Binder app.ConnectionBinder
}

// noDevices is the device access without a grid.
type noDevices struct{}

func (noDevices) NodeDevices(context.Context, string) ([]app.NodeDevice, error) { return nil, nil }

// Keyring opens the token signing keyring of auth.token_key_dir.
func Keyring(cfg config.Hub, now time.Time) (*keyring.Keyring, error) {
	return keyring.Open(keyring.Options{
		Dir: cfg.Auth.TokenKeyDir, Rotation: time.Duration(cfg.Auth.KeyRotationDays) * 24 * time.Hour,
		TokenTTL: cfg.Auth.TokenTTL.Duration(),
	}, now)
}

// KeyMaintenanceEvery is how often the hub reloads and rotates its keys.
const KeyMaintenanceEvery = time.Minute

func component(l *slog.Logger, name string) *slog.Logger {
	return l.With(slog.String("component", name))
}

type repos struct {
	users        *sqlite.Users
	sessions     *sqlite.Sessions
	audit        *sqlite.AuditLog
	invitations  *sqlite.Invitations
	resets       *sqlite.PasswordResets
	emailChanges *sqlite.EmailChanges
	hasher       *argon2.Hasher
}

func newRepos(d Deps) repos {
	a := d.Config.Auth.Argon2

	return repos{
		users:        sqlite.NewUsers(d.DB, d.IDs),
		sessions:     sqlite.NewSessions(d.DB),
		audit:        sqlite.NewAuditLog(d.DB),
		invitations:  sqlite.NewInvitations(d.DB),
		resets:       sqlite.NewPasswordResets(d.DB),
		emailChanges: sqlite.NewEmailChanges(d.DB),
		hasher: argon2.New(argon2.Params{MemoryKiB: a.MemoryKiB, Iterations: a.Iterations, Parallelism: a.Parallelism},
			runtime.GOMAXPROCS(0), hashQueuePerCPU*runtime.GOMAXPROCS(0)),
	}
}

// commonPasswords is the embedded common-password list, decoded once. A
// decoding failure is a build defect (covered by the commonpw tests).
var commonPasswords = sync.OnceValue(func() *commonpw.List {
	l, err := commonpw.Load()
	if err != nil {
		panic(err)
	}

	return l
})

// policies returns the password policy source: the settings (the settings
// store in the hub, the defaults for CLI commands without one) and the
// bundled common-password list.
func policies(s app.Settings) app.Policies {
	if s == nil {
		s = settings.Defaults{}
	}

	return app.NewPolicies(s, commonPasswords())
}

// UserAdmin builds the user administration service used by the CLI.
func UserAdmin(d Deps) *app.UserAdmin {
	r := newRepos(d)

	return app.NewUserAdmin(app.UserAdminDeps{
		Invitations: r.invitations,
		Users:       r.users, Sessions: r.sessions, Audit: r.audit, Tx: d.DB, Hasher: r.hasher, IDs: d.IDs,
		Now: d.Now, Policy: policies(nil), Logger: component(d.Logger, "identity.app.users"),
	})
}

// Module is the wired identity module of the hub.
type Module struct {
	// Keys is the token signing keyring; KeySource of the grid.
	Keys        *keyring.Keyring
	Tokens      *app.Tokens
	Audit       *app.AuditView
	Resets      *app.Resets
	Invitations *app.Invitations
	Profile     *app.Profile
	Accounts    *app.Accounts
	Notifier    app.Notifier
	Auth        *app.Auth
	Setup       *app.Setup
	// Reaper (sessions.reap) and AuditPurger (audit.purge) are jobs run by
	// the hub's jobs scheduler.
	Reaper      *app.SessionReaper
	AuditPurger *app.AuditPurger
	// Purges delete ended invitations and one-time tokens.
	Purges []*app.PurgeJob
	HTTP   *identityhttp.Module
}

// Wire builds the identity module. pages renders the login page in the
// app shell.
func Wire(ctx context.Context, d Deps, pages identityhttp.Pages) (*Module, error) {
	r := newRepos(d)

	local, err := app.NewLocalProvider(ctx, r.users, d.DB, r.hasher, d.Now, component(d.Logger, "identity.app.local"))
	if err != nil {
		return nil, err
	}

	var (
		lifetimes app.SessionPolicies = app.DefaultSessionPolicies()
		retention app.Retention       = app.FixedRetention{Sessions: app.DefaultSessionRetention, Audit: 365 * 24 * time.Hour}
		passwords                     = policies(nil)
		values    app.Settings        = settings.Defaults{}
		limiter   *memory.IPLimiter
	)

	if d.Settings != nil {
		p := settingsrc.New(d.Settings)
		lifetimes, retention, passwords, values = p, p, policies(p), p
		limiter = memory.NewDynamicIPLimiter(p.LoginRate, memory.DefaultCapacity)
	} else {
		limiter = memory.NewIPLimiter(loginIPEvery, loginIPBurst, memory.DefaultCapacity)
	}

	auth := app.NewAuth(app.AuthDeps{
		Users: r.users, Sessions: r.sessions, Audit: r.audit, Tx: d.DB, IDs: d.IDs, Now: d.Now, Provider: local,
		IPLimiter:       limiter,
		Unknown:         memory.NewThrottle(memory.DefaultCapacity),
		Refusals:        memory.NewRefusalGate(memory.DefaultCapacity),
		SessionPolicies: lifetimes,
		Logger:          component(d.Logger, "identity.app.auth"),
	})

	notifier := notify.New(d.Mail, d.Config.Hub.URL)

	changer := app.NewPasswords(app.PasswordsDeps{
		Users: r.users, Sessions: r.sessions, Audit: r.audit, Tx: d.DB, Hasher: r.hasher, IDs: d.IDs, Now: d.Now,
		Policies: passwords, SessionPolicies: lifetimes,
		Notifier: notifier, Logger: component(d.Logger, "identity.app.passwords"),
	})

	setup := app.NewSetup(app.SetupDeps{
		Users: r.users, Audit: r.audit, Tx: d.DB, Hasher: r.hasher, IDs: d.IDs, Now: d.Now, Policies: passwords,
		Auth: auth, Limiter: memory.NewIPLimiter(setupIPEvery, setupIPBurst, memory.DefaultCapacity),
		HubURL: d.Config.Hub.URL, Logger: component(d.Logger, "identity.app.setup"),
	})

	accounts := app.NewAccounts(app.AccountsDeps{
		Hasher: r.hasher, Policies: passwords,
		Users: r.users, Sessions: r.sessions, Invitations: r.invitations, Audit: r.audit, Tx: d.DB, Now: d.Now, Passwords: changer,
		Logger: component(d.Logger, "identity.app.accounts"),
	})

	profile := app.NewProfile(app.ProfileDeps{
		Users: r.users, Tokens: r.emailChanges, Audit: r.audit, Tx: d.DB, IDs: d.IDs, Now: d.Now, Passwords: changer,
		Notifier: notifier, Links: app.NewLinks(d.Config.Hub.URL), Logger: component(d.Logger, "identity.app.profile"),
	})

	invitations := app.NewInvitations(app.InvitationsDeps{
		Invitations: r.invitations, Users: r.users, Audit: r.audit, Tx: d.DB, Hasher: r.hasher, IDs: d.IDs, Now: d.Now,
		Settings: values, Policies: passwords, Auth: auth, Notifier: notifier, Links: app.NewLinks(d.Config.Hub.URL),
		Limiter: memory.NewIPLimiter(inviteIPEvery, inviteIPBurst, memory.DefaultCapacity),
		Logger:  component(d.Logger, "identity.app.invitations"),
	})

	resets := app.NewResets(app.ResetsDeps{
		Tokens: r.resets, Users: r.users, Sessions: r.sessions, Audit: r.audit, Tx: d.DB, Hasher: r.hasher, IDs: d.IDs,
		Now: d.Now, Settings: values, Policies: passwords, Notifier: notifier, Links: app.NewLinks(d.Config.Hub.URL),
		Requests: memory.NewIPLimiter(resetIPEvery, resetIPBurst, memory.DefaultCapacity),
		Accounts: memory.NewKeyLimiter(resetIPEvery, resetIPBurst, memory.DefaultCapacity),
		Confirms: memory.NewIPLimiter(inviteIPEvery, inviteIPBurst, memory.DefaultCapacity),
		Logger:   component(d.Logger, "identity.app.resets"),
	})

	auditView := app.NewAuditView(r.audit, r.users)

	keys, err := Keyring(d.Config, d.Now())
	if err != nil {
		return nil, fmt.Errorf("token keys: %w", err)
	}

	devices := d.Devices
	if devices == nil {
		devices = noDevices{}
	}

	tokens := app.NewTokens(app.TokensDeps{
		Signer: keys, Devices: devices, Binder: d.Binder, Settings: settings.Defaults{},
		Limiter: memory.NewKeyLimiter(tokenEvery, tokenBurst, memory.DefaultCapacity),
		Issuer:  d.Config.Hub.URL, TTL: d.Config.Auth.TokenTTL.Duration(), Now: d.Now,
		Logger: component(d.Logger, "identity.app.tokens"),
	})

	h, err := identityhttp.New(identityhttp.Services{
		Keys:        keys,
		Audit:       auditView,
		Resets:      resets,
		Invitations: invitations,
		Auth:        auth, Passwords: changer, Setup: setup, Profile: profile, Accounts: accounts,
	}, pages, identityhttp.Config{
		HubURL:           d.Config.Hub.URL,
		TrustedProxies:   config.Prefixes(d.Config.HTTP.TrustedProxies),
		AdminNetworks:    config.Prefixes(d.Config.Admin.AllowedNetworks),
		AcceptsMultipart: d.AcceptsMultipart,
	}, component(d.Logger, "identity.http"))
	if err != nil {
		return nil, fmt.Errorf("identity http: %w", err)
	}

	return &Module{
		Keys:        keys,
		Tokens:      tokens,
		Audit:       auditView,
		Resets:      resets,
		Invitations: invitations,
		Profile:     profile,
		Accounts:    accounts,
		Notifier:    notifier,
		Auth:        auth,
		Setup:       setup,
		Reaper:      app.NewSessionReaper(r.sessions, retention, d.Now),
		AuditPurger: app.NewAuditPurger(r.audit, retention, d.Now),
		HTTP:        h,
		Purges: []*app.PurgeJob{
			app.NewPurgeJob(app.JobInvitationsPurge, app.InvitationRetention, r.invitations, d.Now),
			app.NewPurgeJob(app.JobResetTokensPurge, app.TokenRetention, r.resets, d.Now),
			app.NewPurgeJob(app.JobEmailTokensPurge, app.TokenRetention, r.emailChanges, d.Now),
		},
	}, nil
}

// RunKeyMaintenance reloads, rotates and prunes the keyring every
// KeyMaintenanceEvery until ctx is done. It is a background worker: it logs
// its own errors.
func (m *Module) RunKeyMaintenance(ctx context.Context, now func() time.Time, logger *slog.Logger) {
	t := time.NewTicker(KeyMaintenanceEvery)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := m.Keys.Maintain(now()); err != nil {
				logger.ErrorContext(ctx, "token key maintenance failed", slog.Any("error", err))
			}
		}
	}
}
