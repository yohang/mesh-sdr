# ADR 0003: App shell with templ + htmx

- Status: Accepted
- Date: 2026-10-06
- Deciders: project owner
- Spike: SPK-06 (#6). Unblocks UI-001 (#70) and AUTH-001 (#28). Related: UI-006 (#75), UI-008 (#77), UI-009 (#78), AUTH-019 (#46).

This ADR comes from a spike. It describes the options, the spike's recommendation and the owner's **Decision**. It does not change the product specification. Divergences from it are listed under "Divergences from the specification".

## Context

We need an app-shell skeleton that later UI work can build on. It covers:

- **Navigation (UI-006).** A persistent top bar holds the site name, the sections Receiver / Map / Decodes / Files / Admin, the UTC clock, the notifications area and the user menu. Navigation MUST be client-side and MUST NOT reload the receiver audio. Below 768 px the nav becomes a bottom tab bar or a menu. Sections are shown according to the user's role.
- **Responsive layout (UI-007).** Mobile is < 768 px, tablet is 768–1199 px, desktop is ≥ 1200 px.
- **Theming (UI-001 / UI-008).** Colors, spacing and typography come from design tokens with a light set and a dark set. The admin sets `ui_theme` = `light` | `dark` | `auto`, where `auto` follows `prefers-color-scheme`. Users cannot override it. Canvases read the same tokens. `prefers-reduced-motion` turns off non-essential animation.
- **Accessibility (UI-009).** WCAG 2.1 AA: landmarks, headings, keyboard access with visible focus, contrast, color never the only cue, throttled `aria-live` regions.
- **Security baseline (TECHNICAL_SPEC §2.3).** A strict CSP, `frame-ancestors 'none'`, `nosniff` and `Referrer-Policy: same-origin`. Every front-end asset is self-hosted. Untrusted text is always rendered as text.
- **Cookie (§5.6).** `__Host-rx_session` (or `rx_session` when `tls.mode = off`), with `HttpOnly; Path=/; SameSite=Lax`.
- **CSRF (§5.7, AUTH-019).** Every unsafe method carries `X-CSRF-Token` = HMAC(session id, `csrf_secret`). Anonymous unsafe endpoints use a double-submit token bound to a pre-session cookie. `Origin` (or `Referer`) is checked against `hub.url` + `gateway.extra_origins` on unsafe requests and on WS upgrades. JSON endpoints require `Content-Type: application/json`. AUTH-019 itself says "JSON-only bodies with a strict `Content-Type` check".

Owner decisions taken before this spike (not revisited here):

1. CSRF is hand-written. It uses a session-bound synchronizer token sent in a request header (from htmx, via `hx-headers` or the htmx config/events), plus stdlib `net/http.CrossOriginProtection` as defence in depth.
2. The theme mode is set by the admin only. There is no user override.
3. No new third-party Go dependency without the owner. The stdlib and `golang.org/x` are fine.

Stack constraints: templ, htmx 4.0.0 (vendored), Tailwind v4 standalone CLI (no Node), chi, embedded assets.

### What the prototype does (branch `spike/spk-06-app-shell`)

The prototype is built into the real app. Its shortcuts are marked `// SPIKE:` in the code.

| Concern | Files |
|---|---|
| Shell layout, nav, a11y landmarks | `internal/web/templates/layout.templ`, `shell.go` |
| Skeleton pages, CSRF demo form, 404 page in the shell | `internal/web/templates/pages.templ`, `receiver.go` |
| Design tokens, theme, breakpoints, focus, reduced motion | `internal/web/static/css/input.css` |
| CSP nonce and security headers | `internal/http/security.go` |
| Fake session, CSRF middleware, JSON-only check | `internal/http/csrf.go` |
| Page handlers, hardcoded theme constant | `internal/http/pages.go`, `server.go` |
| Shell JS (ES modules): CSRF hook, post-navigation a11y, islands | `internal/web/static/js/` |
| Tests (pages, nonce, CSRF matrix, JSON escaping) | `internal/http/server_test.go` |

Checked in Chromium 154 with htmx 4.0.0, with the strict CSP below and no CSP violations:

- **Audio survives navigation.** A WebAudio test tone started in the top bar keeps playing across boosted navigation and across back/forward (same document, same element). The URL, `<title>`, `aria-current`, focus and the live announcement all update.
- **htmx 4 needs no `'unsafe-inline'` style.** htmx 4 injects its indicator CSS as a constructable stylesheet (`document.adoptedStyleSheets`), and `style-src 'self'` does not block that.
- **`hx-on` is blocked.** It is refused by the CSP (`script-src` violation) and its handler does not run, so the failure is closed.
- **Inline code is blocked.** Inline `<script>` and `style=""` attributes are blocked as expected.
- **Modules load with a nonce-only policy.** Static and dynamic `import()` of same-origin modules from the nonce'd entry module work with `script-src 'nonce-…'` alone (no `'self'`).
- **CSRF and JSON body work.** The htmx POST sends `X-CSRF-Token` and a JSON body. The server answers 403 for a missing, wrong or cross-site token and 415 for a non-JSON body.
- **Tokens resolve for JS.** Theme tokens resolve to concrete colors through `getComputedStyle` (for canvases) in light, dark and auto.
- **Mobile layout works.** Below 768 px the nav is a bottom tab bar.

`go build`, `go vet`, `golangci-lint` and `go test` pass in Docker.

## Options

### 1. Layout and client-side navigation (UI-006)

The constraint that drives the design: the receiver audio must not stop. Whatever the swap mechanism, **the audio engine (AudioContext, media WebSocket, decoders) must be owned by code that lives outside the swapped region.** It is a module loaded once by the shell. The Receiver page UI attaches to it and detaches from it. The prototype's `<msdr-audio-probe>` stands in for that engine.

| Option | How | Pros | Cons |
|---|---|---|---|
| **1A. `hx-boost` on `<body>`, swap `#main` (prototyped)** | `<body hx-boost:inherited='target:"#main" select:"#main" swap:"outerHTML"'>`. The server always returns the full page. htmx keeps only `#main`. `<main hx-history-elt>` limits back/forward restores to `#main`. | One URL gives one representation (no `Vary`, cache-safe). Every link and form is covered by default, content links included. Works without JS (plain links). `<title>` is updated. Back/forward re-fetches and swaps only `#main` (htmx 4 has no history cache). | Each request transfers the full page HTML (small). The nav is not swapped, so `aria-current` and focus are synced by `navigation.js` (~30 lines). Links that must reload the page (login/logout, downloads) need `hx-boost="false"` or a response header such as `HX-Refresh`/`HX-Location`. |
| 1B. Like 1A, plus fragment rendering | The server renders only `#main` when `HX-Request-Type: partial`. Drop `hx-select`, because htmx 4 marks requests that use `select` as `full`. | Fewer bytes, less rendering. | Two render paths per page. Needs `Vary: HX-Request, HX-Request-Type`. Can be added later on top of 1A without breaking it. |
| 1C. Explicit `hx-get` + `hx-target="#main"` + `hx-push-url` on nav links only | Per-link attributes, no boost. | Explicit. | Verbose. Content links and forms are not covered: any plain link reloads the page and cuts the audio. |
| 1D. Default boost (swap `<body>`) + `hx-preserve` on the player | The whole body is swapped and the player element is preserved by id. | The nav is re-rendered by the server too. | Every page must contain the preserved element with the same id. Preserving state relies on `moveBefore` (Chromium only today). Other browsers re-insert the node, which can reset media/canvas/iframe state. More layout churn. |
| 1E. Full page loads, audio in a separate window/worker | — | — | Audio cannot run in a worker. A pop-up player breaks P6 ("no floating windows"). Rejected. |

**Mobile nav.** The prototype uses a single `<nav>` element. CSS turns it into a fixed bottom tab bar below `md` (48 rem = 768 px) and an inline top-bar menu from `md` up. The other option is a hamburger/disclosure menu below 768 px, which needs a little JS and the disclosure ARIA pattern. Tailwind breakpoints are redefined as `md` = 48 rem and `xl` = 75 rem (1200 px) to match UI-007.

**Errors during navigation.** htmx 4 swaps 4xx/5xx responses too (the 404 page is shown inside `#main`), so error pages must be rendered in the shell. The prototype does this for 404.

### 2. Design tokens and theme mechanism (UI-008 / UI-001)

The server writes the theme mode on the root element: `<html data-theme="light|dark|auto">` plus `<meta name="color-scheme">`. There is no JS and no flash of the wrong theme. The prototype hardcodes `auto` (`prototypeTheme` in `internal/http/pages.go`).

| Option | How | Pros | Cons |
|---|---|---|---|
| **2A. `light-dark()` + `color-scheme` (prototyped)** | Each token is defined once: `--msdr-color-bg: light-dark(#f7f8fa, #0d1117)`. `:root { color-scheme: light dark }` gives auto; `[data-theme=light\|dark]` forces one scheme. Color tokens are registered with `@property` (`<color>`), so `getComputedStyle` returns a resolved `rgb()` for canvases. | Single source per token. Native controls and scrollbars follow the theme. Canvases read the same tokens (verified). | Needs `light-dark()` (Chrome 123, Firefox 120, Safari 17.5) and `@property` (Firefox 128). Older browsers get broken colors unless we add a fallback. |
| 2B. Two token blocks | `:root` (light), `[data-theme=dark]`, and `@media (prefers-color-scheme: dark) { [data-theme=auto] {…} }`. | Widest browser support. | The dark set is written twice (auto and dark), or generated. |
| 2C. Tailwind `dark:` variant per element | `@custom-variant dark` keyed on `data-theme` + media. | Familiar Tailwind usage. | Duplicates classes in every template. Canvases cannot read it. Not token-driven. |

**Layering in all three options.** Framework-neutral `--msdr-*` custom properties are the source of truth, readable by JS and canvases. Tailwind `@theme inline` maps them to utilities (`bg-surface`, `text-fg-muted`, …).

**Re-reading tokens.** In `auto` mode, canvases must re-read the tokens on a `matchMedia('(prefers-color-scheme: dark)')` change.

**When an admin change applies.** A theme change applies on the next full page load. Boosted navigation does not touch `<html>`.

**Placeholder values.** The token values are placeholders. Contrast must be checked against UI-009 once the palette is designed.

### 3. CSP and nonce strategy with htmx 4

The prototype's policy, sent on every response:

```
default-src 'none'; script-src 'nonce-<per-request>'; style-src 'self'; img-src 'self' data:;
font-src 'self'; connect-src 'self'; media-src 'self' blob:; manifest-src 'self';
base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'
```

How the nonce flows:

- **Generation.** The nonce is 128 bits from `crypto/rand.Text()`, generated by middleware for each request.
- **Hand-off to templates.** It is stored with `templ.WithNonce`. Templates use `nonce={ templ.GetNonce(ctx) }` on the two `<script>` tags (htmx, `shell.js`).
- **JSON scripts.** `templ.JSONScript` adds the nonce automatically. That is harmless, since `application/json` never executes.

What htmx 4 does under this policy (verified):

- **Indicator styles.** Indicator CSS uses constructable stylesheets. No inline-style nonce is needed, and `inlineStyleNonce` is gone in v4.
- **Eval-based features.** `allowEval` was removed. Features that evaluate JS (`hx-on:*`, `js:` expressions, e.g. in `hx-vals`) use `Function`, which the CSP blocks.
- **Inline scripts in fragments.** An inline `<script>` in a swapped fragment carries another request's nonce and is blocked.

Options:

| Option | Pros | Cons |
|---|---|---|
| **3A. Nonce-only `script-src`, no eval-based htmx features (prototyped)** | Strictest. A same-origin file (for example a Files upload) cannot be loaded as a script. Behaviour lives in ES modules and custom elements. | Template authors must not use `hx-on`, `js:` or inline scripts. A failure is silent apart from a console CSP violation, so this needs a lint/test rule or CSP reporting. |
| 3B. `script-src 'self' 'nonce-…'` | Simpler mental model. | Any same-origin file served with a JS-compatible type becomes executable. That matters for user content (Files) unless it is served from another origin or with `Content-Disposition: attachment` + `nosniff`. |
| 3C. Vendor the htmx `hx-csp` extension (`safeEval`, `hx-nonce`, Trusted Types policy) | Allows `hx-on`/`js:` without `'unsafe-eval'`. Rewrites nonces of inline scripts in fragments. Fails closed. | One more vendored JS asset (not a Go dependency) and more htmx surface to learn. Not needed if we stick to 3A. |
| 3D. `'unsafe-eval'` | — | Contradicts "strict CSP" (§2.3). Rejected. |

Possible additions:

- `require-trusted-types-for 'script'` (Trusted Types; htmx 4 core works with a pass-through policy, hx-csp ships one).
- A `report-to` / `report-uri` endpoint that logs CSP violations at `Warn`.
- `img-src` entries for map tile providers later (MAP), driven by config.

**Template rules that follow from 3A:**

- No `style=""` in templates. Use Tailwind classes, or CSSOM from JS, which is allowed.
- No templ `css`/`script` components.
- No inline `<script>` except `templ.JSONScript`.

### 4. CSRF integration with htmx

The mechanism is already decided (see Context). Only the integration remains open.

**Server side (prototyped).**

- **Middleware order:** `CrossOriginProtection` → session → token check. Only unsafe methods are checked. A failure answers `403` and is logged once at `Warn`.
- **Token:** `base64url(HMAC-SHA256(csrf_secret, session id))`, compared with `hmac.Equal`.
- **Fake session:** in-memory, `// SPIKE`. It gives every visitor a session, standing in for both the real session and the anonymous pre-session.

**Token delivery to the page.**

| Option | Pros | Cons |
|---|---|---|
| **4A. `<meta name="csrf-token">` in the shell + one JS hook (prototyped)** | One place for htmx and for islands (`apiFetch`). Sent only on unsafe, same-origin requests. The token stays valid across boosted navigation because the session does not change. | ~40 lines of JS (`csrf.js`). |
| 4B. `<body hx-headers:inherited='{"X-CSRF-Token":"…"}'>` | Pure htmx, no JS. | Sent on every htmx request, GETs included (htmx 4 fetches same-origin only by default, so it never leaves the origin). Does not cover island `fetch`. Mixes with per-element `hx-headers` (needs `:append`). |
| 4C. `GET /api/v1/auth/session` at startup (the §5.7 wording) | Same path as non-HTML clients. | Extra round trip before the first unsafe request. Can coexist with 4A: the API stays the source for API clients, and the meta tag is a rendering of the same token. |

**htmx 4 detail.** Header and body changes must be made in `htmx:before:request`, not `htmx:config:request`. After `config:request`, htmx 4 rebuilds the body as `URLSearchParams`.

**JSON-only bodies.** htmx sends `application/x-www-form-urlencoded` by default.

| Option | Pros | Cons |
|---|---|---|
| **4D. The shell hook converts every unsafe htmx body to JSON (prototyped, `// SPIKE`)** | Matches AUTH-019 literally. A cross-site form cannot produce `application/json` without a CORS preflight. | No-JS form posts are impossible (the app needs JS anyway). File uploads (Files) need a multipart opt-out, which breaks "JSON-only". Every handler decodes JSON. |
| 4E. JSON required only on `/api/v1` JSON endpoints; htmx HTML endpoints accept urlencoded | Matches the §5.7 wording "JSON endpoints MUST require…". Plain forms and multipart uploads work. | The header token is then the main CSRF defence for HTML endpoints (still strong: a cross-site form cannot set custom headers). Deviates from AUTH-019's "JSON-only bodies". |
| 4F. A vendored htmx JSON-encoding extension | Declarative per element. | Another vendored asset. Equivalent to 4D. |

**CrossOriginProtection vs §5.7.**

- **Origin check.** It compares the `Origin` host with the `Host` header, unless `AddTrustedOrigin` is used. Behind Caddy, `Host` must be preserved, or `hub.url` + `gateway.extra_origins` registered as trusted origins.
- **Requests without headers.** It lets through requests that have no `Sec-Fetch-Site` and no `Origin` (non-browser clients). §5.7 requires `Origin`/`Referer` to match on every unsafe request.
- **WebSocket upgrades.** It does not cover WS upgrades, which are GETs.

So the explicit allow-list check of `Origin`/`Referer` in §5.7 remains to be implemented next to it, at least for the WS upgrade.

**Lifecycle.**

- **Login/logout.** The session id rotates on login, so the token changes. Login and logout must be full page loads (`hx-boost="false"` or `HX-Refresh`/`HX-Redirect`), and the new page renders the new token.
- **Mid-page expiry.** If the session expires while the page is open, unsafe requests get 403. The shell should then show a "session expired, reload" notification (UI-011).

### 5. Passing server data to JS islands

| Option | Pros | Cons |
|---|---|---|
| **5A. `templ.JSONScript` + custom element (prototyped)** | Inert `<script type="application/json">`. `encoding/json` escapes `<`, `>`, `&`, so `</script>` cannot break out (tested). The custom element upgrades whenever htmx inserts it (navigation, history restore, fragments) with no htmx event wiring. `disconnectedCallback` cleans up timers and subscriptions. | Data is duplicated in HTML for large payloads. |
| 5B. `data-*` attributes (JSON in an attribute) | Simple for small configs, escaped by templ. | Awkward for nested or large data. |
| 5C. The island fetches its data over REST/WS on connect | Same path as live updates. Smaller HTML. | Extra round trip and a loading state on first paint. |
| 5D. Init functions on `htmx:after:process` / `htmx.onLoad` instead of custom elements | No Web Components. | Manual init/teardown bookkeeping. Easy to leak or double-init on history restore. |

**Rules.**

- Islands render untrusted text with `textContent` only. In the prototype, a hostile device name is rendered as text.
- Payloads are dedicated view DTOs, never domain types or secrets (§2.3: for example, map API keys are never sent).
- JS is plain ES modules served from `/static/js/`, no bundler. The entry module `shell.js` imports the islands.

### 6. Accessibility baseline (UI-009)

Prototyped:

- **Language.** `lang="en"`.
- **Landmarks.** `header`/banner, `nav aria-label="Main"`, `main#main`, `footer`/contentinfo. One `h1` per page.
- **Skip link.** A skip link to `#main` (`tabindex="-1"`), visible on focus.
- **Focus styles.** Global `:focus-visible` outline (3 px) from a dedicated `--msdr-color-focus` token, in both themes.
- **Current section.** Marked with `aria-current="page"`, using color plus weight plus a bar, so color is never the only cue.
- **After boosted navigation.** Focus moves to `#main`, scroll goes to the top, and the new page heading is announced in a polite live region (`#msdr-announcer`).
- **Clock.** The UTC clock is deliberately not a live region.
- **Reduced motion.** A global `prefers-reduced-motion: reduce` reset of animations and transitions. htmx transitions are off by default.
- **Zoom.** The viewport does not disable zoom (UI-004).
- **Tests.** Go tests assert landmarks, skip link, `aria-current` and the theme attributes.

Not covered: automated WCAG checks (axe-core / Lighthouse / pa11y). They need a browser runtime, and the project has no Node.

## Recommendation (from the spike)

This is the spike's recommendation, kept for the record. Where it differs from the Decision below, the Decision wins.

1. **Navigation: 1A.** Use `hx-boost` on `<body>` with a boost config that swaps `#main` and `hx-history-elt` on `<main>`. Render full pages, and put the audio engine outside `#main`.
2. **Tokens: 2A.** Use `light-dark()`, `@property` color tokens and the Tailwind `@theme inline` mapping.
3. **CSP: 3A.** Use a nonce-only `script-src` and no eval-based htmx features.
4. **CSRF: 4A (meta tag), with 4C kept for the API.** Add an explicit `Origin`/`Referer` allow-list next to `CrossOriginProtection`. Bodies (4D vs 4E) were left to the owner.
5. **Islands: 5A.**
6. **Accessibility:** adopt the baseline.

## Decision

The owner decided on 2026-10-06:

1. **Navigation: 1A.** Use `hx-boost` on `<body>`, configured to swap `#main` (`hx-select="#main"`, `outerHTML`), with `hx-history-elt` on `<main>`.
   - **Full pages for navigation.** Boosted navigation gets the full page from the server.
   - **Audio outside `#main`.** The receiver audio engine and other long-lived resources live in shell-level modules outside `#main`.
   - **Full reloads.** Login, logout and downloads are full page loads (`hx-boost="false"`, or `HX-Refresh`/`HX-Redirect`).
   - **Mobile nav.** Below 768 px the nav is a **bottom tab bar** (CSS only).
2. **htmx fragments.** A fragment uses the same URL as its page. The handler returns the fragment when the `HX-Request` header is present. Actions live on page-scoped paths (for example `POST /admin/...`), not under `/api/v1`. See "Consequences" for how this combines with boosted navigation.
3. **Tokens and theme: 2A.**
   - **Tokens.** `--msdr-*` tokens are defined once with `light-dark()`, set by `color-scheme` from the server-rendered `data-theme`. Color tokens are registered with `@property`, and Tailwind maps them with `@theme inline`.
   - **Browser floor.** The 2024 browser floor is accepted: Chrome/Edge 123+, Firefox 128+, Safari 17.5+.
   - **Theme changes.** A change of `ui_theme` applies on the next full page load. There is no live update.
4. **CSP: 3A.**
   - **Policy.** `script-src` is nonce-only (per-request nonce via `templ.WithNonce`), with no `'self'` and no `'unsafe-eval'`.
   - **Banned in templates.** `hx-on:*` and `js:` expressions are **banned for good**, as are inline `<script>` (except `templ.JSONScript`), `style=""` and templ `css`/`script` components.
   - **Deferred.** Trusted Types and a CSP violation report endpoint are deferred to M5 (Hardening).
5. **CSRF token delivery: 4C.** The token comes only from the session API, `GET /api/v1/auth/session`. There is **no meta tag**.
   - **Client.** Shell JS fetches the token before the first unsafe request and adds `X-CSRF-Token` to unsafe same-origin htmx requests (in `htmx:before:request`) and to island `fetch` calls.
   - **Anonymous pre-session.** Login, password reset and invitation acceptance get a double-submit token bound to a pre-session cookie, through the same endpoint.
6. **Bodies: 4E.** `Content-Type: application/json` is required only on `/api/v1` JSON endpoints. htmx HTML endpoints accept `application/x-www-form-urlencoded` and `multipart/form-data`, and are protected by the CSRF header.
7. **Origin check.** Use the stdlib `http.CrossOriginProtection` only (with `AddTrustedOrigin` for `hub.url` and `gateway.extra_origins` when the hub sits behind a gateway that rewrites `Host`). There is no hand-written `Origin`/`Referer` allow-list for HTTP requests.
   - **WebSocket upgrades** are GETs, so `CrossOriginProtection` does not cover them. They need their own origin check: the origin verification of `coder/websocket`'s `Accept` (`AcceptOptions.OriginPatterns`), the library chosen in SPK-07.
8. **Islands: 5A.** `templ.JSONScript` carries the initial state. Islands are custom elements in ES modules under `/static/js/`, and render untrusted text with `textContent` only. Live updates come over the WebSocket.
9. **Accessibility.** The baseline in section 6 is adopted as the shell contract. Automated checks use **axe-core in a CI-only container**. Node stays out of the app image and the dev image.

### Divergences from the specification

These are recorded here only. No spec change and no GitHub issue were made.

- **TECHNICAL_SPEC §5.7, Origin check.** §5.7 requires the `Origin` (or `Referer`) header to match `hub.url` + `gateway.extra_origins` on every unsafe request and every WS upgrade. The decision uses `http.CrossOriginProtection` instead. It relies on `Sec-Fetch-Site`, or on comparing the `Origin` host with `Host`. It lets through requests that carry neither header (non-browser clients) and never falls back to `Referer`. Those requests still need a valid session-bound CSRF token. WS upgrades keep an origin check, through `coder/websocket`.
- **AUTH-019 (#46), JSON-only bodies.** AUTH-019 asks for "JSON-only bodies with a strict `Content-Type` check". The decision requires JSON only on `/api/v1` JSON endpoints, which matches the §5.7 wording ("JSON endpoints MUST require `Content-Type: application/json`"). htmx HTML endpoints accept form and multipart bodies. On those endpoints the CSRF header is the defence.
- **§5.7, token source.** No divergence: the token comes from `GET /api/v1/auth/session`, as §5.7 says.

## Questions raised by the spike (resolved)

| # | Question | Resolution |
|---|---|---|
| 1 | JSON-only bodies everywhere or only on `/api/v1`? | Only on `/api/v1` (4E). |
| 2 | CSRF token in a meta tag or from the session API? | Session API only (4C). The pre-session token uses the same endpoint. |
| 3 | Browser floor for `light-dark()`/`@property`? | 2024 floor accepted (2A). |
| 4 | Nonce-only `script-src`, and `hx-on`/`js:`? | Nonce-only (3A). `hx-on`/`js:` banned for good. No `hx-csp`. |
| 5 | Trusted Types and CSP reporting? | Deferred to M5. |
| 6 | Automated a11y checks without Node? | axe-core in a CI-only container. |
| 7 | Mobile nav pattern? | Bottom tab bar. |
| 8 | Where do htmx action/fragment endpoints live? | Same URL as the page, fragment when `HX-Request` is present. Actions on page-scoped paths. |
| 9 | Theme change in open tabs? | Applies on the next full page load. |
| 10 | `CrossOriginProtection` only, or an explicit Origin/Referer allow-list? | `CrossOriginProtection` only. WS upgrades checked by `coder/websocket`. Divergence recorded above. |

## Consequences

- **Shell persistence.** Everything outside `#main` (top bar, audio engine, clock, notifications, the WebSocket connections) survives navigation. Feature pages must not own long-lived resources in their DOM. They attach to shell-level modules.
- **Full page vs fragment.** Boosted navigation also sends `HX-Request`, and its target is `#main` with `hx-select`. So a handler must return the **full page** for boosted requests (`HX-Boosted: true`, or `HX-Request-Type: full`) and a **fragment** for other htmx requests.
  - **Caching.** Responses that differ by these headers must send `Vary: HX-Request, HX-Boosted` (or not be cacheable).
  - **Shared helper.** A shared render helper will make this decision, so that pages do not reimplement it.
- **Shell data.** Shared shell data (theme, role-filtered nav) is built per request. It will move from the prototype's `pages.shell()` to a shell/UI module wired in `internal/wire`.
- **CSRF in JS.** The shell JS owns the CSRF token. It fetches `GET /api/v1/auth/session` lazily, caches the token for the page's lifetime, and re-fetches after a 403 caused by an expired or rotated session.
  - **Session expired.** If the session itself has expired, the shell shows a "session expired, reload" notification (UI-011).
  - **Login/logout.** Both are full page loads, so the token cache starts fresh.
- **No JSON conversion.** htmx forms post as form or multipart bodies. Nothing converts them to JSON.
- **Template rules.** No inline scripts or styles, no `hx-on`/`js:`, no templ `css`/`script` components. A lint/test rule over `.templ` files enforces this.
- **Error pages.** Error pages (403/404/500) render inside the shell, because htmx 4 swaps them into `#main`.
- **CI.** CI gains an accessibility job: axe-core in a dedicated container against the running app. The app and dev images stay Node-free.
- **Prototype vs decision.** The prototype on branch `spike/spk-06-app-shell` stays as a reference for the UI epic and **does not match every decision**. It renders a `<meta name="csrf-token">` (decided: session API). It converts every unsafe htmx body to JSON and enforces `requireJSON` on an htmx endpoint (decided: JSON only on `/api/v1`). It always renders full pages (decided: fragment when `HX-Request` is present, except for boosted navigation). It also keeps the `// SPIKE` shortcuts:
  - the fake in-memory session store, to be replaced by DB sessions (AUTH-003/004): hashed id, persisted `csrf_secret`, `__Host-` cookie with TLS, and a pre-session for anonymous visitors;
  - the hardcoded theme, to be replaced by the `ui_theme` setting (UI-001);
  - the nav, which shows every section, to be gated by role (UI-006);
  - trusted origins, still to be read from config;
  - the audio probe and the demo form/endpoint, to be removed;
  - the skeleton pages, which move to their modules' `http/` packages.
- **Dependencies.** No new Go dependency in the app shell. htmx stays the only vendored JS. `coder/websocket` comes from SPK-07.

## References

- Issues: SPK-06 #6, UI-006 #75, UI-008 #77, UI-001 #70, UI-009 #78, AUTH-019 #46, AUTH-001 #28.
- `docs/spec/FEATURE_SPEC.md` §2 (P6), §6.5 UI shell & customisation.
- `docs/spec/TECHNICAL_SPEC.md` §2.3 Security baseline, §5.5 Sessions, §5.6 Cookie, §5.7 CSRF, §5.14 Login.
- htmx 4: <https://four.htmx.org/docs/whats-new-in-htmx-4> (removed `allowEval`, constructable indicator styles, explicit `:inherited`, `HX-Request-Type`, no history cache); `hx-csp` extension: <https://four.htmx.org/extensions/hx-csp>; events guide (`htmx:config:request`, `htmx:before:request`): <https://four.htmx.org/docs/htmx-events-guide>.
- templ CSP nonce and `JSONScript`: <https://templ.guide/security/content-security-policy>, <https://templ.guide/syntax-and-usage/script-templates>.
- Go `net/http.CrossOriginProtection` (Go 1.25): <https://pkg.go.dev/net/http#CrossOriginProtection>.
- `coder/websocket` `AcceptOptions` (origin verification on upgrade, SPK-07): <https://pkg.go.dev/github.com/coder/websocket#AcceptOptions>.
- axe-core: <https://github.com/dequelabs/axe-core>.
- CSS `light-dark()`: <https://developer.mozilla.org/en-US/docs/Web/CSS/color_value/light-dark>; `@property`: <https://developer.mozilla.org/en-US/docs/Web/CSS/@property>.
- WCAG 2.1: <https://www.w3.org/TR/WCAG21/>.
