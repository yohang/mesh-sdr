// Package identity wires the identity module (users, sessions, CSRF,
// authorisation, audit log) for the composition root.
package identity

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/config"
	"github.com/yohang/mesh-sdr/internal/db"
	"github.com/yohang/mesh-sdr/internal/identity/app"
	"github.com/yohang/mesh-sdr/internal/identity/domain"
	identityhttp "github.com/yohang/mesh-sdr/internal/identity/http"
	"github.com/yohang/mesh-sdr/internal/identity/infra/argon2"
	"github.com/yohang/mesh-sdr/internal/identity/infra/commonpw"
	"github.com/yohang/mesh-sdr/internal/identity/infra/memory"
	"github.com/yohang/mesh-sdr/internal/identity/infra/settings"
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
)

// Deps are the dependencies of the identity module.
type Deps struct {
	Config config.Hub
	Logger *slog.Logger
	DB     db.Adapter
	IDs    *shared.UUIDv7Generator
	Now    func() time.Time
}

func component(l *slog.Logger, name string) *slog.Logger {
	return l.With(slog.String("component", name))
}

type repos struct {
	users    *sqlite.Users
	sessions *sqlite.Sessions
	audit    *sqlite.AuditLog
	hasher   *argon2.Hasher
}

func newRepos(d Deps) repos {
	a := d.Config.Auth.Argon2

	return repos{
		users:    sqlite.NewUsers(d.DB, d.IDs),
		sessions: sqlite.NewSessions(d.DB),
		audit:    sqlite.NewAuditLog(d.DB),
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

// policies returns the password policy source: settings (defaults until the
// settings store is wired) and the bundled common-password list.
func policies() app.Policies {
	return app.NewPolicies(settings.Defaults{}, commonPasswords())
}

// UserAdmin builds the user administration service used by the CLI.
func UserAdmin(d Deps) *app.UserAdmin {
	r := newRepos(d)

	return app.NewUserAdmin(app.UserAdminDeps{
		Users: r.users, Sessions: r.sessions, Audit: r.audit, Tx: d.DB, Hasher: r.hasher, IDs: d.IDs,
		Now: d.Now, Policy: policies(), Logger: component(d.Logger, "identity.app.users"),
	})
}

// Module is the wired identity module of the hub.
type Module struct {
	Auth   *app.Auth
	Reaper *app.SessionReaper
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

	auth := app.NewAuth(app.AuthDeps{
		Users: r.users, Sessions: r.sessions, Audit: r.audit, Tx: d.DB, IDs: d.IDs, Now: d.Now, Provider: local,
		IPLimiter: memory.NewIPLimiter(loginIPEvery, loginIPBurst, memory.DefaultCapacity),
		Unknown:   memory.NewThrottle(memory.DefaultCapacity),
		Refusals:  memory.NewRefusalGate(memory.DefaultCapacity),
		Throttle:  domain.DefaultThrottlePolicy(), SessionPolicy: domain.DefaultSessionPolicy(),
		Logger: component(d.Logger, "identity.app.auth"),
	})

	passwords := app.NewPasswords(app.PasswordsDeps{
		Users: r.users, Sessions: r.sessions, Audit: r.audit, Tx: d.DB, Hasher: r.hasher, IDs: d.IDs, Now: d.Now,
		Policies: policies(), Throttle: domain.DefaultThrottlePolicy(), SessionPolicy: domain.DefaultSessionPolicy(),
		Logger: component(d.Logger, "identity.app.passwords"),
	})

	h, err := identityhttp.New(auth, passwords, pages, identityhttp.Config{
		HubURL:         d.Config.Hub.URL,
		TrustedProxies: config.Prefixes(d.Config.HTTP.TrustedProxies),
		AdminNetworks:  config.Prefixes(d.Config.Admin.AllowedNetworks),
	}, component(d.Logger, "identity.http"))
	if err != nil {
		return nil, fmt.Errorf("identity http: %w", err)
	}

	return &Module{
		Auth:   auth,
		Reaper: app.NewSessionReaper(r.sessions, d.Now, component(d.Logger, "identity.app.reaper")),
		HTTP:   h,
	}, nil
}
