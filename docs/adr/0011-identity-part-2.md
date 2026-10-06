# ADR 0011: Identity part 2 (passwords, bootstrap, accounts and access)

- Status: Accepted
- Date: 2026-10-06
- Deciders: project owner
- Scope: two epic parts designed together, because they share `internal/identity`.
  - `epic/auth-2` (epic #447): AUTH-006 #33, AUTH-007 #34, AUTH-010 #37, AUTH-011 #38, AUTH-018 #45, and ACC-011 #58 (a prerequisite of AUTH-006/007).
  - `epic/acc-1` (epic #448): ACC-001 #48, ACC-002 #49, ACC-003 #50, ACC-004 #51, ACC-005 #52, ACC-006 #53, ACC-007 #54, ACC-008 #55, ACC-009 #56, ACC-010 #57, and AUTH-009 #36 (the same use case as ACC-009).

## Context

ADR 0009 built the identity core: users, sessions, CSRF, authorisation and the audit log. It left open points, with the enforcement of `must_change_password` first. TECHNICAL_SPEC §5, §7.1 (`invitations`, `password_reset_tokens`), §7.4 and SR-01…SR-11, SR-43…SR-45 and SR-64 describe the rest, with several inconsistencies listed below.

Constraints in force:

- light DDD and stdlib tests;
- spec-first OpenAPI with `x-meshsdr-access`, JSON only on `/api/v1`;
- CSRF per ADR 0003, pages through the shell renderer (ADR 0007);
- dependencies: stdlib and `golang.org/x` free, others approved by the owner (`golang-jwt/jwt/v5`, `wneessen/go-mail`).

Two other parts move in parallel:

- the DB settings store (`epic/adm-1`, ADR 0010), with migrations 00008–00010;
- the grid control channel (`epic/grid-2`, ADR 0008), with migrations 00003–00007.

## Decision

### Delivery

- **PR 1, `epic/auth-2`** (first). Commits in order: the `must_change_password` gate, the password policy, the forced change page, the change API, `user reset-password`, `user list`, the setup link, this ADR. It closes #33 #34 #37 #38 #45 #58. It needs no migration.
- **PR 2, `epic/acc-1`**, rebased on main after PR 1. Commits in order: migrations 00011+ (`invitations`, `password_reset_tokens`, `email_change_tokens`), mail, roles, sessions, account page, invitations, reset, admin users, deletion and export with `user remove`, audit view, optional accounts, access tokens last.
  - grid-2 merged first, so ACC-007 is in PR 2, split with the gateway epic (grid-3, see "Access tokens").
  - Migrations 00011–00014; the range 00011–00019 is reserved for identity. A development database that applied them before the settings migrations (00008–00010) needs goose's allow-missing once, or a reset.
  - PR 2 closes #48 #49 #50 #51 #52 #53 #55 #56 #36, references #57 (closed with ADM-011 retention) and #54 (closed by grid-3 once the keys and revocations reach the nodes).

### Forced password change (AUTH-006) — PR 1

- An identity middleware runs after the session, CSRF and JSON middlewares. For a user flagged `must_change_password`, it allows only:
  - `/account/password`, `/logout`, `/policy`;
  - `/api/v1/auth/session`, `/api/v1/auth/logout`, `/api/v1/auth/password`, `/api/v1/openapi.json`;
  - `/static/*`, `/api/v1/healthz/*`, favicon, manifest and robots.
- Every other route is refused, anonymous ones included:
  - pages redirect (303, or `HX-Redirect`) to `/account/password?forced=1`, with a safe `next` for GET requests;
  - API calls answer 403 `password_change_required`.
- Access tokens (ACC-007) will refuse flagged users too.

### Password change (AUTH-006, AUTH-007) — PR 1

- `/account/password` is both the forced interstitial (the route of the ticket) and the voluntary change. `POST /api/v1/auth/password` (listener) is the JSON form.
- **Current password.** It is required (SR-04). A wrong one counts as a failed login: the attempt is reserved, delayed and locked exactly like password login.
- **New password.** It follows the policy and must differ from the current one (`same_as_current`).
- **Effects, in one transaction:**
  - the hash is set and `must_change_password` is cleared;
  - every session of the user is revoked (`password_change`);
  - the request's session is replaced by a new one with the same absolute expiry and "remember me" state (§5.5 rotation);
  - the change is audited (`auth.password.change`).
- **Concurrency.** The storing transaction re-checks that the request's session is still active and that the stored hash is the one verified, so a concurrent reset or change wins.
- **Gate paths.** A request whose raw path differs from its decoded path is never allow-listed (chi routes on the raw path).
- **Redirect.** A forced change redirects to the safe `next`; a voluntary one to `/account/password?changed=1`.
- **Notification.** The e-mail notice (SR-04) comes with mail in PR 2.

### Password policy (ACC-011) — PR 1

- The minimum is `auth.password_min_length` (default 10, floor 8) and the maximum 256 characters. There are no composition rules.
- The bundled list is SecLists `xato-net-10-million-passwords-100000` (MIT), at a pinned commit with a SHA-256 check.
  - `make vendor-passwords` keeps the 38 451 lower-cased entries of 8 characters or more (shorter ones already fail the length rule), sorted and gzipped (118 KB), with `LICENSE.SecLists`.
  - The check ignores case.
- A refusal is `invalid_password` with one `password` violation whose code is the reason: `too_short`, `too_long`, `invalid_utf8`, `common` or `same_as_current`.
- Passwords are normalised to Unicode NFC (`golang.org/x/text/unicode/norm`) before they are checked, hashed or verified, so the same characters typed on systems that compose them differently match. The common-list lookup also applies NFKC and trims surrounding spaces. Existing hashes of non-NFC passwords (pre-release only) would need a reset.

### Settings seam

- Identity reads its admin-editable settings through a consumer-side `app.Settings` port, with interim constant defaults (`identity/infra/settings.Defaults`):
  - `auth.password_min_length`;
  - later `invitations.ttl_hours` (default 7 days, maximum 30) and `password_reset.ttl_minutes` (default 30).
- Whichever of adm-1 and acc-1 merges second wires these keys into the store, like adm-1's session and login-throttle keys. Done in PR 2: `invitations.ttl_hours` (1–720 hours, default 168) and `password_reset.ttl_minutes` (5–1440 minutes, default 30) are store keys under Admin › Access › Invitation and reset links, read with `auth.password_min_length` and `listen_policy` through `identity/infra/settingsrc`. CLI commands, which run without the store, keep `settings.Defaults`.

### First admin setup (AUTH-018) — PR 1

- **Link creation.** At hub start, when no enabled global admin exists, the hub creates a 256-bit token. It keeps only its SHA-256, in memory. The token is valid for 60 minutes and a restart issues a new one.
- **Display.** The URL is built from `hub.url` and printed once on stderr, outside slog. A Warn log line without the token says so.
- **Access.** The link is reachable only from `admin.allowed_networks`, the existing key whose default is any address (owner decision; no new key). Requests are limited to 10, then 1 per minute, per client address.
- **Page.** `/setup/{token}` asks for username, optional e-mail and display name, and the password twice. It sends `Referrer-Policy: no-referrer` and `Cache-Control: no-store`.
- **Completion.** In one transaction it re-checks that no admin exists and creates the admin (`actor_kind = system`, `via: setup`). The token is consumed and the admin is signed in. An unknown, expired or used link answers 404.
- **Not implemented.** `auth.bootstrap_admin` is not implemented. Packaging uses `user exists` and `user add --role admin`.

### Tokens in URLs (SR-07)

- Routes keep the spec paths: `/setup/{token}`, `/invite/{token}`, `/password/reset/{token}`.
- One helper (`internal/http/redact`) replaces the token after `/setup/`, `/invite/`, `/password/reset/`, `/account/email/verify/` and `/auth/invitations/`, wherever the prefix appears and whatever the case. Every logged request path goes through it (request log, 404/405, CSRF and role refusals, the forced-change gate, rendering and API errors), and `SafeNext` refuses a `next` that carries a token.
- `static/js/token-url.js`, imported by the shell, replaces the address with the page's `data-replace-url`. The token stays in a hidden field.

### CLI — PR 1 (AUTH-010, AUTH-011), PR 2 (AUTH-009)

- `meshsdr hub user reset-password <name>` follows the rules of `user add`:
  - the password is asked twice, read from `MESHSDR_PASSWORD`, or generated, printed once and flagged;
  - it revokes every session (`password_reset`), clears the lock-out and is audited (`user.password.reset`).
- `meshsdr hub user list [--all] [--json]` lists the enabled users (`--all`: disabled ones too) with:
  - name and e-mail;
  - roles (implicit `listener`, device scopes after `@`);
  - enabled state and last sign-in.
- `meshsdr hub user remove <name> [--yes]` comes in PR 2, with `hub keys rotate|revoke`.

### Accounts and access — PR 2

- **Roles (ACC-006).**
  - `PUT /users/{id}/roles` replaces the grants.
  - **Any privilege change, upgrade or downgrade, revokes every session of the user** (owner decision, strict SR-01). The revocation is pushed to nodes once ACC-007 exists.
  - The last enabled global admin cannot be demoted, disabled or deleted (web and CLI, checked in the transaction).
- **Sessions (ACC-005).**
  - Own sessions and any user's for admins.
  - Revoke one or all others. Revoke reasons reuse the existing values: `logout` for one's own, `admin` otherwise.
  - Device and browser come from a small stdlib user-agent heuristic; the raw value is shown as text.
- **Account page (ACC-004).**
  - Profile, security, sessions, your data.
  - An e-mail change needs the current password. It is verified by a link to the new address, and the old address gets a notice. Without SMTP it applies at once, unverified.
- **Invitations (ACC-002).**
  - Created by admins, with role, optional device scope, optional e-mail and expiry. The link is shown once and also mailed when possible. Invitations can be revoked.
  - `/invite/{token}` creates the user, the local identity and the grant in one transaction, then signs in. The role always comes from the invitation, and an invitation e-mail is stored verified.
  - A signed-in visitor is asked to sign out first. Rate limit: 3 per hour per address and per token.
- **Password reset (ACC-003).**
  - The reset link may be sent to **any e-mail on the account, verified or not** (owner decision, §5.3).
  - The request always gets the same answer, and the mail is sent asynchronously. Limits are 3 per hour per address and per account; a lock-out never blocks a reset.
  - Completing it, in one transaction: sets the hash, consumes the token, revokes all sessions, clears the lock-out and the flag, marks the e-mail verified, and writes the audit entry.
- **Mail.**
  - `wneessen/go-mail`, plain-text templates, links built from `hub.url`.
  - An in-memory bounded queue with retries; tokens are never stored in clear.
  - `smtp.tls` is `starttls` or `implicit` with verified certificates; plaintext needs `smtp.allow_insecure = true` and logs a startup Warn.
  - Development uses mailpit, pinned by digest, under a compose `mail` profile.
- **Deletion and export (ACC-009, AUTH-009).**
  - Deletion removes the `users` row (cascading to identities, grants, sessions and tokens) and clears `invitations.email` of redeemed invitations. Other modules can add erasure hooks (grid: `connections`).
  - The audit log keeps the user UUID as the pseudonymous id. New audit entries no longer carry usernames. Earlier entries are accepted as they are (pre-release, and the table refuses updates).
  - The export is JSON: account and sessions.
- **Audit view (ACC-010).**
  - Filters, keyset pagination and CSV/JSON export, with CSV formula-injection protection.
  - The retention job is adm-1's ADM-011 (`audit.purge`).
- **Access tokens (ACC-007).**
  - **Format.** Ed25519 JWS (`golang-jwt/jwt/v5`). `kid` is the RFC 7638 thumbprint.
  - **Claims.**
    - `iss` = `hub.url`, `aud` = `rx-node:<id>`, `sub` = user id or `anon`.
    - `sid` = a hash prefix of the session id.
    - `cid`, `roles`, `scp`, `lim`.
    - `iat`, `nbf`, `exp`, `jti`.
  - **Lifetime.** TTL `auth.token_ttl` of 5 minutes, clamped to 60–600 s. Leeway 30 s.
  - **Signing keys.** A hub-managed keyring in `/var/lib/meshsdr/keys`:
    - each key in its own file, mode 0600, in a directory with mode 0700;
    - the key cycle is next → current → retired, with rotation every 30 days and the `hub keys rotate|revoke` commands;
    - like the node certificate, it is a documented exception to "the binary never writes files".
  - **Distribution.** Public keys go out over `ctl.keys.update`, and also as `/.well-known/jwks.json`.
  - **Revocation.** `ctl.revocations` gains `sessions` and `users`.
- **Navigation.** The shell gets a user slot: Sign in, Account, Log out. Users, Invitations and Audit log are sections of adm-1's admin area (`layout.AdminSections`), rendered in its admin layout.
- **REST additions.** `/me/sessions`, `/me/email`, `/me/export`, `DELETE /me`, `/users/{id}/sessions`, `/users/{id}/password`, `/users/{id}/export`.
  - Object-level checks apply (SR-18).
  - New bodies reject unknown fields (SR-20).

## Implementation notes (PR 2)

- **Request bodies.** Every closed JSON body (`additionalProperties: false`) is checked for unknown fields once the route matches, only for callers the access policy accepts, so refusals keep their 401/403 (SR-20).
- **Persistence.** One repository per aggregate: invitations, reset tokens and e-mail change tokens store only token hashes; issuing a reset or e-mail token invalidates the user's earlier ones; a stale invitation copy cannot be redeemed or revoked twice (conditional update). Scheduler jobs (adm-1's `jobs/app.Scheduler`, hourly) delete ended invitations after 30 days (`invitations.purge`) and ended reset and e-mail change tokens after 1 day (`reset_tokens.purge`, `email_tokens.purge`).
- **Sessions.** The public handle of a session (`Session.Ref`, a hash prefix of its id) identifies it to users, admins and access tokens (`sid`); the id itself never leaves the hub (§7.1).
- **Mail.** `[smtp]` config (host, port 587, tls `starttls`, username, password secret, from, allow_insecure); `internal/mail` sends plain text with go-mail through a 256-message in-memory queue (3 retries); logs carry only the recipient domain. `internal/identity/infra/notify` renders the messages. Development: mailpit `v1.31.4` pinned by digest under the compose profile `mail`, settings in `.env.example`.
- **Roles.** `PUT /users/{id}/roles` replaces the grants; the HTML editor sets a global role, or operator on listed devices. Any change revokes every session (owner decision); the last enabled admin cannot be demoted, disabled (web and CLI) or deleted.
- **Pages.** `/account` (profile, e-mail, password link, sessions, your data), `/account/email/verify/{token}` (GET shows, POST applies), `/admin/users[/{id}]`, `/admin/invitations`, `/admin/audit` (+ `/export?format=csv|json`, CSV formula cells prefixed with `'`), `/invite/{token}`, `/password/forgot`, `/password/reset/{token}`. Token pages send `Referrer-Policy: no-referrer` and hide the token (`token-url.js`). The top bar shows a discreet "Sign in" link, or the user's name, Account, the admin links and "Sign out"; the settings epic adds its Admin entry next to them.
- **Rate limits.** Reset requests 3/h per client address (429) and per account (silent); reset links, invitation checks and acceptances 10 then 1 per 6 minutes per address (§5.12's 3/h would block a retry after a taken username); token mints 30/min per session.
- **Deletion.** One transaction: the `users` row (identities, grants, sessions, tokens cascade), the address of the invitations it redeemed, and the `UserEraser`s of other modules (none yet: the grid `connections` cleanup is to add). New audit entries no longer carry usernames.
- **Access tokens (ACC-007).** The token contract (claims, `Sign`, `Verify`, JWK/JWKS, thumbprint `kid`) is the shared package `internal/protocol/rxv1/token`, also used by node verification (grid-3).
  - Identity owns the keyring (`auth.token_key_dir`, default `/var/lib/meshsdr/keys`; `auth.token_ttl` 5m, 1m–10m; `auth.key_rotation_days` 30), the issuer, `GET /.well-known/jwks.json` (keys and `revoked_kids`), `POST /api/v1/auth/token` and `meshsdr hub keys list|rotate|revoke`.
  - The keyring is the grid's key source (`Published`, `OnChange`); identity publishes revocations through `app.RevocationPublisher` (`identity.Deps.Revocations`).
  - Grid-3 owns `ctl.keys.update`, the `ctl.revocations` transport, node-side verification and the gateway authz that binds connection ids (`app.ConnectionBinder`): until it is wired, signed-in callers get tokens bound by `sub` and `sid`, anonymous callers get 401.
  - Scopes: listen and demod on each device the caller may listen to (device policy, else the global `listen_policy`, from the settings store; an unavailable or invalid value fails closed to `registered`, with a Warn); preset for an operator on the device; retune for an admin, or such an operator where `operator_can_retune`. `lim.max_demods` is 4 until a setting exists.
- **Security review fixes.**
  - Pending reset and e-mail change links die whenever access changes (password change or reset, generated password, disable, sign-out of other or all sessions, e-mail change), in the same transaction; a reset or confirmation of a disabled account is refused. A reset confirms only the address its link was e-mailed to (`password_reset_tokens.sent_to`, migration 00014).
  - Deleting a user also erases its grid presence rows (closed rows deleted, open rows stripped of user, session, address and user agent), from the web and the CLI.
  - An invitation is refused once its creator is no longer an enabled admin; its address is confirmed only when the invitation was e-mailed.
  - The key directory is locked (advisory `flock`) around every read-modify-write, so the hub and `meshsdr hub keys` never lose each other's changes. The hub signs with the state it loaded last: a CLI revocation applies within a minute, together with the push of `revoked_kids`.
  - Account flows never answer `email_taken`: an address another account uses is "cannot be used" (or a dead link on confirmation). Without mail, an e-mail change applies at once, so its outcome still differs for a taken address; with mail nothing is told before the link is opened.
  - Exports (account, user, audit log) are POST requests with the CSRF header (`static/js/download.js` saves the file); the JWKS is cached 30 s, less than the key publish lead.
  - The CLI (`hub user disable|reset-password|remove`, another process) does not publish revocations to nodes: their tokens stop at the next refresh, within one token lifetime.
  - Nodes authorise media actions from `scp` only; `roles` in the token is informational.
  - Audit rows written before usernames were dropped from audit details keep them (pre-release, owner decision Q6a).
- **Dependencies.** `wneessen/go-mail`, `golang-jwt/jwt/v5` (approved), `github.com/oapi-codegen/runtime` (path parameters, as grid-2).

## Spec inconsistencies

Recorded here; the spec is not edited.

1. **Forced change route.** `/account/password?forced=1` (AUTH-006) vs `/password/change` (FEATURE pages table).
2. **Reset routes.** `/password-reset` (ACC-003) vs `/password/forgot` and `/password/reset/{token}` (FEATURE pages).
3. **Reset TTL.** 1 h `auth.reset_token_ttl` (§5.3) vs 30 min `auth.password_reset_ttl` (§7.1, §7.4, SR-07) vs 60 min `password_reset.ttl_minutes` (FEATURE §9).
4. **Invitation TTL.** 7 days `auth.invitation_ttl`, cfg (§5.4, §7.1, SR-08), vs `invitations.ttl_hours`, db, 72 h (FEATURE).
5. **`invitations` columns.** §5.4 `used_at`, `role` vs §7.1 `redeemed_at`, `role_id`, `delivery`, `redeemed_user_id`.
6. **Reset target.** A verified e-mail (ACC-003) vs any e-mail (§5.3). §7.1 sets `email_verified_at` by completing a reset, which is circular.
7. **Common passwords.** SHOULD (§5.3) vs MUST, "common and breached" (SR-04).
8. **Token TTL key and bounds.** The key is `auth.token_ttl`, `auth.access_token_ttl` or `auth.token_ttl_s`. Maximum 900 s (§5.8) vs 10 min (SR-44); leeway 30 s (§5.8) vs ≤ 60 s (SR-43).
9. **Signing key.** One `auth.token_signing_key` (§7.4), or `auth.signing_key_file` "required" (FEATURE), vs a rotating keyring with `auth.key_rotation_days` and `admin rotate-keys` (§5.8, SR-45), which §7.4 does not list.
10. **Revocation push latency.** 1 s (§4.4, §5.8) vs 5 s (SR-43). ADR 0008's `ctl.revocations` carries only `cert_serials`.
11. **Session effect of a role change.** Rotate (§5.5), revoke on role removal (§5.5), revoke on downgrade only (ACC-006, AUTH-003), or a new id on any privilege change (SR-01).
12. **Deletion.** "Delete or anonymise" (AUTH-009) vs "remove personal data, keep a pseudonymous id" (ACC-009). Usernames already sit in audit rows that cannot be updated.
13. **Bootstrap.** `admin create-user --admin` (§5.1) vs `auth.bootstrap_admin` (§7.4, `origin = config`) vs the setup URL and `user add` (AUTH-018).
14. **Setup network.** "Local network only" for the setup link (AUTH-018) vs SR-14's ban on "is private" classifications.
15. **SMTP keys.** `smtp.tls` is `off` (§5.3), `none` (§7.4, FEATURE), and `implicit` vs `tls`, while SR-09 requires TLS. `smtp.username` vs `smtp.user`.
16. **Password change scope.** AUTH-007 revokes the other sessions vs §5.5 "password change revokes all".
17. **`/me` endpoints.** §6.10 `/me` PATCHes only the display name; ACC-004, ACC-005 and ACC-009 need e-mail, sessions, export and delete endpoints that are not listed.
18. **Listener grant.** ACC-006 "assign `listener`" vs §7.1 "listener is implicit, no row".
19. **Token minting paths.** ACC-007 mints "or through the events socket" vs §5.8's two minting paths (authz and `/auth/token`).
20. **`user list --all`.** AUTH-011's `--all` is undefined.
21. **Change notices.** SR-04 requires notifying the account e-mail on password or e-mail change; no ticket mentions it.
22. **Invitations while signed in.** ACC-002's Rights table leaves Listener and Operator as "—", so acceptance while signed in is undefined.
23. **Pseudonymous id.** ACC-009's "pseudonymous id" vs UUIDv7 ids, which encode the creation time.

## Consequences

- `must_change_password` is enforced: ADR 0009 open point 19 is closed.
- A fresh installation has no default credential: the first admin comes from the setup link or the CLI (SR-11).
- The a11y job seeds an admin and checks pages that need a session (`signed-in` in `urls.txt`).
- PR 2 depends on the outcome of grid-2 for ACC-007 and on adm-1 for the settings store and audit retention.
