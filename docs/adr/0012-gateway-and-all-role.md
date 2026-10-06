# ADR 0012: Gateway, node media access, node removal and the all role

- Status: Accepted
- Date: 2026-10-06
- Deciders: project owner
- Scope: epic part `epic/grid-3` of epics #446 (grid) and #453 (integrations): GRID-011 (#19), GRID-012 (#20), GRID-014 (#22), GRID-015 (#23), GRID-003 (#11), INT-001 (#85). It finishes the gateway part of GRID-007 (#15).
- Amends: ADR 0002 (implementation notes: forward auth and node transport).

## Context

ADR 0002 chose Caddy embedded in the hub, with one static `/nodes/{nodeId}/ws` route whose upstream is resolved per request from the node registry (option C). ADR 0008 built the internal CA, the enrollment and the control channel. ADR 0011 specifies the access tokens (ACC-007), which the accounts epic (`epic/acc-1`) builds in parallel. This part adds the gateway itself, the authorization of node media connections, their verification on the node, node revocation, the `all` role, and the node-only build.

## Decision

Numbers refer to the questions of the design proposal; the owner accepted every recommendation.

### Gateway (GRID-011, INT-001)

1. **Listeners (Q1).** `hub.listen` is removed. Caddy owns every public port of the hub through the `[gateway]` table. There is no direct listener for the hub router.
2. **Forward auth in-process (Q2, amends ADR 0002).** The ADR 0002 implementation notes called for a unix-socket sub-request. Instead, a `meshsdr_node_authz` handler calls the hub router in-process at `/internal/gateway/authz?node=<id>`. The request keeps its real peer address, TLS state and cookies, and passes through the identity middlewares.
   - A non-2xx answer goes to the browser as is, with its body, and no upgrade happens.
   - A 2xx answer sets `X-Rx-Access-Token` and `X-Rx-Cid` on the proxied request and gives the node address in `X-Rx-Upstream`, which never leaves the hub.
   - The route first deletes every client `X-Rx-*` header (§4.6 rule 5). The gateway answers 404 on `/internal/*` and on any other `/nodes/*` path.
3. **Node transport (Q3).** A `meshsdr_node` reverse-proxy transport dials the node with `pki.HubDialConfig`:
   - TLS 1.3 and the `<id>.nodes.rx.internal` server name;
   - the in-memory gateway client certificate `urn:rx:gateway:<hub-id>`, re-minted at 2/3 of its life without a Caddy reload;
   - the node URI SAN, the pinned fingerprint and the revocation list, the same checks as the control channel.

   The transport does not depend on placeholders in `server_name`.
4. **Upstream (Q4).** There is no cache. The authz reads the node once per upgrade and returns its address; the `meshsdr_nodes` upstream source reads it from the request variables. Enrollment, removal, revocation and address changes never reload Caddy.
5. **TLS (Q5, Q6).** These keys are added (see the configuration table below):
   - `gateway.tls_mode`:
     - `acme` (default): certificate for the host of `hub.url`; TLS-ALPN on `https_listen`, plus HTTP-01 when `http_listen` is set;
     - `files`: `gateway.tls_cert` and `gateway.tls_key`;
     - `internal`: local CA in `storage_dir`;
     - `off`: plain HTTP on `http_listen`, behind a TLS-terminating proxy or for development.
   - `gateway.https_listen` (`:443`) and `gateway.http_listen` (none). With TLS, `http_listen` redirects to `hub.url` with 308.

   Handshakes run per connection, so the accept loop never blocks. ACME is validated by the configuration and contract tests only; it has no public run in CI.
6. **Body limit (Q7).** `gateway.max_body` (1 MiB) caps `/api/v1` request bodies through a chi middleware.
7. **Caddy settings.**
   - Admin API disabled, `persist: false`, `file_system` storage at `gateway.storage_dir` (`/var/lib/meshsdr/caddy`).
   - Protocols `h1`/`h2` (only `h1` without TLS), a grace period of 10 s, and `stream_close_delay`/`stream_timeout` from the configuration.
   - Caddy is loaded once at start and stopped at shutdown.
   - Its logs go to the injected logger at `log.level` (`caddy.logging.writers.slog`, component `gateway.caddy.<logger>`).
8. **Modules and dependencies.** `github.com/caddyserver/caddy/v2` v2.11.7 with `caddyhttp`, `caddyhttp/headers`, `caddyhttp/reverseproxy`, `caddyhttp/rewrite`, `caddytls`, `caddypki`, `filestorage` and `logging` (not `modules/standard`). The custom modules, the binding table and the module registration stay in `internal/grid/infra/gateway` (ADR 0002 decision 5). One gateway runs per process. `gateway_test.go` is the contract test to run on every Caddy upgrade.

### Media access (GRID-011, GRID-012, ACC-007 coordination)

9. **Authz rules.** `grid/app.MediaAccess` decides each upgrade:
   - **404 `node_not_found`** for an unknown, pending, disabled or revoked node.
   - **503** for a node without an open channel (`node_offline`) or with an incompatible version (`node_incompatible`).
   - **403 `origin_denied`** unless the Origin is the one of `hub.url`.
   - **429 `rate_limited`** with `Retry-After`: 10 upgrades per minute per client address, and 30 tokens per minute per session (per address when anonymous).
   - **Scopes (`scp`).** `listen` and `demod` where the effective listen policy admits the subject; `preset` for operators of the device; `retune` for them when the node sets `operator_can_retune`; everything for admins.
   - **401 / 403 on scopes.** A node whose devices are all out of reach answers 401 to anonymous visitors and 403 to users.
   - **Global listen policy.** It is `settings.listen_policy` (default `anonymous`) until the settings store exists.
   - **Presence.** The `connections` row is created with the cid before the token is returned.
10. **Token contract (Q8, option b).** The neutral package `internal/protocol/rxv1/token` (`golang-jwt/jwt/v5`, already approved) holds:
    - the claims, `Sign`, and an offline `Verify` with a 30 s leeway;
    - the Ed25519 JWK/JWKS types with RFC 7638 kids;
    - `SessionRef`, the single derivation of `sid` and of the session entries of `ctl.revocations`.

    The hub ports are `KeySource`, `TokenIssuer` and `RevocationBroadcaster`. Until the identity keyring of ACC-007 is merged, an interim adapter holds one ephemeral Ed25519 key per hub process. After acc-1 merges, the wiring will:
    - adapt `idm.Keys` as the `KeySource`;
    - route identity's `RevocationPublisher` to the grid broadcaster;
    - bind the authz cid to `POST /api/v1/auth/token`;
    - clear `connections.user_id` on account deletion.
11. **Distribution.**
    - `ctl.keys.update {issuer, keys, revoked_kids}` is pushed on every control connect and on key changes.
    - `ctl.revocations` now carries `sessions` and `users`, pushed to every channel and re-pushed for 15 minutes to channels that reconnect.
    - A dropped channel flushes its queue and finishes the close handshake before its context is cancelled.
12. **Node media WebSocket (GRID-012).** `GET /ws` on the node does the following:
    - It accepts only the gateway certificate (with the hub id of `hub_trust.hub_identity` when set) and checks the Origin against the issuer of the keys.
    - It verifies the token offline: signature, `iss`, `aud=rx-node:<id>`, `nbf`/`exp` with leeway, and the cid equal to `X-Rx-Cid` and unique (409 otherwise).
    - Refusals: 503 `hub_unavailable` without keys since boot; 401 `token_invalid`/`token_expired`; 403 for a revoked session or user.
    - It runs `session.hello`/`session.welcome`, then `auth.refresh` (same cid and audience, new scopes) and closes with 4401 at `exp` + 30 s.
    - Device-scoped messages (`device.attach`, `preset.select`, `device.retune`) are checked against the scope. They are answered `unsupported_type` until the device epics land.
    - These close the matching sessions with 4403: revoked sessions or users, the node's own certificate serial, and a control channel closed by the hub with 4403 (removed, disabled, re-enrolled). A signing key dropped from the key set closes its sessions with 4401.
    - The node reports media connections with `connection.opened` and `connection.closed`.
13. **Challenge signature (Q9).** `ctl.hello.challenge_sig` is dropped. The JWKS arrives on the same channel after `ctl.hello`, so it cannot verify it. The hub URI SAN of the mTLS client certificate authenticates the hub.
14. **Code placement (Q10).** The media endpoint lives in `internal/grid/infra/media`; its payloads are in `internal/protocol/rxv1/media`.

### Removal and revocation (GRID-015)

15. **Deletion** (existing) revokes the certificate. The hub pushes the revocation list and closes the channel with 4403, the node closes its media connections, and the gateway answers 404.
16. **Revocation (Q11).** `POST /api/v1/nodes/{id}/revoke` (admin) and `meshsdr hub node revoke` revoke the current and pending certificates and keep the row as `revoked` until a new enrollment token. Config-declared nodes may be revoked too.
17. **Devices (Q12).** Devices of a deleted node are deleted with it (§7.1 cascade). Disabling the schedules that reference them waits for the `schedules` table.

### The all role (GRID-003, GRID-014)

18. **Shape (Q13, Q14).** `meshsdr all` is one process that reads `hub.toml` and `node.toml` from one config directory.
    - **CA.** On first start it creates the hub CA (default `tls/ca.pem` and `tls/ca.key`) when both files are absent, never replacing one.
    - **Local node.** `node.id` defaults to `local` and `node.listen` to `127.0.0.1:8074`. The node is declared in the hub with origin `config`, so it can be disabled but not deleted, and it is listed like any node.
    - **Certificate.** The hub CA issues it in-process (`tls/node.pem`, `tls/node.key`), with no token, whenever the files hold no certificate pinned by the hub. The previous certificate is revoked.
    - **Revoked local node.** It is not re-enrolled until an admin issues a new token, and the hub then runs alone.
    - **Same path as remote nodes.** The local node uses the same control channel over loopback mTLS, the same gateway route and the same token checks. `meshsdr all config check` validates both files.
19. **Docker (Q15, Q16).**
    - The image runs `all` by default and exposes 443.
    - `/etc/meshsdr/tls` is a link to `/var/lib/meshsdr/tls`, so the CA and node certificates stay on the data volume without any change to the configuration.
    - `meshsdr hub migrate` keeps working before the first start, because the hub role does not default its CA files.
20. **Node-only artifact (Q17).** `-tags nogateway` leaves Caddy out (16 MiB instead of 55 MiB, stripped). `hub` and `all` then fail fast after loading the configuration, before opening the database.
    - The Dockerfile `prod-node` target ships it as `meshsdr-node`, published with the `-node` tag suffix.
    - CI lints and tests that build and fails if it links Caddy.
21. **Development and tests (Q18).**
    - The dev stack runs `all` with `tls_mode = off` on 8073; its certificates are created in `.infra/docker/dev/config/tls` (ignored by git), and `make lint` and `make test` cover both builds.
    - The end-to-end tests use a Go WebSocket client: browser → gateway (forward auth, token injection) → node over mTLS, with refusals, presence, removal, revocation, the node going offline and the all role.
    - A real-browser test comes with the receiver UI (RX-002).

### Configuration

| Key | Default |
|---|---|
| `gateway.mode` | `embedded` (`sidecar` is refused, ADR 0002) |
| `gateway.tls_mode` | `acme` |
| `gateway.https_listen` | `:443` |
| `gateway.http_listen` | — |
| `gateway.tls_cert`, `gateway.tls_key` | — |
| `gateway.acme_email`, `gateway.acme_ca` | — (Let's Encrypt) |
| `gateway.storage_dir` | `/var/lib/meshsdr/caddy` |
| `gateway.stream_close_delay`, `gateway.stream_timeout` | `2h`, `24h` |
| `gateway.max_body` | `1MiB` |
| `settings.listen_policy` | `anonymous` |

Removed: `hub.listen`.

## Spec inconsistencies

Recorded here; the spec is not edited.

1. **Gateway keys.**
   - §7.4 lists `gateway.https_listen`, `http_listen`, `acme_email`, `admin_api` and `trusted_proxies`; FEATURE lists `gateway.public_listen` and `admin_url`.
   - `gateway.stream_close_delay` and `gateway.max_body` (§4.6) are not in §7.4.
   - No spec key names the public certificate and key: INT-001 points to `tls.*`, which §7.4 uses for hub↔node mTLS, and the hub `tls.cert`/`tls.key` of the §7.4 example were dropped by ADR 0008.
2. **Proxies and listen addresses.** `gateway.trusted_proxies` (§2.3, §7.4, SR-13) and `http.trusted_proxies` (ADR 0009) disagree; with the hub served in-process, the gateway is no proxy hop. The hub address is `hub.listen` in FEATURE and ADR 0005, `hub.listen_internal` in §7.4, and Caddy is the only listener (ADR 0002).
3. **Removal.** GRID-015 archives the devices of a removed node, but §7.1 deletes them. Its "remove the gateway route" does not apply to option C. Its schedules have no table yet.
4. **Token TTL key and bounds.** The key is `auth.token_ttl_s` (GRID-012), `auth.token_ttl` (ADR 0011) or `auth.access_token_ttl` (§7.4). The maximum is 900 s (§5.8) vs 600 s (ADR 0011).
5. **Authz answers and limits.**
   - §5.16 lists 401/403/429/503, while GRID-011 adds 404 for unknown and disabled nodes.
   - §5.8's `hub_unavailable` has no HTTP status or close code; 503 is used.
   - "WS upgrades 10/min per IP (hub and node)" (§5.12) cannot be enforced on the node, which only sees the gateway: the authz enforces it.
6. **Hub challenge.** §4.3's `ctl.hello` challenge would be verified with a key set delivered later on the same channel.
7. **The all role.**
   - GRID-014 gives the local node the default id `local`, while `node.id` is required (ADR 0005): the default applies to `all` only.
   - §7.4 stores the all-role certificates under `/etc/<product>/tls` as the only files the product creates in the config directory, but `hub ca init` and `node enroll` (ADR 0008) write there too, and a container's config directory is not persistent.
   - §3.4 has the hub issue the certificate on first start, while GRID-003 enrolls over a loopback channel.
8. **INT-001 rights.** INT-001 gives admins ✅ on configuration files, which is OS-level access.
9. **Gateway duties.** GRID-011 gates on `gateway.admin_url` (sidecar mode, deferred). §3.2 puts security headers and size limits in the gateway, but the hub router applies them.

## Consequences

- The default `meshsdr` binary grows to about 55 MiB (stripped); the node-only artifact is about 16 MiB.
- Any Caddy reload (only a hub restart today) closes the proxied WebSockets after `stream_close_delay`.
- Media connections opened before a hub restart end with it. The interim key changes at every hub start until the identity keyring replaces it.
- Media connections are refused until the node has received the keys over its control channel.
- Tickets:
  - **Closed:** #19, #20, #22, #11, #85 and #15. The schedule part of #23 waits for the schedules epic, and #23 closes with this part.
  - **Waiting on acc-1:** ACC-007 (#54) closes when its keyring and revocation publisher are wired here, after acc-1 merges.

## References

- TECHNICAL_SPEC §3.4, §3.5, §4.3, §4.6, §4.10, §5.8, §5.9, §5.12, §5.16, §6.2, §7.4; FEATURE_SPEC GRID-003, GRID-011, GRID-012, GRID-014, GRID-015, INT-001.
- ADR 0002, ADR 0005, ADR 0008, ADR 0009, ADR 0011; spike branch `spike/spk-03-caddy`.
