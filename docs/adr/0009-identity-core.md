# ADR 0009: Identity core (users, sessions, CSRF, authorisation)

- Status: Accepted
- Date: 2026-10-06
- Deciders: project owner
- Scope: epic part `epic/auth-1`. Tickets: AUTH-017 #44, AUTH-003 #30, AUTH-015 #42, AUTH-020 #47, AUTH-004 #31, AUTH-019 #46, AUTH-002 #29, AUTH-005 #32, AUTH-016 #43, AUTH-001 #28, AUTH-008 #35, AUTH-014 #41, AUTH-012 #39, AUTH-013 #40.

## Context

TECHNICAL_SPEC §5 and §7.1 specify local accounts, sessions, CSRF protection, roles, rate limits and the audit log. Several points admit more than one reasonable design, and the two specs disagree on some names and defaults. ADR 0003 already fixed the CSRF mechanism: a session-bound token fetched only from `GET /api/v1/auth/session`, a pre-session double-submit token for anonymous forms, `http.CrossOriginProtection` instead of an Origin allow-list, and JSON required only on `/api/v1`.

Owner decisions taken before this part: light DDD in `internal/identity/{domain,app,infra,http}`, `golang.org/x/crypto/argon2`, stdlib tests, repositories in `infra/sqlite` with `infra/repotest` contract suites (ADR 0006), and a spec-first OpenAPI document. JWT access tokens (ACC-007) and mail are later parts.

## Decision

### Module layout

- `domain`: the `User` aggregate (with its identities and role grants), the `Session` aggregate, the request `Principal`, the `ThrottlePolicy`, audit entries and repository interfaces. Ids wrap the shared UUIDv7 (`shared.UUID`).
- `app`: the `Provider` interface (the local form login is its only implementation), `Auth` (login, session resolution, logout), `UserAdmin` (CLI) and the `SessionReaper`.
- `infra`: `argon2` (hasher), `sqlite` (repositories), `repotest` (contract suites) and `memory` (rate limiters).
- `http`: the router module (client address, session, CSRF, JSON-only API bodies), `Authorize`/`Require`, and the login page.
- `internal/identity/wire.go` wires the module; `internal/wire` composes it into the hub.

### Passwords (AUTH-017)

- Argon2id PHC strings: 16-byte salt, 32-byte key, parameters from `auth.argon2.memory_kib|iterations|parallelism` (defaults 65536/3/1). The config load refuses values below 19456 KiB and 2 iterations.
- After a successful login, a hash with other parameters is re-hashed. A malformed stored hash makes only that account unusable.
- A semaphore (`GOMAXPROCS`) limits concurrent hash computations, and at most 4 requests per CPU may wait for a slot. Beyond that, logins answer 429 `rate_limited`.
- Every login attempt runs exactly one verification, against a dummy hash computed at startup when the account is unknown, disabled or has no local password.

### Sessions and cookies (AUTH-003)

- The token is 32 random bytes, sent base64url in the cookie. Only its SHA-256 is stored (`sessions.token_hash`).
- Lifetimes are idle 24 h and absolute 24 h, or 30 days with `remember_me` (Q7). The cookie gets a `Max-Age` only with "remember me".
- `last_seen_at` is written at most once per minute, by a query that touches only unrevoked rows. Revocations use dedicated queries that keep an earlier revocation (`COALESCE`). A save never rewrites `revoked_at`, so a request recording activity from a stale copy cannot resurrect a session that was logged out, rotated or disabled meanwhile.
- Login always creates a new session and revokes the session the browser presented (`revoke_reason = 'rotated'`).
- Logout revokes the row (`revoke_reason = 'logout'`) rather than deleting it (Q8).
- Disabling a user revokes all its sessions in the same transaction.
- The hub runs a reaper hourly: it deletes rows 30 days after expiry or revocation, in batches (Q12).
- Cookie names follow the `hub.url` scheme, because no `tls.mode` key exists:
  - `https`: `__Host-rx_session` / `__Host-rx_presession`, with `Secure`;
  - `http`: `rx_session` / `rx_presession`. This stays allowed (loopback, trusted LAN), but the hub logs a warning at startup.
  - Both are `HttpOnly; Path=/; SameSite=Lax` without `Domain`.

### CSRF (AUTH-019)

- Middleware order: client address → session → `http.CrossOriginProtection` (with `hub.url` as a trusted origin) → `X-CSRF-Token` check → `Content-Type: application/json` required on `/api/v1` unsafe requests that have a body (415 `unsupported_media_type`).
- Session token: `base64url(HMAC-SHA256(csrf_secret, raw session token))`. `csrf_secret` is 32 random bytes per session, stored in clear (Q3): the raw session token is never stored, so the database alone cannot forge a token. The secret rotates with the session.
- Pre-session token: `HMAC-SHA256(process key, pre-session cookie value)`. The pre-session cookie has no database row. A restart changes the key, and the client simply fetches the token again.
- A failure answers 403 `csrf_failed` (problem+json under `/api/`, the shell error page elsewhere) and logs one Warn.
- `static/js/csrf.js` (owned by this module, imported by the shell):
  - fetches the token lazily from the session API;
  - adds it to unsafe same-origin htmx requests by wrapping `ctx.fetch` in `htmx:before:request`, and to island requests through `apiFetch`;
  - retries once after `csrf_failed`.

### Rate limiting (AUTH-002, SR-05)

- Per client address: an in-memory token bucket (`golang.org/x/time/rate`), 5 per minute, burst 5. It sits in an LRU of 100 000 entries, and IPv6 clients are keyed by /64.
- Per account (the normalised identifier as typed): consecutive failures from the 5th impose a delay of 1 s, 2 s, 4 s and so on. The 10th locks the account for 15 minutes, doubling on each further failure up to 24 h.
- Each attempt is reserved atomically before the password is verified: it is counted as a failure in a write transaction (accounts) or under a mutex (unknown identifiers), and the count is reset on success. Parallel guesses therefore cannot exceed the thresholds.
- While delayed or locked, attempts are refused with 429 `rate_limited` and `Retry-After`, without verifying the password. Only the first refusal of a key (client address, login identifier) within its blocking window is audited; later ones are logged at Debug, so a flood does not turn into audit writes through the single writer.
- Bodies of `/login`, `/logout` and `/api/v1/auth/*` are capped at 64 KiB. A login over 254 bytes or a password over 1024 bytes is refused as invalid credentials before any lookup or hash.
- Existing users keep their counters in `users.failed_login_count` and `users.locked_until`. Identifiers that match no account are throttled in an in-memory LRU with the same algorithm (Q15), so answers are identical.
- The thresholds are code constants until the settings store exists (Q4).

### Authorisation (AUTH-004, AUTH-016)

- Each operation in `openapi.yaml` declares `x-meshsdr-access: anonymous|listener|operator|admin`. This is the policy table of SR-15.
- `api.NewHandler` loads it from the embedded document, and a strict middleware enforces it. An operation without an access level fails a test and is refused at runtime.
- HTML routes use `Module.Require(role)`: anonymous visitors are redirected to `/login?next=…`, and forbidden requests get a 403 page.
- Admin requirements also check the resolved client address against `admin.allowed_networks` (403 `admin_network_denied`). Non-admin operations are never affected, even for admins (Q13).
- Client address: the TCP peer, or, when the peer is in `http.trusted_proxies`, the right-most `X-Forwarded-For` hop that is not a trusted proxy.
  - Hops may carry a port (`ip:port`, `[v6]:port`).
  - On a malformed hop, the right-most hop that parsed is used, or the address is unknown when none parsed; it never falls back to the proxy. Unknown addresses share one login rate-limit bucket and never pass the admin network check.
  - `Forwarded` is ignored. Addresses are canonicalised (IPv4-mapped, zones).
- Listen policy hook: `Principal.CanListen(effectivePolicy)`. Resolving the effective device policy belongs to the device registry (later).

### Audit log

`audit_log` is created now with the §7.1 columns and a trigger that aborts every UPDATE (Q9). DELETE is not blocked: the retention job (`retention.audit_log`) needs it. Tamper evidence (the SR-69 hash chain) is deferred. It records:

- login success, failure (with the reason and provider in `after`) and lock-out;
- logout;
- user creation, disable and enable from the CLI (`actor_kind = 'cli'`).

There is no read API yet (ACC-010).

### Schema and configuration

- Migration `00002_identity.sql` creates `roles` (seeded 0/10/20/30), `users`, `user_identities`, `user_roles`, `sessions` and `audit_log`, following the §7.2 type mapping.
- `user_roles.device_id` has no foreign key until the `devices` table exists; the grid migration adds it (Q10).
- Identities and grants belong to the `User` aggregate.
- New config keys: `auth.argon2.memory_kib|iterations|parallelism`, `admin.allowed_networks` (default `["0.0.0.0/0", "::/0"]`) and `http.trusted_proxies` (default `[]`) (Q2, Q5).
- `MESHSDR_PASSWORD` is accepted as a non-config variable for `hub user add --noninteractive`.

### REST and UI

- `GET /auth/session` returns `{authenticated, user?, roles, csrf_token, providers}` and sets the pre-session cookie when it is missing.
- `POST /auth/login {login, password, remember_me?}` returns the same body with the new token and sets the session cookie.
- `POST /auth/logout` requires `listener`, answers 204 and clears the cookie.
- `/login` is a full page. Its htmx form posts to `POST /login`:
  - on error it returns the form fragment with the generic message (401, or 429);
  - on success it sends `HX-Redirect` to a safe `next` (same-origin path, no `//` or `\`, matching a route; otherwise `/`).
- `POST /logout` ends the session.
- Pages render through an injected `Pages` interface. The composition root passes a temporary adapter over the current layout, which the shell renderer (`internal/web/render`) replaces when the shell epic lands.

### CLI (AUTH-008, AUTH-012, AUTH-013, AUTH-014, AUTH-015)

- `meshsdr hub user add <name> [--email] [--display-name] [--role listener|operator|admin]`:
  - interactively, it asks for the password twice;
  - with `--noninteractive`, it reads `MESHSDR_PASSWORD`;
  - otherwise it generates a password, prints it once (even with `--silent`) and sets `must_change_password`.
- `hub user exists` answers with the exit status only. `disable` revokes every session; `enable` clears the lock-out.
- The commands refuse to run on a schema that is not current.
- `remove`, `reset-password` and `list` come later.

## Divergences from the specification and open points

The spec is not edited. These inconsistencies were found and resolved as follows:

1. **Argon2 keys and defaults.** §5.3 says `memory_kib` 19456/2/1; §7.4 says `auth.argon2.memory` (size string); SR-03 and FEATURE §9 say `memory_kib` 65536/3/1. **Decision:** `memory_kib`, 65536/3/1, floor 19456/2.
2. **Session lifetime keys and defaults.** §5.5 has `session_idle_timeout`/`session_max_age` (12 h, 24 h / 30 d with "remember me"); §7.1 and §7.4 have `auth.session_idle_timeout`/`auth.session_max_lifetime` (cfg; example 14 d / 90 d); FEATURE and SR-01 have `session.idle_timeout`/`session.absolute_timeout` (db, 24 h / 30 d). **Decision:** idle 24 h, absolute 24 h or 30 d with `remember_me`, as constants until the settings store exists.
3. **`sessions` columns.** §5.5 has `id_hash`, `ip_hash`, `csrf_secret`; §7.1 has `id` + `token_hash`, `ip`, `csrf_secret_hash`, `revoke_reason`. A hashed CSRF secret cannot key the HMAC token of §5.7. **Decision:** §7.1 columns, with `csrf_secret` in clear instead of `csrf_secret_hash`, and the extra revoke reason `rotated`. Session token entropy is 256 bits (§5.5), not 128 bits (SR-01).
4. **`users`.** §5.1 has `disabled_at`; §7.1 has `enabled`. **Decision:** `enabled` (§7.1).
5. **`audit_log`.** §5.13 has `ts`, `actor_type`, `actor_id`, `ip_hash`, `reason`; §7.1 has `at`, `actor_kind`, `actor_user_id`, `actor_ip`, `request_id`. **Decision:** §7.1. Reasons go in `after`. The SR-69 hash chain is deferred.
6. **Logout.** "Deletes the session" (§5.5, SR-01, AUTH-005) vs revocation with `revoke_reason = 'logout'` and 30-day retention (§7.1). **Decision:** revoke.
7. **Login rate limits.** §5.12 has 5/min per IP, 20/h per account and an exponential lock-out after 10 failures; FEATURE has `auth.login_rate_limit = 5/15m`; SR-05 has 10 failures in 15 min lock for 15 min. **Decision:** the mix described above. The 20/h per-account limit is replaced by the progressive delay.
8. **Trusted proxies.** `http.trusted_proxies` (FEATURE, AUTH-016) vs `gateway.trusted_proxies` (§2.3, §7.4, SR-13). **Decision:** `http.trusted_proxies`. The gateway epic decides how the gateway itself is trusted.
9. **Password policy.** Minimum 12 and maximum 256 (§5.3), minimum 10 and maximum ≥128 (ACC-011), never below 8 (SR-04). **Decision:** minimum 10, floor 8, maximum 256. The common-password list (SHOULD) is not implemented yet.
10. **Invitation and reset TTL keys** differ (`auth.invitation_ttl` 7 d vs `invitations.ttl_hours` 72; reset 1 h, 30 min or 60 min). They are not used in this part.
11. **Bootstrap.** `admin create-user --admin` (§5.1), `auth.bootstrap_admin` (§7.4), the setup URL and `user add` (AUTH-018). Only `meshsdr hub user add --role admin` exists so far; AUTH-018 decides the rest.
12. **Cookie name.** It depends on `tls.mode`, which no config key defines. It is derived from the `hub.url` scheme.
13. **Role change.** "Rotate the id on role change" (§5.5) vs "revoke all sessions on a downgrade" (AUTH-003, ACC-006). This is left to ACC-006.
14. **Origin check.** AUTH-019 checks `hub.url` only, while §5.7 adds `gateway.extra_origins`. ADR 0003 replaced the allow-list with `http.CrossOriginProtection`, so only `hub.url` is a trusted origin for now.
15. **Access-token key names.** `auth.token_ttl_s`/`auth.signing_key_file` (FEATURE) vs `auth.access_token_ttl`/`auth.token_signing_key` (§7.4). This is for ACC-007.
16. **CLI env var.** `$PRODUCT_PASSWORD` (AUTH-008) is `MESHSDR_PASSWORD`.
17. **Lock-out enumeration (SR-05, SR-06).** An account is throttled by one persisted counter, whichever identifier is typed (username or e-mail). An unknown identifier is throttled by an in-memory counter per typed string. By comparing the lock-out of a username with that of an e-mail, or by watching which counters survive a hub restart, an attacker can tell whether two identifiers belong to the same account, or whether an account exists. **Owner decision:** accepted and documented as a residual risk; no code change.
18. **Dummy hash parameters.** The dummy hash uses the current Argon2 parameters. A user whose stored hash still has older parameters answers faster or slower than an unknown account until the next successful login re-hashes it. **Owner decision:** accepted as transitional.
19. **`must_change_password` is not enforced yet** (AUTH-006). Users created with a generated password can sign in and use the hub without changing it. **Owner decision:** open point; enforcement and the change-password flow come first in `epic/auth-2`.
20. **Unknown fields.** SR-20 requires rejecting unknown fields. The generated strict server decodes JSON without `DisallowUnknownFields`. `LoginRequest` declares `additionalProperties: false`, but this is not enforced at runtime yet.

## Consequences

- AUTH-004 is only partly done: REST and HTML routes are covered, while `/api/ws` topics and node media messages come with their epics through the same `Authorize` and `CanListen` hooks.
- AUTH-005's "close the WebSockets bound to the session" and AUTH-012's "stop token refresh" become effective with the WebSocket and access-token epics, because they read the revoked session.
- The shell epic imports `static/js/csrf.js` and replaces the composition root's temporary `Pages` adapter with its renderer. The login page then renders inside the app shell.
