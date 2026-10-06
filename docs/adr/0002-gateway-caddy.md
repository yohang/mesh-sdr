# ADR 0002: Gateway, Caddy embedded vs sidecar, and how per-node routes are applied

- **Status:** Proposed
- **Date:** 2026-10-06
- **Deciders:** project owner
- **Spike:** SPK-03 (#3). Prototype and raw measurements: [`spikes/spk-03-caddy/`](../../spikes/spk-03-caddy/README.md)
- **Unblocks:** GRID-011 (#19), GRID-012 (#20). Related: INT-001 (#85)

## Context

The hub fronts all browser traffic through a gateway (TECHNICAL_SPEC §3.2, §4.6):

- TLS termination, using ACME or operator-provided certificates (INT-001).
- Static routes to the hub (`/`, `/assets/*`, `/api/v1/*`, `/api/ws`).
- One route `/nodes/{nodeId}/ws` per enrolled node. Each route is added and removed at runtime with no restart (GRID-011), and an unknown or disabled id gets 404.
- Forward auth to the hub. The hub mints an EdDSA access token, and the gateway injects it as `X-Rx-Access-Token` after stripping any client-supplied `X-Rx-*` header (§4.3, §5.8, §5.16).
- WebSocket proxying to the node over mTLS, with the gateway client certificate (URI SAN `urn:rx:gateway:<hub>`).

The spec names Caddy as the reference gateway (P8). It allows two modes, `gateway.mode = embedded | sidecar`. It accepts that a reload can drop proxied WebSockets, mitigated by `stream_close_delay` (default `2h`), stable routes, and route changes only on enrollment or removal (§4.6 rules 2–3, Risks).

Decisions already made, not revisited here:

- One `meshsdr` binary with roles hub, node and all.
- TOML config plus `MESHSDR_` env overrides.
- EdDSA JWT access tokens (golang-jwt/jwt/v5) that nodes verify offline.
- Hub↔node mTLS with the hub's internal CA.
- Docker distroless deployment in M0.
- The stack plan says "Caddy embedded as a Go library".

This ADR settles **how** the gateway is built and driven. It does not change the product specification.

### What the spike measured

The spike used Caddy v2.11.7 and Go 1.26.8 on loopback, with a hub plus 2–3 fake mTLS nodes. Full tables are in the spike README.

| Question | Finding |
|---|---|
| 1. Runtime routes, 404 for unknown ids | Works in every variant. An apply call through `caddy.Load` or the admin API takes about 3–5 ms (max 12 ms). The new node is reachable on the next request. A registry-only change (dynamic upstream) takes under 1 µs. |
| 2. WS + mTLS to the node | Works. The node sees the gateway URI SAN. Connect costs about 3–4 ms more than direct, and the echo RTT 10–50 µs more (noisy on loopback). |
| 3. Open WS during a route change | **Every** config change closes **every** proxied WS on every node, including changes to an unrelated node and admin-API partial updates. With `stream_close_delay = 3s`, all streams were force-closed at 3.01 s. The delay postpones the cut but does not prevent it. With a dynamic upstream module, nothing is reloaded and all WS survive. |
| 3b. New traffic during reloads | About 1.1 new requests (plain HTTP to the hub, or WS upgrades) failed with `EOF` per reload, under continuous load. The control run without reloads had 0 failures. |
| 4. Forward auth + token injection | Works as the JSON form in §4.6: `reverse_proxy` with `rewrite` and `handle_response`, to a hub unix socket. A client-forged `X-Rx-Access-Token` is stripped. Non-2xx statuses (401, 404, 503) are passed through and the upgrade is never attempted. |
| 5. TLS | Operator cert/key (`load_files`) works. The managed cert (`automate` + issuer) works with the internal issuer, with the first TLS connection 51 ms after `Load`. The ACME issuer config validates. No public ACME run was possible in the spike. |
| 6. Size, modules, logging | The gateway module set adds about **+40 MiB** to the binary: 46.2 MiB vs a 6.6 MiB baseline, and `meshsdr` is 11.8 MiB today. The Caddy core pulls in cel-go, OTLP/gRPC, smallstep and quic-go whatever modules are chosen, and the full `standard` set is 55 MiB. zap→slog works through a custom `caddy.logging.writers.slog` module plus `logging.sink`. Only 1 line bypasses it, on the first `Load`. |

Other constraints found:

- Caddy is a process singleton (`caddy.Load` / `caddy.Stop`).
- Custom modules are built from JSON, so they can receive dependencies only through a package-level binding table. This is an exception to the AGENTS.md "no globals" rule.
- The package `init` redirects the stdlib `log` package to zap.
- The admin API has `POST /stop`, which calls `os.Exit`.
- Caddy needs a `storage` root for certificates and ACME state, outside SQLite.

## Options

The three options share the same route shape (strip, forward auth, rewrite, mTLS proxy). They differ in how node lifecycle changes reach Caddy.

### Option A: Embedded library, one route per node, full `caddy.Load` on each change

This is the literal reading of §4.6 `embedded`. The hub keeps the node list, rebuilds the whole JSON config, and calls `caddy.Load(cfg, false)` on every enrollment, removal or address change. Changes are batched to at most 1 per 5 s.

Pros:

- It follows the spec text: one route per node, `@id: node-<id>`.
- The JSON is identical to the sidecar's, so B stays possible later with the same config builder.
- One process and one image.
- The hub UI and API can be served in-process through a small handler module, with no hop.

Cons:

- Every enrollment or removal cuts **all** listeners on **all** nodes, after `stream_close_delay`. With `2h`, every listener connected at enrollment time is dropped 2 h later.
- Each reload makes about 1 new request fail.
- +40 MiB binary.
- Caddy's process-global state lives inside the hub process.
- A small global binding table is needed for the custom modules (hub handler, slog writer).

Measurements: apply p50 3.1 ms, max 12.3 ms. WS closed ≤ 20 ms after a change with delay 0, and at delay + 10 ms otherwise. 459/495 HTTP and 175/190 WS dials OK during a 40-change storm.

### Option B: Sidecar Caddy process driven by the admin API

This is §4.6 `sidecar`. Caddy (the official `caddy` binary or image) runs next to the hub. The hub sends `POST /load` at start, then `POST /id/node-routes/routes` and `DELETE /id/node-<id>` over a 0600 unix socket. It detects drift (`GET /config/` hash) and re-pushes after a sidecar restart.

Pros:

- `meshsdr` stays small (11.8 MiB). Caddy is upgraded independently and is a stock, well-known artifact.
- No Caddy globals in the hub process. A gateway crash does not take down the hub, and the reverse is also true.
- Stack-agnostic, as in P8.

Cons:

- Same reload semantics as A. Admin-API partial updates still re-provision the full config, so all WS are cut and about 1 request fails per change. This was measured: identical to A.
- Two processes and two images to supervise, version-pin and keep in sync. This is more work for the all-in-one hobby install.
- The hub cannot be served in-process: Caddy must reach the hub over a socket for every request.
- Drift detection and re-push logic are needed (§4.9).
- Total deployed size is larger: about 55 MiB for Caddy plus 11.8 MiB for `meshsdr`.
- The custom slog bridge is not available, so Caddy logs to stderr in its own JSON format: one stream, two formats.

Measurements: apply p50 3.7 ms, max 11.7 ms. WS survival is the same as A. 482/515 HTTP and 187/197 WS dials OK during the storm. The prototype measured the admin-API code path in-process, which is the same code a sidecar runs. Process supervision was not measured.

### Option C: Embedded library, one static node route plus a dynamic upstream module

§4.6 `embedded` explicitly allows "a custom upstream-source module that reads the node registry directly". There is a single route:

- It matches `path_regexp ^/nodes/[a-z0-9][a-z0-9-]{1,62}/ws$`.
- `vars rx_node={http.request.uri.path.1}`, then the same strip, forward auth and rewrite chain.
- `reverse_proxy` with `dynamic_upstreams: {source: meshsdr_nodes}`, which reads the hub registry per request.
- The TLS transport uses `server_name: "{http.vars.rx_node}.nodes.rx.internal"`. Placeholders are resolved per connection, as verified in the v2.11.7 source and the prototype.

Authz returns 404 for unknown or disabled ids and 503 for offline ones. Node enrollment, removal and address change never touch the Caddy config. Caddy is loaded once at hub start, and again only for hub-level changes such as TLS settings.

Pros:

- No WS cut and no failed request on node lifecycle events. The "accepted risk" of §4.6 rule 3 and the Risks table goes away for node changes.
- `stream_close_delay` and batching become largely irrelevant.
- Removal latency is below 1 µs, and there is no config churn or drift.
- The route list is constant, and node state lives only in the hub registry and DB, which fits P4.
- One process, and the hub is served in-process.
- About 30 lines of module code. Latency is the same as A within run-to-run noise: connect p50 5.4–5.6 ms vs 5.7–6.6 ms.

Cons:

- It diverges from the **letter** of §4.6: rule 1 ("one route per enrolled node"), rule 4 (`@id node-<id>` per route) and the §4.10 step "POST /id/node-routes/routes". GRID-011 behaviour is fully met. A spec clarification issue would be needed (separate issue, per #3 notes).
- Removing a node does **not** close its open WS at the gateway. Closing must come from the node (`ctl.revocations` / cert serial), from token expiry (≤ 300 s + 30 s grace), or from a small connection-tracking middleware (not prototyped). In A and B with `2h`, removal does not close them promptly either.
- It depends on the Caddy `UpstreamSource` interface and on placeholder support in `server_name`. Both are Go API surface that must be pinned and covered by a contract test on upgrade.
- Same +40 MiB and the same global binding table as A.
- A sidecar later would need the per-node JSON from A, or a custom Caddy build containing the module.

### Considered, not evaluated: no Caddy

A gateway built from `net/http` + `httputil.ReverseProxy` + `certmagic` or `autocert` would avoid about 40 MiB and all Caddy globals: the baseline build is 6.6 MiB. It is outside the stack plan and P8's reference gateway, so the spike did not evaluate it. It is listed so the owner can weigh the size cost of A and C.

## Recommendation

**Option C** (embedded, static node route with a hub-registry upstream module), built so that the per-node JSON builder from A remains the documented sidecar path (`gateway.mode = sidecar`, implemented later only if needed).

Recommended details, if accepted:

1. Caddy is the only listener on the public port. The hub router is served in-process through a `meshsdr_hub` handler module. Forward auth goes to the hub's internal handler on a 0600 unix socket.
2. Embedded mode sets `admin.disabled: true` and `admin.config.persist: false`, `storage` at `/data/caddy`, `protocols: ["h1","h2"]`, and `logging.logs.default` + `logging.sink` pointing at the slog writer, with the level mapped from `log.level`.
3. Use `ca: {provider: file}` instead of the deprecated `root_ca_pem_files`. Pin `github.com/caddyserver/caddy/v2` to one minor version (v2.11.x), with a contract test of the custom modules on each bump.
4. Confine the Caddy globals (module registration, binding table) to `internal/gateway/infra`, behind a `Gateway` port that the hub app layer uses (`Start`, `Stop`, `ApplyTLS`). This keeps the rest of the code compliant with the DI rules.
5. Close sockets on node removal from the node side (`ctl.revocations` before the control channel is closed). Add a gateway-side connection tracker only if the owner requires gateway-enforced cut-off.

## Open questions for the owner

1. **Which option, A, B or C?** The spike recommends C.
2. **If C:** do you accept the deviation from §4.6 rules 1 and 4 and §4.10 (one static route instead of one route per node)? If so, a separate spec-clarification issue will be opened, since this ADR does not edit the spec.
3. **Socket cut-off on node removal or revocation:** is node-side closing (`ctl.revocations`) plus token expiry (≤ 5.5 min) enough? Or must the gateway itself cut a removed node's open WebSockets immediately? That would need a small connection-tracking module in C. In A and B it is a forced close after `stream_close_delay`, which is 2 h by default.
4. **Binary size:** is +40 MiB on `meshsdr` (about 12 → about 50 MiB, and the same growth for the image) acceptable for every role, including `node`-only deployments that never use the gateway? Alternatively, should the gateway be excluded from node-only builds with a build tag? That would conflict with "one binary".
5. **Persistent TLS state:** ACME accounts and certificates (and the internal-issuer CA, if used) live in Caddy storage. Is a file store on the `/data` volume (`/data/caddy`) acceptable under P4 ("DB is the only persistent store")? Or is a SQLite-backed `certmagic.Storage` module required? Operator-provided cert/key needs no storage.
6. **Globals exception:** do you accept a package-level binding table, confined to the gateway infra adapter, as the documented exception to the AGENTS.md "no globals / constructor injection only" rule? Caddy cannot inject constructor dependencies into modules.
7. **`gateway.mode = sidecar` in M0:** implement it now, defer it (keep only the config builder), or drop it?
8. **If A or B is chosen:** keep the spec defaults `stream_close_delay = 2h` and `stream_timeout = 24h`, knowing that every listener connected at enrollment time is cut when the delay expires and that each reload fails about 1 new request in flight?
9. **Caddy log level:** should Caddy follow `log.level` one-to-one? Or should it be capped at `info` unless a dedicated key (e.g. `gateway.log_level`) is set? At debug, Caddy logs about 2 records per request.

## Consequences

If C is accepted:

- GRID-011 is implemented as follows:
  - `internal/gateway` (infra adapter, Caddy JSON builder, custom modules) behind a `Gateway` port.
  - The hub node registry is the source of truth for upstreams.
  - Authz answers 404, 503, 401, 403 or 429.
  - There are no route add/remove calls in the enrollment flow.
- GRID-012 is unaffected: nodes verify the token offline. The gateway only strips and injects.
- The Risks entry "Gateway restart or config reload" now covers only hub restarts and TLS/config changes, not node lifecycle events.
- `meshsdr` grows by about 40 MiB and gains about 140 modules in `go.sum`. Dependency updates (Renovate/Dependabot) must include a contract test of the custom modules against the pinned Caddy version.
- The deployment needs `/data/caddy` (or the storage chosen in Q5) on the data volume. On distroless nonroot, binding :443 relies on Docker's default `net.ipv4.ip_unprivileged_port_start=0`, or on mapping a high port.
- A future sidecar mode reuses the per-node JSON builder (Option A shape) and the admin-API driver from the spike. Both are already exercised.

If A or B is chosen, the reload findings (all WS cut, about 1 failed request per reload) stay as the accepted risk the spec describes. Batching (≤ 1 apply per 5 s) and the client reconnect/resume path (TECHNICAL_SPEC §2.3, Protocol v1 `resume`) become mandatory parts of M0.

## References

- TECHNICAL_SPEC §2.3 (Availability, Security baseline), §3.2, §3.5, §4.1–§4.3, §4.6, §4.9, §4.10, §5.8, §5.16. FEATURE_SPEC GRID-011, GRID-012, GRID-015, INT-001, P4, P8, config keys `gateway.*`.
- Spike code and measurements: `spikes/spk-03-caddy/README.md`. Reference configs: `spikes/spk-03-caddy/examples/`.
- Caddy docs:
  - reverse_proxy, streaming (`stream_close_delay`, `stream_timeout`): https://caddyserver.com/docs/caddyfile/directives/reverse_proxy#streaming
  - Admin API and `@id`: https://caddyserver.com/docs/api#using-id-in-json
  - JSON config structure: https://caddyserver.com/docs/json/
  - Dynamic upstreams: https://caddyserver.com/docs/json/apps/http/servers/routes/handle/reverse_proxy/dynamic_upstreams/
  - Extending Caddy (modules): https://caddyserver.com/docs/extending-caddy
  - Embedding API: https://pkg.go.dev/github.com/caddyserver/caddy/v2
- Caddy v2.11.7 source consulted:
  - `caddy.go` (`Load`, `changeConfig`, `Validate`)
  - `listen.go` (shared listener hand-over on reload)
  - `modules/caddyhttp/reverseproxy/streaming.go` (`cleanupConnections`: the delay only schedules the close)
  - `modules/caddyhttp/reverseproxy/httptransport.go` (`server_name` placeholders resolved at dial time)
  - `admin.go` (`/stop` → `os.Exit`)
  - `logging.go` (`zap.RedirectStdLog`, `sink`)
