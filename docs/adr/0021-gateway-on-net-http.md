# ADR 0021: Gateway on net/http, without Caddy

- Status: Accepted
- Date: 2026-10-07
- Deciders: project owner
- Amends: ADR 0002 (Caddy embedded as a library) and ADR 0012 (Caddy settings, node-only artifact, CI). Also drops the `prod-node` image of ADR 0019 decision 14.

## Context

ADR 0002 option C embedded Caddy with one static `/nodes/{id}/ws` route, an in-process forward auth and a custom node transport (ADR 0012). After ADR 0012 the gateway uses little of Caddy: the hub router, the authz, the upstream and the transport are our own code; Caddy adds TLS automation, about 40 MiB, a package-level binding table (an exception to "no globals"), an `init` module registry, a zap→slog bridge, a second build (`nogateway`) with its own lint and test passes, and a second image. ADR 0002 listed "no Caddy" (`net/http` + `httputil.ReverseProxy` + `autocert`) as considered but not evaluated.

## Decision

The owner drops Caddy. `internal/grid/infra/gateway` is a `net/http` server with the same behaviour and security properties:

1. **One `http.Server`** on `gateway.https_listen` (HTTP/1.1 and HTTP/2 with TLS) or on `gateway.http_listen` with `tls_mode = off` (HTTP/1.1). It serves the node route, 404 on any other `/nodes/*` and `/internal/*` path (case and dot segments included), and the hub router for everything else. With TLS, `http_listen` redirects to `hub.url` with 308 (and answers ACME HTTP-01 challenges). Stop: graceful shutdown for 10 s, then every remaining connection, proxied WebSockets included, is closed.
2. **TLS modes** (same `[gateway]` keys):
   - `acme`: `golang.org/x/crypto/acme/autocert` for the host of `hub.url`, cache in `gateway.storage_dir` (default now `/var/lib/meshsdr/acme`), TLS-ALPN-01 on `https_listen`, HTTP-01 when `http_listen` is set, `acme_email` and `acme_ca` as before;
   - `files`: `tls_cert`/`tls_key` read at start (key 0600, checked at load);
   - `internal`: a certificate for the host of `hub.url` minted in memory by the **hub CA** (`tls.ca_cert`, required; renewed at 2/3 of its 30-day life), instead of a separate local CA in `storage_dir`;
   - `off`: unchanged.
3. **Node route**: client `X-Rx-*` headers are deleted, the in-process authz sub-request (`AuthzPath`, `grid/http.AuthzHandler` → `grid/app.MediaAccess`) is unchanged and its non-2xx answers pass through. On success `httputil.ReverseProxy` dials the node with `NodeTLS` (`pki.HubDialConfig`: TLS 1.3, `<id>.nodes.rx.internal`, in-memory gateway certificate, node URI SAN, pinned fingerprint, revocation list), one connection per request, no redirects, no buffering. The request header allow-list (handshake, `Origin`, `User-Agent`, plus token, cid and node id), the 101 response allow-list, the `node_refused` problem replacing any other node answer (`nosniff`, `sandbox` CSP, `no-store`), informational node answers dropped, `gateway.max_body`, and no `Server` header are kept. An unreachable node answers 502. `gateway.stream_timeout` bounds a proxied WebSocket.
4. **Logging**: the gateway logs through the injected logger (`grid.infra.gateway`); nothing logs headers, so tokens and credentials are never logged (no redaction layer needed). Server errors (TLS handshakes) are Debug.
5. **Removed**: `gateway.mode` (no sidecar to choose), `gateway.stream_close_delay` (no reloads); a config still setting them fails to load (`unknown_key`). The binding table, the module registry, the slog bridge, the `nogateway` build tag, its lint/test passes and Caddy-link checks, and the `prod-node` image.
6. **One image**: `prod` runs every role (`CMD ["all"]` by default, `node` for a node-only container). CI publishes it (and `-sources`) on pushes to `main` and tags only.

## Consequences

- One binary and one image; 145 modules leave `go.sum`. No third-party dependency is added (`golang.org/x/crypto` was already used).
- `tls_mode = internal` browsers must trust the hub CA (`tls/ca.pem`) rather than a gateway-specific CA.
- WebSocket over HTTP/2 (extended CONNECT) is off in Go by default: browsers open media and events WebSockets over HTTP/1.1.
- No more upgrade contract test against Caddy; the gateway tests cover headers both ways, authz pass-through, the transport checks and the TLS modes.
