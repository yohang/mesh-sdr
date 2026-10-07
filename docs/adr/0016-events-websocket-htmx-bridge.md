# ADR 0016: Events WebSocket to htmx bridge

- Status: Accepted
- Date: 2026-10-07
- Deciders: project owner
- Spike: SPK-05 (#5). Unblocks DIAG-002 (#212) and FIL-001 (#282). Builds on ADR 0003, 0004, 0007, 0009, 0010 and 0011.

This ADR comes from a spike. The owner accepted the spike's recommendation; the answers to every question raised by the spike are in "Decision". The product specification is not changed: the inconsistencies found are recorded under "Spec issues" only.

## Context

TECHNICAL_SPEC §6.6 defines a hub events WebSocket, `GET /api/ws` with subprotocol `rx.v1` (§6.1):

- **Transport and envelope.** JSON envelopes `{v, type, id, ts, payload}` (§6.2), the 5 s `session.hello` → `session.welcome` handshake, `ack`/`error` frames and the 44xx close codes.
- **Client messages.** `session.hello`, `sub {topics, since?}`, `unsub {topics}` and `presence.heartbeat`.
- **Hub events.** `presence.*`, `map.*`, `decode.new`, `diag.state`, `node.status`, `device.status`, `preset.changed`, `bookmark.changed`, `settings.changed`, `notification`, `files.new` and `session.revoked`, on the topics `presence`, `map`, `decodes:device=<id>`, `diagnostics:device=<id>`, `nodes`, `devices`, `notifications` and `admin.connections`.
- **Security and limits.** Session cookie (or anonymous per policy), Origin checked at the upgrade (§6.9), per-topic access rules, 16 KiB inbound messages, 10 msg/s, a 1 000-event queue per connection, and `since` replay for persistent topics (§6.8).
- **No HTML on the wire.** Every untrusted string is plain text and "no message carries HTML" (§6.1).

The UI is templ + htmx 4. These constraints are already decided:

- **ADR 0003 and ADR 0007.** Boosted navigation swaps `#main`. A fragment uses the same URL as its page. Long-lived resources (WebSockets) live in shell modules outside `#main`. CSP is nonce-only, with `hx-on`/`js:` banned for good. Behaviour lives in ES modules and custom elements, and untrusted text goes through `textContent`. WS upgrades are not covered by `http.CrossOriginProtection`: they are checked by `coder/websocket`'s `OriginPatterns`.
- **ADR 0004.** `coder/websocket` is the library. `internal/protocol/rxv1` is the codec and `rxv1/wsconn` the adapter (426 pre-check, bounded queue, pings).
- **ADR 0009 and ADR 0011.** DB sessions in the `rx_session` cookie (`SameSite=Lax`), `Authorize(role)`, `Principal.CanListen(policy)`. Any privilege change revokes every session of the user.
- **ADR 0010.** The settings `Store` holds an atomic snapshot and `Subscribe` callbacks. `listen_policy` is a live DB setting.
- **Dependencies.** No new third-party Go dependency. htmx stays the only vendored JS.

Issue #5 notes that the htmx `ws` extension does out-of-band HTML swaps, while the spec's socket carries JSON. It asks for a dispatcher that maps JSON events to `htmx.trigger` events, templ fragments refreshed with `hx-trigger="… from:body"`, and naming conventions.

### What the prototype does (branch `spike/spk-05-events-bridge`)

The prototype is built into the real hub. Its shortcuts are marked `SPIKE` in the code.

| Concern | Files |
|---|---|
| Topic value object (`nodes`, `decodes:device=<id>`, …), domain errors | `internal/events/domain/topic.go` |
| In-process broker: publish, attach, all-or-nothing `Subscribe` with an `Authorizer` port, `Recheck`, 32 topics per connection | `internal/events/app/broker.go` |
| `/api/ws` handler: Origin, handshake, `sub`/`unsub`, rate limit, strike counters, session revalidation, graceful shutdown | `internal/events/http/ws.go` |
| `wsconn.AcceptOrigins` (passes `OriginPatterns` to `coder/websocket`) | `internal/protocol/rxv1/wsconn/wsconn.go` |
| `Revalidate` (re-resolves the cookie session of a long-lived request) | `internal/identity/http/longlived.go` |
| Wiring: topic authoriser (identity + `listen_policy`), session adapter, grid `Status.Listen` → `node.status` publisher | `internal/wire/events.go`, `internal/wire/wire.go` |
| Client dispatcher (ES module, shell-owned) | `internal/web/static/js/events.js`, imported by `shell.js` |
| One fragment refreshed by an event: the Admin › Devices table (`/admin/devices`, fragment on `HX-Request`) | `internal/grid/http/admin.templ`, `admin.go` |
| Tests | `internal/events/**/*_test.go`, `internal/wire/events_integration_test.go`, `internal/wire/devices_test.go` |

What was checked:

- **Go tests (pass with `-race`).**
  - Upgrade: 426 without `rx.v1`; 403 for a foreign `Origin`; accepted for the same host and for `hub.url` (a pattern match although `Host` differs).
  - Handshake: `session.hello` must come first; 4408 after the hello timeout.
  - Subscriptions: `sub` → `ack`, then only the events of the subscribed topics are delivered. A forbidden topic gets `forbidden` and nothing is subscribed. Unknown topics and unknown payload fields get `invalid_payload`. A media message type gets `unsupported_type`.
  - Limits: `rate_limited` with `retry_after_ms` above 10 msg/s.
  - Session end: an ended session gets `session.revoked`, then close 4401.
  - Topic re-check: topics that became forbidden are dropped.
  - End to end: a real hub plus a real enrolled node over mTLS. The node coming **online** is pushed as `node.status` to an anonymous subscriber of `nodes`, and `admin.connections` is refused to that subscriber.
  - Fragment: `/admin/devices` returns only the table fragment for a non-boosted htmx request.
- **Chromium (DevTools protocol) against the production image** (the a11y stack with a published port, signed in as admin, strict CSP):
  - **Connection.** The dispatcher opens `/api/ws` under `connect-src 'self'`, with no console message.
  - **Refresh.** Two synthetic `msdr:node.status` events cost one fragment GET (214 bytes; `delay:250ms` debounces them).
  - **Restart.** Restarting the hub closes the socket with 1001 during graceful shutdown. The dispatcher reconnects within about 1 s and fires `msdr:resync`, which refetches the fragment.
  - **Logout.** Logging out from the same browser triggers `session.revoked` and close 4401 at the next re-check (30 s), then `msdr:session.expired`. The dispatcher does not reconnect.
- **Repository checks.** `golangci-lint` reports 0 issues. `make a11y` passes: 21 pages × 3 theme modes × 2 OS schemes × 2 viewports, `/admin/devices` included.

Not prototyped:

- presence, map, `since` replay, snapshots and resume tokens;
- per-recipient payload projection;
- coalescing;
- the islands path;
- multi-tab sharing;
- a UI notice for an ended session.

## Options

### 1. How hub events reach the page

| Option | How | Pros | Cons |
|---|---|---|---|
| **1A. JSON events → DOM events → fragments refetch over HTML (prototyped)** | A shell ES module owns the socket. It fires `msdr:<type>` on `<body>` with `{payload, ts}` (via `htmx.trigger`). A fragment declares `hx-get="<its page URL>" hx-trigger="msdr:<type> from:body …"` and refetches itself. | Matches §6.1/§6.6 (JSON only, no HTML on the socket). Authorisation, projection and escaping stay in the page handler that already renders the fragment for the viewer's role. The socket carries no rendering or per-viewer HTML. Works with nonce-only CSP: no `hx-on`, no inline code. The same events serve islands. | One HTTP round trip per refresh (debounced). The fragment re-renders entirely: `outerMorph` keeps focus and scroll. Events must be ordered after commit and after derived state (see 4). |
| 1B. htmx 4 `hx-ws` extension | htmx 4.0.0 ships it as a separate file (`dist/ext/hx-ws.js`) to vendor. `hx-ws:connect="/url"` opens a socket owned by that element (shared by URL since RC1, no longer). Text messages are HTML swapped by htmx rules, or JSON `{content, target, swap, select}`. Other JSON goes through `htmx:ws:before:message:incoming`, which can cancel the swap. `hx-ws:send` posts form values as JSON with a `headers` object. Reconnect is configurable (`ws.reconnect`, `ws.reconnectMaxDelay`). | Declarative. Built-in reconnect. | Its swap model sends HTML on the socket, which contradicts §6.1. Its outgoing format (`headers` + form values) is not an rx.v1 envelope. The rx.v1 handshake (`session.hello` in 5 s), `sub`/`unsub` and close-code handling would all be custom JS on top of it anyway. The spike found no documented way to set the `rx.v1` subprotocol, and the hub answers 426 without it. One more vendored asset. The connection must sit outside `#main`, so per-page attributes do not map to page-scoped subscriptions. |
| 1C. Server-rendered HTML over our own socket (OOB swaps) | The hub renders templ fragments per connection and pushes them; the dispatcher swaps with `htmx.swap`. | No refetch round trip. | Diverges from §6.1 ("No message carries HTML"). Per-connection, per-role rendering in the events hot path. Fragments then need nonce-free markup and the same authorisation as their pages, in two places. Hard to coalesce. |
| 1D. SSE instead of WS (`hx-sse` or `EventSource`) | A one-way `text/event-stream`, with subscriptions over REST or the query string. | `Last-Event-ID` gives replay for free. No upgrade, so `CrossOriginProtection`-style reasoning applies. | Diverges from §6.1/§6.6 (WS `rx.v1`, `sub` messages, close codes). Subscription changes need extra requests. Over HTTP/1.1 each tab holds one of the 6 connections per origin. Gains little over 1A, since 1A also refetches state after reconnect. |
| 1E. Islands consume events directly | Custom elements listen to `msdr:*` (or a JS bus) and update their DOM with `textContent`. | No round trip. Fits high-rate or append-only views (map, presence counter, decode list tail, diagnostic chips). | Client-side rendering: the HTML templates and the JS can drift. The full payload must be safe for every subscriber of the topic (see 2). |

1A and 1E are complementary: the dispatcher's DOM events serve both. A fragment refetch suits low-rate state (node/device status, file list, settings). An island suits high-rate or append-only data.

### 2. Event payloads: thin or full

| Option | Pros | Cons |
|---|---|---|
| **2A. Thin events (ids plus state) when a fragment refetches (prototyped for `node.status`: `{node_id, status}`)** | No per-recipient projection. What the viewer may see is decided by the HTML handler, which already applies roles, `files.visibility` and listen policy. | Diverges from the §6.6 payloads (e.g. `node.status` lists `version`, `cpu`, `temp_c`, `listeners`…). |
| 2B. Full §6.6 payloads, projected per recipient | Islands and non-browser clients get everything without a refetch. | Needs a projection hook per event type and role. §6.6 already requires one for `node.status` (admin vs public subset), and DIAG-002 requires one for diagnostics ("other users … see only states of sessions they own"). Every projection is a leak risk. |
| 2C. Full payloads published once, with an `Audience` predicate per event | The broker drops events a subscriber may not see (e.g. a `diag.state` of another user's session). | Filters but does not reshape. Still needs 2B for role-dependent fields. |

### 3. Server endpoint design

**Origin and CSRF-equivalent concerns.**

- `wsconn.AcceptOrigins` passes `OriginPatterns` (prototype: `scheme://host` of `hub.url`, later `gateway.extra_origins`). Spike findings on `coder/websocket` v1.8.15:
  - a request **without `Origin`** is accepted (non-browser clients, like `CrossOriginProtection`);
  - an `Origin` whose host equals `Host` is always accepted, **whatever its scheme**;
  - a pattern containing `://` is matched against `scheme://host`, otherwise against the host only (`path.Match`, case-insensitive);
  - a refusal answers 403 before the upgrade.
- Browsers always send `Origin` on a WS handshake, so the cross-site socket hijacking case is covered by the Origin check.
- `SameSite=Lax` keeps the session cookie off cross-site handshakes in current browsers. A same-site sibling subdomain still sends it, so the Origin check remains the defence.
- The hub WS accepts no state-changing message: `sub`, `unsub` and `presence.heartbeat` only read or refresh presence. So no CSRF token is needed on it.

| Option | Pros | Cons |
|---|---|---|
| **3A. Origin check only (prototyped)** | Matches ADR 0003 §7 and §6.9. No extra round trip. | Relies on the Origin allow-list being right behind the gateway (`Host` preserved or `hub.url` registered). |
| 3B. Also require the CSRF token (from `GET /api/v1/auth/session`) in `session.hello` | Defence in depth if the socket later gains writes. | An extra fetch before connecting, and anonymous visitors would need the pre-session token. It adds little while the socket is read-only. |
| 3C. Also reject handshakes without `Origin` when a session cookie is present | Closes the "no Origin" bypass for cookie-bearing non-browser clients. | Non-browser clients holding a cookie are not a CSRF vector anyway. |

**Authentication over the socket's lifetime.** The principal is resolved once, at the upgrade. A session can end afterwards (logout, admin revocation, the role change of ADR 0011, idle or absolute expiry).

| Option | Pros | Cons |
|---|---|---|
| 3D. Periodic re-resolution (prototyped, every 30 s, `identity/http.Module.Revalidate`) | Simple. Also catches expiry. | Up to 30 s late. One session read per connection per period. `Auth.Resolve` records activity, so **an open tab keeps the session alive against the idle timeout** (Decision 4). |
| 3E. Push from identity: a consumer-side `SessionsEnded(ids)` port, as for the node `RevocationPublisher`, closes the matching sockets at once | Immediate `session.revoked`. | Covers revocations, not time-based expiry: keep a slow timer (or the session's known expiry instant) for those. |

`listen_policy` and other settings changes re-authorise the topics (prototype: on the same 30 s timer, `Subscription.Recheck`; better: a settings `Store.Subscribe` callback). Topics that became forbidden are dropped. §6.6 has no message for that, so the prototype sends `error {code: forbidden, re: null, details: {topics}}`.

**Topic authorisation** is an `Authorizer` port of `events/app`, implemented in the composition root from identity (`Authorize`, `Principal.CanListen`) and settings. The prototype is simplified (Decision 7 sets the rule):

- `admin.connections` requires `admin`;
- every other topic follows the global `listen_policy`.

**Limits and back-pressure (prototyped).**

- Read limit 16 KiB, and 10 msg/s via `x/time/rate` (`rate_limited` + `retry_after_ms`).
- Violation strikes in a 60 s window:
  - 10 `invalid_*` → 4400;
  - 10 `forbidden` → 4403;
  - 10 `rate_limited` → 4429 (the threshold is the spike's).
- Outbound: wsconn's byte-bounded queue (1 MiB) → 4413. §6.8 instead says 1 000 events, with map and presence deltas coalesced per key, and `rate_limited` before 4413. That needs a per-topic coalescing queue (the `sendq` model of ADR 0004), not built here.
- `Publish` never blocks: each sink only enqueues.
- `http.Server.Shutdown` does not track hijacked connections, so the module runs a worker that closes every socket with 1001 on shutdown (verified).
- Not built: a cap on concurrent sockets per client address or session. An anonymous page view opens a socket only if the page declares topics (see 5).

### 4. Publishing from modules (in-process bus)

| Option | Pros | Cons |
|---|---|---|
| **4A. Central broker in `internal/events/app` (prototyped). Producers depend on a small `Publisher` port declared in their own `app` package, or keep their existing listener hook, adapted in the composition root.** | Producers stay unaware of WS, topics and rx.v1. One place for fan-out, authorisation and limits. Consumer-side interfaces, no globals (AGENTS.md). | One more module. Event types are strings checked against the rx.v1 catalogue at delivery. |
| 4B. Each module exposes `Listen`/`Subscribe` hooks; the events module registers on all of them | No new port in producers. The grid `Status.Listen` already exists and is how the prototype gets `node.status`. | N different hook shapes. Ordering between listeners is implicit. |
| 4C. Transactional outbox for persistent topics (`decoded_messages`, `decoder_diagnostics`, map) | The row id or timestamp is the `since` cursor, which gives replay after reconnect (§6.6). Survives a crash between commit and publish. | More machinery. Only needed for replayable topics. |

Rules that apply in every case:

- Publish **after commit** (§7.3 rule 3), from the code that committed.
- Publish **after derived state is updated.** The prototype registers the `node.status` publisher after the device registry's `NodeStatusChanged` listener, so a refetch triggered by the event already sees the devices marked offline. Listener order is part of the contract.
- Payloads are dedicated view DTOs (plain text), never domain types.

How each module would emit:

| Module | Event (topic) | Source |
|---|---|---|
| grid | `node.status` (`nodes`) | `Status.Listen` (prototyped) |
| grid | `device.status` (`devices`) | a listener on `Devices` state/report changes (new hook) |
| presets / grid | `preset.changed` (`devices`) | the preset activation use case, after the node acks |
| settings | `settings.changed {keys}` (`devices`) | `Store.Subscribe`: diff the old and new snapshot, keep keys marked `x-public`. The same callback triggers `Recheck` for `listen_policy`. |
| identity | `session.revoked` (always) | the `SessionsEnded` port (3E) |
| files | `files.new` (`notifications`) | after the file row commits (FIL-005) |
| decodes / diagnostics | `decode.new`, `diag.state` (per device) | control-channel ingestion (`decode.batch`, `diag.transition`), after insert; outbox cursor (4C) |
| notifications | `notification` | system notices (node offline, decoder reports to admins), with an audience |

### 5. Client dispatcher

Prototyped in `static/js/events.js`, imported by `shell.js`, about 200 lines:

- **One socket per tab, owned by the shell.** It survives boosted navigation.
- **Lazy connection.** The socket opens only when the document declares topics. Topics are declared by `data-msdr-topics="nodes devices"` on fragments. After every `htmx:after:swap` and `pageshow`, the dispatcher diffs the declared set against the subscribed one and sends `sub`/`unsub`.
- **Events.** `env.type` → `htmx.trigger(document.body, "msdr:" + type, {payload, ts})`. Malformed frames are ignored. Nothing is inserted as HTML.
- **Reconnect.** Full-jitter exponential back-off (1 s → 30 s). After 4429, at least `retry_after_ms`. No retry after 1000, 1003, 1008, 1009, 4400 or 4403. 4426 stops (reload the app). 4401 and `session.revoked` stop and fire `msdr:session.expired`/`msdr:session.revoked`, because reconnecting would silently continue as anonymous. Abnormal closes (1006, 4413, 4503, 1001) retry, as ADR 0004 requires for 1006.
- **Resync.** After a reconnection, `msdr:resync` tells every fragment to refetch, since events may have been missed.

Alternatives, open:

- **Topic declaration.** Shell-wide fixed topics (e.g. `notifications` always) instead of, or on top of, page-declared ones.
- **First-load gap.** Events between the server render and the `sub` ack are lost. The options are: (a) always fire `msdr:resync` after the first ack (one extra GET per page view); (b) render a cursor or revision into the page and pass it as `since`; (c) accept the gap for status-like data. The prototype does (c).
- **Multi-tab.** One socket per tab (prototyped) or one per browser. One per browser means a `BroadcastChannel` leader election or a `SharedWorker`; the latter needs `worker-src 'self'` in the CSP.
- **Hidden tabs.** Keep the socket open, or close it after N minutes hidden and resync on `visibilitychange`.

## Decision

The spike's recommendation is accepted. Each point answers one of the questions the spike raised.

1. **Bridge: 1A, with 1E islands for high-rate views.** The socket carries JSON rx.v1 envelopes only. A shell-owned dispatcher turns each event into an `msdr:<type>` DOM event on `<body>`, and templ fragments refetch themselves over their page URL. Islands (custom elements) consume the same DOM events for high-rate or append-only views (map, presence, decode list tail, diagnostic chips). `hx-ws`, server-rendered HTML on the socket (1C) and SSE (1D) are rejected; `hx-ws` is not vendored.
2. **Payloads: thin events (2A) plus a per-event audience predicate (2C).** Events that make a fragment refetch carry ids and state only; the HTML handler decides what the viewer sees. An event whose existence is private carries an `Audience` predicate and the broker drops it for other subscribers. Per-role projections (2B) are added only for an island that needs a full payload. The §6.6 payload lists are read as "up to" lists.
3. **Socket security: Origin check only (3A).** `OriginPatterns` holds `hub.url` and `gateway.extra_origins` as `scheme://host`. The socket accepts no state-changing message, so there is no CSRF token in `session.hello` (no 3B) and handshakes without `Origin` are not rejected (no 3C). Any future message that changes state reopens this decision.
4. **Session lifetime: identity pushes revocations (3E), plus a slow expiry timer.** Identity exposes the end of sessions (logout, admin revocation, role change, disabled user) through a consumer-side port; the events module closes the matching sockets at once with `session.revoked` then 4401. A slow timer (or the session's known expiry instant) handles idle and absolute expiry. **An open socket does not count as activity**: the check must not record activity, unlike the prototype's `Resolve`.
5. **Dropped topics.** When a topic becomes forbidden mid-connection (`listen_policy` change through the settings `Store.Subscribe` callback, role change), the hub drops it and sends `error {code: forbidden, re: null, details: {topics}}`, as prototyped. Recorded as spec issue 4.
6. **`sub` is all-or-nothing**, as prototyped: one forbidden or invalid topic rejects the whole request and nothing is subscribed.
7. **Topic access.**
   - `admin.connections` requires `admin`.
   - `nodes`, `devices`, `presence`, `notifications` and per-device topics follow §6.6: anonymous access when some device is anonymous-listenable or `public_map` is on, per-device topics checked against the device's effective listen policy. The prototype's global-policy rule is not kept.
   - `files.new` stays on `notifications` and carries an audience predicate applying `files.visibility` (FIL-001). Recorded as spec issue 8.
8. **Diagnostics (DIAG-002).** The session owner's decoder states travel as `diag.state` on the media WS (§6.5). The hub topic `diagnostics:device=<id>` keeps `diag.state` with an audience predicate restricted to the owner of the session, so other users of a shared device never see them. `decoder.diag` is read as `diag.state` (spec issue 1).
9. **First-load gap.** The dispatcher fires `msdr:resync` after the first `sub` ack of a connection as well as after every reconnection: one extra GET per page view.
10. **Topic declaration.** Pages declare topics with `data-msdr-topics`; the socket opens lazily, so anonymous visitors on pages without live parts open no socket. Shell-wide topics are added only when a shell feature needs one (e.g. `notifications` for the notification area).
11. **Multi-tab.** One socket per tab. No `SharedWorker` or `BroadcastChannel` leader for now.
12. **Back-pressure.** wsconn's byte-bounded queue (1 MiB, close 4413) stays until presence and map exist; the §6.8 event-count queue with per-key coalescing ships with them. 10 `rate_limited` strikes in 60 s close with 4429, like the 4400 and 4403 thresholds.
13. **Bus: 4A.** A new `internal/events` module (`domain`, `app`, `http`) owns `/api/ws` and the in-process broker. Producers depend on a consumer-side `Publisher` port (or an existing hook adapted in the composition root), publish after commit and after derived state, with explicit listener order. The outbox (4C) comes with the decodes epic, for `since` replay of `decodes`, `diagnostics` and `map`.
14. **Limits.** At most 32 topics per connection. A cap on concurrent sockets per session and per client address is required before presence ships; its values are set by the implementing ticket.
15. **Compression.** `permessage-deflate` stays off on the hub WS.

## Conventions

- **Event names.** `msdr:<rx.v1 type>`, exactly the §6.6 type (`msdr:node.status`, `msdr:files.new`, `msdr:decode.new`, `msdr:diag.state`, `msdr:device.status`). Shell events use the same prefix with names that are not rx.v1 types: `msdr:resync`, `msdr:events.state` (`{state, code?}`), `msdr:events.error`, `msdr:session.revoked`, `msdr:session.expired`. htmx 4 parses `hx-trigger` names up to the first space, so the colon and dots are safe (checked against the vendored 4.0.0 parser and in Chromium).
- **Event detail.** `{payload, ts}`, the envelope payload untouched. Never HTML; islands render it with `textContent`.
- **Live fragments.** A live fragment is a templ component that is also the page's htmx fragment (same URL, ADR 0007), rooted at an element with a stable `id`. It carries:
  - `data-msdr-topics="<topics>"`: the topics it needs;
  - `hx-get="<page URL>"`;
  - `hx-trigger="msdr:<type> from:body delay:250ms, msdr:resync from:body"`: debounce bursts. htmx trigger filters (`msdr:x[detail…]`) are evaluated JS, which the nonce-only CSP blocks, so filter with per-device topics or on the server instead;
  - `hx-swap="outerMorph"`: keeps focus, selection and scroll.
- **Accessibility.** A refetch announces nothing. Changes users must hear go through the shell's throttled live region (UI-009), never through an `aria-live` on the refreshed fragment.
- **Topics.** `kind` or `kind:device=<id>` with §6.1 identifiers (`events/domain.ParseTopic`). At most 32 per connection (prototype value).
- **Go producers.**
  - Publish after commit and after derived state, with `Type` from the `rxv1` constants and a view DTO payload.
  - The adapter from the module's hook or port to the broker lives in the composition root, until the events module grows its own `wire.go`.
  - Log components: `events.http.ws`, `events.app.broker`.
- **Errors on the socket.** Domain errors map to rx.v1 codes at the boundary:
  - `topic_forbidden` → `forbidden`;
  - `invalid_topic` → `invalid_payload` with `details.path = "topics"`;
  - `too_many_topics` → `capacity_exceeded`.

## Spec issues

Found while prototyping. The spec is not edited.

1. **DIAG-002 vs §6.3/§6.5/§6.6:** `decoder.diag` is not a catalogue type (Decision 8).
2. **§6.6 `session.hello`/`session.welcome` on the hub:** `capabilities` and `resume_token` are defined for the media WS. The hub has nothing to resume, yet `resume_token` is listed as always present. The prototype omits it.
3. **§6.6 `sub` ack `snapshots`:** no topic defines its snapshot shape except `map.snapshot`. The prototype sends `{}`.
4. **No "server unsubscribed you" message** (Decision 5).
5. **`capacity_exceeded`** is defined for demodulators only. The prototype reuses it for too many topics.
6. **Repeated `rate_limited` → 4429:** no threshold (already ADR 0004 spec issue 9).
7. **§6.8 hub queue** "1 000 events" vs the byte-bounded `wsconn` queue, and "`error rate_limited` then 4413" mixes the rate-limit code with the slow-consumer close.
8. **`files.new` on `notifications` and `settings.changed` on `devices`:** the topic choice does not match the visibility rules of those features (Decision 7).
9. **`node.status` "public subset for non-admins"** implies per-recipient projection that the envelope model does not otherwise describe (Decision 2).

## Consequences

- **New module.** `internal/events` (`domain`, `app`, `http`) becomes the owner of `/api/ws`. The prototype's `SPIKE` shortcuts are replaced:
  - topic access per §6.6 (point 7);
  - the identity push port;
  - an activity-neutral session check;
  - an event-count queue with coalescing;
  - `since`/outbox for persistent topics;
  - a socket cap.
- **Producers.** Each feature epic adds its publisher next to its use case, with a test that the event follows the commit. DIAG-002 and FIL-001 follow the conventions above: a `data-msdr-topics` fragment for the Files gallery and the Decoders tab, plus islands for the diagnostic chips (UI-022).
- **Pages.** Pages gain live parts without new JS. The shell gains a "session ended, reload" notice bound to `msdr:session.revoked`/`msdr:session.expired` (UI-011).
- **Generated files.** Fragment templates must stay valid both as part of the page and as a standalone fragment, and `TestTemplateRules` still applies.
- **Dependencies.** No new dependency: `coder/websocket` and `golang.org/x/time` are already in `go.mod`. htmx stays the only vendored JS. `hx-ws` is not vendored.
- **Prototype vs decision.** The branch `spike/spk-05-events-bridge` stays as a reference for the implementing tickets and does not match every point of the decision. It does not fire `msdr:resync` after the first subscription, it polls sessions every 30 s through `Resolve` (which records activity), it uses the global listen policy for every non-admin topic, and it has no audience predicate and no socket cap.

## References

- Issues: SPK-05 #5, DIAG-002 #212, FIL-001 #282, UI-011, UI-022.
- TECHNICAL_SPEC §5.5 Sessions, §5.6 Cookie, §5.7 CSRF, §6.1 Transport, §6.2 rx.v1 (envelope, errors, close codes, handshake), §6.3 catalogue, §6.6 Hub events WS, §6.8 Back-pressure, §6.9 Limits, §7.3 Persistence rules. FEATURE_SPEC DIAG-002, FIL-001, UI-022.
- ADR 0003 (§3 CSP, §7 Origin check for WS), ADR 0004 (coder/websocket, `wsconn`, close-code behaviour), ADR 0007 (render helper, fragments), ADR 0009 and 0011 (sessions, `Authorize`, revocations), ADR 0010 (settings `Store.Subscribe`).
- htmx 4: `hx-ws` extension (<https://four.htmx.org/extensions/hx-ws>, events `htmx:ws:before:message:incoming`, JSON swap override, per-element connection ownership since RC1), `hx-sse` extension (<https://four.htmx.org/extensions/hx-sse>), morph swaps (`innerMorph`, `outerMorph`), trigger modifiers `from:`, `delay:`.
- `coder/websocket` v1.8.15 `AcceptOptions.OriginPatterns` and `authenticateOrigin` (accept.go): <https://pkg.go.dev/github.com/coder/websocket#AcceptOptions>.
- RFC 6455 §10.2 (Origin considerations); OWASP, Cross-Site WebSocket Hijacking.
