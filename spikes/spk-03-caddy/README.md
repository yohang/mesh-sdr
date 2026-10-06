# SPK-03: embedded Caddy gateway (throwaway prototype)

Spike for issue #3. This code is not production code. It lives in its own Go module, so the main `go.mod` does not change and `make lint` / `make test` ignore it. The decision record is [ADR 0002](../../docs/adr/0002-gateway-caddy.md).

## What it is

One process runs everything, over real TCP and TLS on loopback:

- **Gateway**: Caddy v2.11.7 as a Go library (`caddy.Load`), with the gateway's module set (`gateway/imports.go`) and three custom modules (`gateway/modules.go`):
  - `http.handlers.meshsdr_hub` serves the hub's own `http.Handler` (the chi router in the real app) in-process, with no extra hop.
  - `http.reverse_proxy.upstreams.meshsdr_nodes` is a dynamic upstream source that reads the hub's node registry on each request.
  - `caddy.logging.writers.slog` bridges zap to slog.
- **Fake hub** (`fakehub/`): the node registry plus `GET /internal/gateway/authz?node=<id>`, served on a unix socket. It returns 404 for unknown or disabled nodes, 503 for offline nodes and 401 when there is no session. Otherwise it returns 204 with `X-Rx-Access-Token` (an EdDSA JWT, `aud=rx-node:<id>`) and `X-Rx-Cid`.
- **Fake nodes** `roof`, `shack`, `attic` (`fakenode/`): TLS 1.3 WebSocket echo servers that accept mTLS only. They accept only a client cert with an `urn:rx:gateway:*` URI SAN, verify the token offline with the hub public key, and send a `hello` JSON that echoes what they received.
- **PKI** (`pki/`): a stand-in for the hub internal CA. It issues the gateway client cert, the node certs (URI SAN `urn:rx:node:<id>`, DNS `<id>.nodes.rx.internal`) and a self-signed "operator" cert.

The node route follows TECHNICAL_SPEC §4.6. It strips `X-Rx-*`, runs forward auth (`reverse_proxy` + `rewrite` + `handle_response`), rewrites the path to `/ws`, then proxies with `flush_interval: -1`, `stream_close_delay`, `stream_timeout` and the TLS transport (hub CA plus gateway client cert).

The spike tests three drivers:

| Driver | How the hub applies a node add/remove | Option in the ADR |
|---|---|---|
| `embedded-load` | Rebuilds the full JSON and calls `caddy.Load` (one route per node, `@id: node-<id>`) | A |
| `admin-api` | `POST /id/node-routes/routes` / `DELETE /id/node-<id>` on Caddy's admin API over a unix socket. The code path is the same as a sidecar's, so this measures the sidecar option. | B |
| `dynamic` | Changes only the hub registry. There is one static route `/nodes/{id}/ws` whose upstream (and TLS `server_name`) resolves per request, so Caddy is never reloaded. | C |

## Run

From the repo root (Go runs only in Docker):

```sh
docker compose run --rm --no-deps app bash spikes/spk-03-caddy/run.sh    # scenarios, ~60 s
docker compose run --rm --no-deps app bash spikes/spk-03-caddy/sizes.sh  # binary sizes
```

`run.sh` also writes the reference configs `examples/caddy-per-node-files.json` (option A/B, operator cert) and `examples/caddy-dynamic-acme.json` (option C, ACME), using production-like paths.

## Measurements

Measured on 2026-10-06 with Caddy v2.11.7, Go 1.26.8 and 12 vCPU, all on loopback. Absolute latencies are lower than over a real network; the deltas are what matter.

### Functional checks

All 14 checks pass for all three drivers:

- WS upgrade and echo through the gateway with mTLS to the node.
- The node sees the gateway URI SAN.
- The hub-minted token is injected and verified offline.
- A client-forged `X-Rx-Access-Token` or `X-Rx-Node-Id` is stripped.
- With no session, the authz 401 is passed through and there is no upgrade.
- A node that is not enrolled or unknown gets 404.
- A node added at runtime is reachable, and a node removed at runtime gets 404.
- An offline node keeps its route and authz answers 503.
- The hub API is served in-process.
- Any other path gets 404.

| | embedded-load | admin-api | dynamic |
|---|---|---|---|
| Add node: apply call | 3.5 ms | 3.0 ms | < 1 µs |
| Remove node: apply call | 4.5 ms | 4.5 ms | < 1 µs |
| New node reachable after apply returns | +7.8 ms (1st WS handshake) | +7.5 ms | +7.8 ms |

### Open WebSockets across a route change (question 3)

Two WebSockets (`roof`, `shack`) echo every 10 ms. Event 1 adds an unrelated node `attic`. Event 2 removes `shack`.

| Driver | `stream_close_delay` | roof, add attic | shack, add attic | roof, remove shack | shack, remove shack |
|---|---|---|---|---|---|
| embedded-load | 0 | closed ≤ 20 ms | closed ≤ 20 ms | closed ≤ 10 ms | closed ≤ 10 ms |
| embedded-load | 3 s | closed at 3.01 s | closed at 3.01 s | closed at 3.02 s | closed at 3.02 s |
| admin-api | 0 | closed ≤ 10 ms | closed ≤ 10 ms | closed ≤ 10 ms | closed ≤ 10 ms |
| admin-api | 3 s | closed at 3.02 s | closed at 3.02 s | closed at 3.01 s | closed at 3.01 s |
| dynamic | n/a | survived | survived | survived | survived |

What the table shows:

- Any config change, even an unrelated one, closes every proxied WebSocket on every node route. Admin-API partial updates behave the same, because Caddy re-provisions the whole config on every change.
- `stream_close_delay` only postpones the close: the streams are force-closed exactly when the delay runs out. With the spec default of `2h`, every listener connected at the time of a node enrollment is cut 2 h later.
- In `dynamic` mode nothing reloads, so nothing is cut. This also means that removing a node does not close its open sockets. That must come from the node (`ctl.revocations`) or from a small connection-tracking middleware, which is not prototyped.

### Reload storm (new connections during reloads)

The test makes 40 route changes, one every 25 ms, while 2 clients loop on new-connection `GET /api/v1/ping` and new WS dials. The control row has the same traffic with no change.

| Driver | apply p50 | apply max | HTTP ok/total | WS dials ok/total | errors |
|---|---|---|---|---|---|
| control | – | – | 399/399 | 146/146 | none |
| embedded-load | 3.1 ms | 12.3 ms | 459/495 | 175/190 | 49× `EOF`, 2× `connection reset` |
| admin-api | 3.7 ms | 11.7 ms | 482/515 | 187/197 | 43× `EOF` |

Each reload makes about 1.1 new requests fail, both plain HTTP to the hub UI/API and WS upgrades. The likely cause is connections caught by the outgoing server instance during the listener hand-over. The result was the same in 3 runs.

### Latency (N=200 connects, N=1000 echoes)

| Path | connect p50 | connect p95 | echo RTT p50 | echo RTT p95 |
|---|---|---|---|---|
| browser → gateway (per-node route) → node | 6.6 ms | 10.4 ms | 48 µs | 65 µs |
| browser → gateway (dynamic route) → node | 5.4 ms | 10.0 ms | 46 µs | 52 µs |
| direct mTLS → node (no gateway, no authz) | 2.3 ms | 2.5 ms | 8 µs | 27 µs |

The gateway adds about 3–4 ms per WS connect: the TLS termination, the forward-auth round trip over the unix socket, and a second TLS handshake to the node. It adds 10–50 µs per message, which is noisy from run to run (another run measured 18 µs and 57 µs echo p50). Dynamic upstream resolution costs nothing measurable.

### TLS (question 5)

| Mode | Config | Result |
|---|---|---|
| Operator cert/key | `tls.certificates.load_files` + `tls_connection_policies: [{}]` + `automatic_https.disable` | PASS (used by all scenarios) |
| Managed, internal issuer | `tls.certificates.automate: [host]` + automation policy `issuers: [{module: internal}]` + `pki.certificate_authorities.local.install_trust: false` | PASS, first TLS WS 51 ms after `Load` |
| ACME | same as the internal issuer, with `issuers: [{module: acme, email, ca}]` | `caddy.Validate` OK, not issued (no public DNS in the spike) |

### Binary size and module set (question 6)

Builds use `CGO_ENABLED=0 -trimpath -ldflags='-s -w'`:

| Build | Size | Packages | Modules |
|---|---|---|---|
| baseline (net/http + httputil reverse proxy + slog) | 6.6 MiB | 191 | 1 |
| gateway module set (`gateway/imports.go` + custom modules) | 46.2 MiB | 939 | 142 |
| same without `caddypki` (still pulled in by `caddytls`) | 46.2 MiB | 937 | – |
| `modules/standard` (≈ the `caddy` sidecar binary) | 55.0 MiB | 1016 | 161 |
| `meshsdr` today (main module, no Caddy) | 11.8 MiB | – | – |

Embedding adds about +40 MiB to `meshsdr` (estimate: about 50 MiB in total). Trimming the module set saves only about 9 MiB compared with `standard`. The core `caddyhttp` and `caddytls` packages already pull in cel-go, OpenTelemetry OTLP exporters, gRPC, smallstep/certificates and quic-go, so no import choice gets below about 46 MiB. The idle heap after `caddy.Load` is about 1 MiB, with 24 more goroutines.

The minimal module set:

- `caddyhttp` (app, matchers, `subroute`, `vars`, `static_response`)
- `caddyhttp/headers`
- `caddyhttp/reverseproxy`
- `caddyhttp/rewrite`
- `caddytls`
- `caddypki` (only for the internal issuer, linked anyway)
- `filestorage`
- `logging` (json encoder)

### Logging (question 6)

The `caddy.logging.writers.slog` module plus `encoder: json` re-emit every Caddy entry on the injected `*slog.Logger`. Each entry keeps Caddy's timestamp, the mapped level and `component=gateway.caddy.<zap logger name>`, for example `gateway.caddy.http.handlers.reverse_proxy` or `gateway.caddy.tls.handshake`. Without `logging.sink`, stdlib `log.Printf` output (for example module cleanup errors) bypasses the bridge. With the sink pointed at the same writer, only 1 line in the whole run bypasses slog: `redirected default logger`, which Caddy prints on the first `Load`, before the config exists. At DEBUG, Caddy is very chatty: about 1 `tls.handshake` record and 1 `reverse_proxy` record per request. The app should map `log.level` to Caddy's level, with `info` as the floor for Caddy unless debugging.

## Gotchas found

- `@id` keys are accepted by `caddy.Load` and the admin API, which index and then strip them. `caddy.Validate` rejects them (`unknown field "@id"`), so strip them before validating.
- `root_ca_pem_files` in the reverse_proxy TLS transport is deprecated (it logs a WARN). Use `"ca": {"provider": "file", "pem_files": [...]}`.
- `server_name` in the TLS transport **does** accept placeholders at request time, although the docs comment says otherwise. This is what makes option C possible with per-node TLS server names.
- Node ids must match `^[a-z0-9][a-z0-9-]{1,62}$` (at least 2 characters). The dynamic route validates the id with `path_regexp` before using it in a server name.
- Caddy's package `init` redirects the stdlib `log` package to zap (`zap.RedirectStdLog`). `slog.Default()` writes through `log`, so it would end up in Caddy's logger. The app must keep using its own injected logger.
- Caddy is a process-wide singleton: one config, `caddy.Load` / `caddy.Stop`, global module registry, global listener pool. Two gateways cannot run in one test binary in parallel, and custom modules cannot receive constructor dependencies. `gateway.Bind` (a package-level map) is the only bridge, and it breaks the AGENTS.md "no globals" rule, so it must stay inside the gateway adapter.
- When enabled, the admin API exposes `POST /stop`, which calls `os.Exit` on the **hub** process. In embedded mode, keep the admin API disabled (`admin.disabled: true`).
- Set `admin.config.persist: false` and an explicit `storage` root (for example `/data/caddy`). Otherwise Caddy writes to `$XDG_CONFIG_HOME` and `$XDG_DATA_HOME`. On distroless nonroot that is `/home/nonroot`, which is outside the volume.
- Set `protocols: ["h1","h2"]` to avoid the default HTTP/3 UDP listener. Set `automatic_https.disable_redirects` unless port 80 should be bound.
