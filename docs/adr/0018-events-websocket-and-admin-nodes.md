# ADR 0018: Hub events WebSocket, presence registry and Admin › Nodes

- Status: Accepted
- Date: 2026-10-07
- Deciders: project owner
- Scope: epic part `epic/events-1`: GRID-017 (#25), AUTH-004 (#31), GRID-005 (#13), GRID-009 (#17) and the events WebSocket of GRID-001 (#9).
- Builds on: ADR 0016 (events WebSocket to htmx bridge), and ADR 0003, 0007, 0008, 0009, 0010, 0011, 0012 and 0013.

## Context

ADR 0016 decided how hub events reach the pages. Its prototype (`spike/spk-05-events-bridge`) left these points to the implementing tickets:

- the socket caps;
- the identity push of revocations;
- an activity-neutral session check;
- the topic access rules of §6.6.

This part adds the hub side of `/api/ws`, the presence rows of events sockets, listener counts, and the admin pages of the grid: Admin › Nodes and Admin › Connections. GRID-005 and GRID-009 had their use cases and REST endpoints since ADR 0008. Only their pages were missing (ADR 0008 Q4).

The design review found these gaps:

- Node media sessions reported `connection.opened` and `connection.closed` but never `connection.heartbeat`. The reaper therefore closed every media presence row after 45 s.
- `Auth.Resolve` records activity, and a logout pushed no revocation.
- The grid timings and `retention.connections` were not settings yet (ADR 0008 Q2).
- GRID-009 gives operators read access to Admin › Nodes, while §5.10 and the API (`listNodes`, `getNode`) were admin-only.

## Decision

Numbers refer to the questions of the design proposal. The owner accepted every recommendation.

### Events module (ADR 0016 decisions 1–15)

1. **Module.** `internal/events` holds the events model.
   - `domain`: the `Topic` value object, `Viewer`, `Audience` and the errors.
   - `app`: the `Broker`, the `Admission` and the `Presence` port.
   - `http`: `/api/ws`.
   - The wiring is in `internal/wire/events.go`.
2. **Upgrade.**
   - The rx.v1 subprotocol is checked first (426) and then the `Origin` (403 `origin_denied`), both answered with problem+json. `/api/ws` is under `/api`, so its refusals use the error format of the API.
   - The `Origin` must be the origin of `hub.url` or name the request host. A request without `Origin` is not a browser's and passes (ADR 0016 decision 3). `gateway.extra_origins` is not added until it is needed (Q7).
3. **Socket caps (Q7).** Sockets are capped at 16 per session, 64 per client address and 4 000 for the hub. Upgrades are capped at 30 per minute per client address (§5.12).
   - These are constants (`app.DefaultLimits`), not configuration keys.
   - Refusals answer 429 `too_many_connections`, 429 `rate_limited` with `Retry-After`, or 503 `capacity_exceeded`.
4. **Topic access (ADR 0016 decision 7).**
   - Admin topics need `admin`, including `admin.allowed_networks`.
   - Per-device topics need listen permission on the device, given by the device's effective listen policy. An unknown or disabled device is refused.
   - The other topics are open to signed-in users. Anonymous visitors get them only while some enabled device is anonymous-listenable; `public_map` does not exist yet.
   - The upgrade itself is not refused to anonymous visitors: the receiver page is public in M0, and a socket with no topic carries nothing.
5. **Topics without a producer (Q6).** `notifications`, `map`, `decodes:*`, `diagnostics:*` and `admin.connections` are accepted and authorised, but nothing is published on them yet. `settings.changed` and `notification` come with UI-011.
6. **Session lifetime (Q8–Q10, ADR 0016 decision 4).**
   - Identity pushes the end of sessions through its `RevocationPublisher`. The composition root fans it out to the nodes and to the broker, which ends the matching sockets at once with `session.revoked`, then 4401.
   - A logout now publishes `Revocation{Sessions: [ref]}` too, which also closes the media connections of that session on the nodes.
   - Idle and absolute expiry are found by re-reading the session at its known expiry, and at least every 5 minutes. The re-read uses `Auth.Peek` (through `identity/http.Module.CheckSession`), which records no activity. It also catches a user disabled from the CLI of another process.
7. **Background requests (Q11).** The dispatcher marks the requests that hub events trigger with `X-Msdr-Background: 1`. The identity session middleware then resolves the session with `Peek`, so a live page does not keep its session alive against the idle timeout.
8. **Queue (Q14).** wsconn's 1 MiB byte queue stays (close 4413). `presence.count` is debounced and replaces itself; the §6.8 coalescing queue comes with map and presence deltas (M1).
9. **Re-authorisation (ADR 0016 decision 5).** A `listen_policy` change (settings `Store.Subscribe`) or a device report (a node's devices and their listen policy overrides) asks every socket to re-authorise its topics. Dropped topics are named in `error {code: forbidden, re: null, details: {topics}}`.

### Producers

10. **Change hooks.** The grid services report committed changes; `internal/wire` adapts them to the broker:
    - `Control.OnApplied`: each committed batch of node events, with its distinct types, after the link callbacks;
    - `Nodes.OnChange`: admin changes;
    - `Enrollment.Enrolled`: enrollments;
    - `Devices.OnForget`: forgotten devices;
    - `Presence.OnChange`: rows opened, closed or reaped by the presence service;
    - `Status.Listen`: status transitions. This listener is registered after the device registry's, so devices are already marked offline when the event goes out.
11. **Events (Q2).** Payloads are thin (ADR 0016 decision 2):
    - `node.status {node_id, status}` on the `nodes` topic. It fires on status transitions, admin changes, enrollments and capability reports, and on heartbeats at most once per node every 30 s. `status` is `Node.Health()`: `enrolling`, `revoked`, `removed` or the runtime status.
    - `device.status {device_id, node_id, state}` on `devices`, after device reports, node status changes and forgets (`forgotten`).
    - `presence.count {total}` on `presence`, when the listener count changes, at most once per second.

### Presence registry (GRID-017)

12. **Events rows (Q12).**
    - An `/api/ws` socket opens a `connections` row of kind `events` before its first application frame (§7.3 rule 1). The row records the user, session, role rank, client address and user agent.
    - Liveness belongs to the hub: every 15 s the events module refreshes the rows of its live sockets in one batched `UPDATE`. Sockets that stop answering pings are closed.
    - `presence.heartbeat {view, device_id?}` is validated. It records `device_id` only when the viewer may listen to that device; `view` has no column and is not stored.
    - Close reasons:
      - `client` when the client leaves or breaks the protocol;
      - `policy` when the session is revoked or expires;
      - `hub_restart` at a graceful shutdown. A crash is covered by the existing close at start.
13. **Media rows.** Each media session on the node emits `connection.heartbeat` every 15 s. Its key coalesces successive heartbeats of one session in the event buffer, latest wins.
14. **Listeners (Q4).** Listeners are the open media connections; events sockets are pages, not listeners.
    - `GET /connections` gains a required `listeners` field next to `count`, which keeps counting every open row.
    - `presence.count` has no `by_device` yet: it needs per-recipient filtering (PRS-002).
15. **Retention (Q13).** `retention.connections` (default 30 d, at least 1 d) drives a new `connections.purge` job, run hourly and listed on Admin › Data & retention. The reaper no longer deletes rows.

### Settings (Q13)

16. `grid.heartbeat_interval_s` (default 10, 1–300) and `grid.offline_after_s` (default 60, 5–3600, more than twice the interval) are DB settings, edited on a "Node health" settings page (`/admin/grid`, in the Nodes section).
    - Both apply live to the status service.
    - The interval goes to nodes in `ctl.hello`, so a node takes a new interval when its control channel reconnects.
    - A value set in the DB or the config replaces the base timing; a default keeps it. The Go tests run with faster bases.

### Admin › Nodes (GRID-005, GRID-009, GRID-015)

17. **Rights (Q1).**
    - Operators read the list and the detail; `listNodes`, `getNode` and `getNodeCapabilities` become `x-meshsdr-access: operator`. Recorded as a divergence from §5.10.
    - Admins add, edit, disable or enable, re-enroll, revoke (Q15) and remove nodes, and probe their capabilities.
18. **Pages.**
    - **`/admin/nodes`** lists every node. Each row shows the state (with its hint and the disabled flag), last seen, version, certificate expiry, devices and listeners.
    - **`/admin/nodes/{id}`** has five parts:
      - health;
      - certificate: serial, fingerprint, expiry and a pending renewal. A warning appears once two thirds of a 90-day life are over without renewal;
      - load graph;
      - devices;
      - capabilities.
    - **`/admin/nodes/new`** adds a node.
    - **Live fragments.** The list table and the status part of the detail are live fragments (`data-msdr-topics`, `hx-trigger="msdr:… from:body delay:1s, msdr:resync from:body"`, `outerMorph`). The admin forms stay outside them, so a refresh never touches a form being filled in.
19. **Enrollment token.** Adding a node and issuing a new token render a page that shows the token once (`Cache-Control: no-store`, never in a URL). The page shows the CA fingerprint, the expiry and the `meshsdr node enroll --token-file … --ca-fingerprint …` command.
20. **Destructive actions.** Re-enroll, revoke and remove sit in a `<details>` with a required confirmation checkbox, as in Admin › Users, so no script is needed.
    - Remove is offered for admin-added nodes only.
    - Config-declared nodes can be disabled or revoked but not removed.
21. **API twins.** Each form maps to its operation in `htmlActions`:

    | Form | Operation |
    |---|---|
    | `POST /admin/nodes` | `createNode` |
    | `POST /admin/nodes/{id}`, `…/disable`, `…/enable` | `updateNode` |
    | `…/token` | `issueNodeEnrollmentToken` |
    | `…/revoke` | `revokeNode` |
    | `…/delete` | `deleteNode` |
    | `…/probe` | `probeNodeCapabilities` |
    | `POST /admin/grid` | `patchSettings` |

22. **Load graph (Q3).**
    - **Rendering.** It is inline SVG rendered by templ from the RAM ring of ADR 0008 Q21, the last hour since hub start. No library and no script are involved. SVG presentation attributes are not style attributes, so the nonce-only CSP allows them.
    - **Charts.** One chart shows CPU and memory in use, in percent. A second chart shows temperature, when the node reports one.
    - **Drawing.** Lines break where heartbeats are missing. Colours are theme tokens applied through classes.
    - **Accessibility.** Each chart is a `<figure>` whose `figcaption` gives the current, lowest and highest values, and the `svg` has `role="img"` labelled by that caption. A data table, one row every 5 minutes, carries the same figures.

### Admin › Connections (Q5)

23. `/admin/connections` (admin) is a minimal read-only page.
    - **Content.** It lists the open rows of every node: user or "anonymous", client (listener, page, map viewer), address, node and device, opened and last seen.
    - **Live update.** It refreshes on `presence.count`.
    - **Left to M1.** PRS-001 (IP masking and reveal, preset and band) and GRID-022 (`admin.connections` deltas) stay in M1. Because the page refreshes on the listener count only, a new or closed page socket appears at the next refresh.

### AUTH-004 (Q16)

24. Every message the node handles is checked against the token scope:
    - `device.attach` against `listen`;
    - `demod.create` against `demod` (new);
    - `preset.select` against `preset`;
    - `device.retune` against `retune`.

    More than ten `forbidden` answers in a minute close the connection with 4403 (§5.9). Messages not implemented yet are answered `unsupported_type`; their handlers must check the scope when the device epics add them. Together with `/api/ws` topic access and the REST and HTML rights matrix (ADR 0013), this closes AUTH-004.

### Client

25. **Dispatcher.** `static/js/events.js` is ADR 0016's dispatcher, imported by `shell.js`. Compared with the prototype:
    - it fires `msdr:resync` after every acknowledged `sub` (decision 9), which also covers reconnections;
    - it does not ask again on the same connection for a topic the hub refused;
    - it marks background requests (decision 7 above);
    - it shows the shell's "Your session has ended" banner (`#msdr-session-ended`, `role="alert"`, with a reload link) on `msdr:session.revoked` and `msdr:session.expired` (Q18).

### Tests

26. Tests cover these layers:
    - **Unit:** the topic, broker, admission, listen policies and load graph.
    - **`/api/ws` handler:**
      - refusals: 426, 403 and the caps;
      - the 4408 hello timeout;
      - all-or-nothing `sub` and `invalid_payload`;
      - presence rows and their close reasons;
      - revocation and expiry (4401);
      - rate limiting, the 4403 strikes and 1001 at shutdown.
    - **Hub end to end:**
      - through the gateway with a real node: topic access and its change once a device is anonymous-listenable, `node.status`, `device.status`, `presence.count` from a gateway media connection, events rows, and a logout closing the socket;
      - media rows surviving the reaper thanks to `connection.heartbeat`;
      - Admin › Nodes driven through its forms, from adding a node, through enrolling it with the token shown, to removing it.
    - **Accessibility job.** The job creates a hub CA (`ca-init` service) and declares one node in a config drop-in (`.infra/a11y/hub.d/grid.toml`). It checks `/admin/nodes`, `/admin/nodes/new`, `/admin/nodes/attic`, `/admin/connections` and `/admin/grid`. It also checks that a live page opens the events socket, subscribes and refetches its fragment as a background request under the strict CSP.

### Tickets (Q17)

27. The PR closes #25, #31, #13, #17 and #9: every M0 component the hub starts now runs, `/api/ws` included.

## Implementation notes

- **Migrations.** No migration was needed: `connections` already had every column.
- **`/admin/nodes/new`.** The add form lives at this path, so a node whose id is `new` has no HTML detail page; its API stays available.
- **Audit actor.** Node actions of the HTML pages use the `user` audit actor, like the REST calls (ADR 0008 Q3).
- **Spike.** The spike's `wsconn.AcceptOrigins` was not kept. The events handler checks the `Origin` itself (problem+json answers) and upgrades with `wsconn.AcceptOriginChecked`.

### Security review fixes

The review found no authentication bypass, admin leak or CSRF issue. It found denial-of-service and correctness issues, fixed as follows:

1. **Bounded `sub`.** Duplicate topics are dropped, and a request that would exceed 32 topics is refused before any authorisation.
2. **One policy snapshot.** Topic checks read a cached snapshot of the effective listen policies (`policyCache`) instead of reading the device registry for each topic. The snapshot is reloaded on a `listen_policy` change, a device report or a forgotten device. Sockets re-authorise their topics only when the snapshot actually changed.
3. **Presence writes.** `presence.heartbeat` writes its device only when it differs from the last one recorded, and at most every 10 s per connection.
4. **Caps.**
   - IPv6 clients count per /64 for the per-address cap and for the upgrade rate (`app.AddressKey`).
   - An anonymous socket holding no topic is closed (1000) after a 30 s grace period, during which it counts against the caps.
5. **Idle timeout.**
   - The `/api/ws` upgrade (`Upgrade: websocket`) is a background request: the identity middleware peeks at the session.
   - The dispatcher stops after five connections in a row without a `session.welcome`. The shell then shows a "live updates stopped, reload" notice (`#msdr-events-stopped`).
6. **Audiences.**
   - `device.status` reaches operators, admins and the viewers who may listen to the device.
   - `node.status` reaches operators, admins and the viewers who may listen to one of the node's devices.
   - The registry states `enrolling`, `revoked`, `removed` and `forgotten` reach operators and admins only. `Viewer.Staff` marks operators and admins.
7. **Heartbeat interval per link.** Each node is evaluated against the heartbeat interval sent in the `ctl.hello` of its current channel (`LinkState.HeartbeatInterval`) until it reconnects. Lowering `grid.heartbeat_interval_s` therefore no longer marks connected nodes degraded.
8. **Tickers.** The status sweeper and the presence reaper reset their tickers as soon as the timings change.
9. **Strikes.** A connection closes on the eleventh violation in a minute (more than 10), consistent with the node (§5.9).
10. **Session checks.** A session whose known expiry is close or already past is re-read at most every 5 s.

## Spec divergences

Recorded here; the spec is not edited.

1. **Operator rights.** Operators read Admin › Nodes (GRID-009 👁), while §5.10 gives them ❌ on nodes.
2. **Listener counts.** §7.3 counts listeners from open rows without saying which kinds. Only media connections are counted, and `presence.count` has no `by_device` yet.
3. **`presence.heartbeat`.** It does not drive liveness: the hub owns the liveness of its sockets. Background tabs throttle timers, so a client heartbeat would reap live tabs.
4. **Upgrade refusal.** §5.9 refuses anonymous upgrades when no device is anonymous-listenable; the hub refuses the topics instead (decision 4).
5. **Logout.** A logout closing media connections is not in §5.8 or §7.3 rule 6, which name revocations, disabled users and policy changes.
