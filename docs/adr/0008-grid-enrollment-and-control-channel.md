# ADR 0008: Grid enrollment, internal CA and control channel

- Status: Accepted
- Date: 2026-10-06
- Deciders: project owner
- Scope: epic #446, part `epic/grid-2`: GRID-004 (#12), GRID-005 (#13), GRID-006 (#14), GRID-007 (#15), GRID-008 (#16), GRID-009 (#17), GRID-010 (#18), GRID-013 (#21), GRID-016 (#24), GRID-017 (#25).

## Context

TECHNICAL_SPEC §4 (grid design), §6 (protocol), §7.1 (`nodes`, `devices`, `connections`, `node_capabilities`, `node_event_cursor`) and §7.4 (config keys) describe how a hub enrolls nodes, trusts them over mTLS and drives them through a hub-dialled control channel. FEATURE_SPEC §6 (GRID rows) describes the same features with different names and numbers in several places. This part of the epic is built before identity (users, roles, audit log), the DB settings store, the hub events WebSocket, the node media WebSocket, the node device configuration and the admin UI exist.

ADR 0002 (embedded Caddy, option C), ADR 0004 (coder/websocket, `internal/protocol/rxv1`), ADR 0005 (configuration loading) and ADR 0006 (DB adapter) apply.

## Decision

Numbers refer to the questions of the design proposal; every recommendation was accepted.

### Missing foundations

1. **Admin REST without identity (Q1).** The node, device and connection endpoints are declared in `openapi.yaml` and served, but every handler first asks an `Authorizer` port for the admin role. Production wiring uses a deny-all authorizer (401 `unauthenticated`) until the identity epic provides a real one; tests inject an allow-all one. The M0 admin path is the CLI: `meshsdr hub node add|list|show|token|remove`.
2. **DB settings (Q2).** `grid.heartbeat_interval_s` (10 s), `grid.offline_after_s` (60 s), `grid.enrollment_ttl_minutes` (60 min) and `retention.connections` (30 d) are fields of a `grid/app.Timings` struct filled with the FEATURE_SPEC defaults at wiring. A settings port replaces it when the settings store exists.
3. **Audit (Q3).** Enrollment, re-enrollment, revocation, removal and dropped node events go to an `Auditor` port. The only adapter logs the record at Info (`component=grid.infra.audit`). The identity epic creates `audit_log`; a later epic wires a DB adapter.
4. **Admin UI (Q4).** Admin › Nodes is deferred to the app-shell epic. CLI and REST cover M0.

### Domain

- `internal/grid/domain`: value objects `NodeID`, `NodeName`, `NodeURL`, `EnrollmentToken`, `EnrollmentKey`, `CertInfo` (fingerprint, serial, not-after), `BootID`, `SemVer`, `DeviceID`, `ConnectionID`; aggregates `Node` (enrollment state machine `pending → enrolled → revoked`, runtime status, origin and locked fields, optimistic `version`), `CapabilityReport`, `Device` and `Connection`, one repository each.
- Repositories live in `internal/grid/infra/sqlite`, their contract suites in `internal/grid/infra/repotest` (ADR 0006).
- Domain errors carry stable codes (`node_not_found`, `node_exists`, `entity_locked`, `setting_locked`, `version_conflict`, `enrollment_token_invalid`, `enrollment_token_expired`, `node_not_pending`, `node_revoked`, `device_id_conflict`, `node_unavailable`, …).

### Internal CA and certificates

- **Keys (Q5):** ECDSA P-256 for the CA and every leaf. Ed25519 stays reserved for access tokens.
- **Validity:** CA 10 years. Node leaf 90 days, renewed by the hub at 2/3 of its validity (checked on every `ctl.welcome` and daily) with `ctl.cert.renew`, for the same public key.
- **Node leaf SANs:** `urn:rx:node:<id>`, DNS `<id>.nodes.rx.internal` (ADR 0002 `server_name`), plus the `node.listen` IP or DNS name when it is specific. The hub always dials with `ServerName = <id>.nodes.rx.internal`, checks the chain against the CA, the URI SAN, the pinned fingerprint (`nodes.cert_fingerprint`, updated in the renewal transaction) and the revocation list.
- **Hub identity (Q6):** the hub id is the host of `hub.url`; the hub client certificate carries `urn:rx:hub:<host>`.
- **Hub client certificate (Q7):** minted in memory at start from the CA key (30 days, re-minted at 2/3). The hub has no `tls.cert`/`tls.key`; these keys exist on the node only.
- **CA creation (Q8):** `meshsdr hub ca init` writes `ca.pem` and `ca.key` (0600) into `<config-dir>/tls/` only when they are absent and never overwrites them. An operator-provided CA is accepted as well. Without `tls.ca_cert`, the hub runs with the grid disabled (no enrollment, no control channel) and says so at start.
- **TLS version (Q9):** TLS 1.3 only on every hub↔node link. It satisfies GRID-007's "1.2 or later" and §4.3's SHOULD.
- **Revocation (Q10):** a `revoked_certificates` table (serial, node, not-after, revoked-at, reason). Revoking or deleting a node records its serial; the hub never dials a revoked serial and pushes `ctl.revocations {cert_serials}` on every control connect. `POST /nodes/{id}/revoke` is GRID-015.

### Enrollment

- **Token storage (Q11):** the token is 32 random bytes, shown once as base64url together with the CA fingerprint (SHA-256 of the CA certificate, colon-separated hex). The hub stores `K = HKDF-SHA256(token, info "meshsdr enroll v1")` in `nodes.enrollment_token_digest`: K is the HMAC key of the exchange, single-use and valid for the TTL only. The node derives the same K from the token. A database leak lets an attacker complete the enrollment of a pending node within its TTL, nothing more. Config-declared nodes take their token from `nodes.<id>.enrollment_token`.
- **Exchange (Q12):** the hub dials `POST https://<url>/enroll`, accepting only the node's self-signed certificate for that exchange (pinned between the two phases). One path, two phases selected by `phase`:
  1. `{phase:"hello", node_id, hub_ca_chain, nonce, proof}` with `proof = HMAC(K, nonce ‖ node_id ‖ sha256(self-signed cert))` → `{csr, mac}` with `mac = HMAC(K, nonce ‖ sha256(csr))`;
  2. `{phase:"certificate", nonce, certificate_chain, mac}` with `mac = HMAC(K, nonce ‖ sha256(chain))` → 204.
  The node keeps phase-1 state in RAM for 60 s. It checks that the CA in `hub_ca_chain` matches `hub_trust.ca_fingerprint` before verifying the proof.
- **Who writes files (Q13):** `meshsdr node enroll [--token-file] [--ca-fingerprint]` is a one-off command: it serves `/enroll` until success or timeout, writes `tls.key` (0600), `tls.cert` and `hub_trust.ca_cert` atomically, and exits. Bare `meshsdr node` without a certificate keeps serving the pre-enrollment API (403 everywhere, 501 on `/enroll`) and tells the operator to run `node enroll`. Certificate renewal rewrites `tls.cert` atomically: a documented exception to "the binary never writes config files".
- **TTL (Q14):** 60 minutes (`grid.enrollment_ttl_minutes` default) for hub-issued tokens. A config-declared token has no TTL and is used once.
- After the 204 the hub records `enrolled_at`, the fingerprint, serial and not-after, and clears the token in one transaction, then opens the control channel. Re-enrollment (`POST /nodes/{id}/enrollment-token`, `meshsdr hub node token`) moves the node back to `pending`.

### Control channel (`rx-ctl.v1`)

- The hub dials `wss://<url>/control` over mTLS with coder/websocket. Read limit 64 KiB, compression disabled, WS ping every 15 s with a 30 s pong timeout. The node closes with 4408 when `ctl.hello` does not arrive within 5 s (timer), accepts `/control` only from a hub URI SAN (and `hub_trust.hub_identity` when set), and closes the older channel when a new authenticated one arrives.
- The hub `ControlManager` runs one supervisor per enrolled, enabled, non-revoked node, with full-jitter exponential back-off from 1 s to 60 s, reset after a channel stayed up 60 s.
- **WS adapter (Q15):** `internal/protocol/rxv1/wsconn` (426 pre-check, read loop helpers, bounded outbound queue closing with 4413 on overflow, ping). Payload structs live in `internal/protocol/rxv1/ctl`.
- **Delivery:** every node→hub event carries `payload.seq` per `boot_id`. The hub applies events in order in batches (≤ 250 ms or 500 events) and updates `node_event_cursor` in the same transaction, which makes replays idempotent, then answers `ctl.ack {upto_seq}`. The node keeps unacknowledged events in a RAM buffer and replays them after a reconnect. Snapshot events (`node.heartbeat`, `node.capabilities`, `device.state` per device) are coalesced latest-wins.
- **Buffer (Q16):** `node.event_buffer = { max_events = 10000, max_bytes = "16MiB" }`. On overflow the node drops by priority (diagnostics, then map, then decodes, then other events, oldest first) and reports `node.events_dropped {count, kinds}` on the next connection; the hub audits it.
- **Message subset (Q17):** the codec holds the full §4.4 catalogue. M0 handles hub→node `ctl.hello`, `ctl.ack`, `ctl.ping`, `ctl.capabilities.probe`, `ctl.cert.renew`, `ctl.revocations`, and node→hub `ctl.welcome`, `node.capabilities`, `node.heartbeat`, `device.state`, `connection.opened|closed|heartbeat`, `node.events_dropped`, `ctl.pong`, `ack`, `error`. Catalogued but unimplemented events are logged at Warn and acknowledged; catalogued but unimplemented requests get `error {code:"unsupported_type"}`.
- **`ctl.hello.challenge_sig` (Q18)** is omitted in M0: the hub is authenticated by its mTLS URI SAN. It is added with the token-signing key (GRID-012).
- **Heartbeat interval (Q19):** `ctl.hello` carries an additive `heartbeat_interval_ms` field.

### Heartbeat and status

- **Status values (Q20):** `nodes.status` ∈ `online`, `degraded`, `offline`, `unreachable`, `incompatible` (adds `incompatible` to §7.1). `enrolling` and `revoked` are derived from `enrollment_state` in the API.
- Rules, evaluated by a 5 s sweeper and on every event: a welcome on a compatible node → `online`; 2 missed heartbeats, clock offset > 1 s, `ntp_synced = false` or an older minor version → `degraded` with a hint code; no heartbeat for `grid.offline_after_s` or a lost channel → `offline`; dial failures before any welcome since hub start → `unreachable`; a major mismatch → `incompatible`. The database is written on transitions and on heartbeats only; transitions are published on an in-process event port (`node.status`) after commit.
- **Load history (Q21):** a RAM ring of the last 360 heartbeats per node (1 h at 10 s), returned by `GET /nodes/{id}`.

### Version compatibility

- An `internal/version` package holds the product version (`-ldflags -X`, falling back to the module build info).
- The subprotocol must be `rx-ctl.v1` (426 otherwise). With the hub at minor N of a major: N or N+1 → compatible; N-1 → `degraded` (`upgrade_recommended`); older → `incompatible` (`node_too_old`); another major → `incompatible` (`node_incompatible`). An incompatible node keeps only welcome, capabilities and heartbeats, and receives no state.
- **Pre-1.0 and dev builds (Q22):** for `0.y.z`, `y` plays the role of the major and `z` the minor. A development build (`dev`, `(devel)`, a pseudo-version) is always compatible and logged at Warn.

### Capabilities and devices

- **Storage (Q23):** the §7.1 `node_capabilities` rows (one per namespaced capability) plus an auxiliary `node_capability_reports` table (node, full JSON document, `capabilities_hash`, product version, protocols, platform, reported-at). A report replaces every row of the node in one transaction; an unchanged hash is skipped.
- The M0 node probe reports the platform (OS, arch, CPU count, memory), product version, protocols and devices; driver, decoder and codec probes come with their epics.
- **Device registry:** the hub mirrors `devices[]` into `devices`. A device missing from a report keeps its row with `online = false`, `runtime_state = 'unavailable'`. `device.state` updates the runtime columns.
- **Conflicts (Q24):** `device_id_conflict` when another node owns the id, or when the type of an existing id changes. The conflicting device is skipped and the rest of the report applies.
- **Node device config (Q25):** `node.toml` gets `[devices.<id>]` with the §7.4 keys `name`, `type`, `enabled`, `freq_range`, `sample_rates`, `listen_policy`, `operator_can_retune`, `always_on`, `scheduler_enabled`. `driver` and the other device tables come with the device epic.
- **Missing parents (Q26):** `devices.active_preset_id` and `connections.user_id` are created without foreign keys; the presets and identity epics add them with a table rebuild.
- Disabling schedules of stale devices (GRID-016) waits for the `schedules` table.

### Presence

- A `Connection` aggregate with `Open`, `Heartbeat` (one batched `UPDATE … IN`), `Close` and `CloseStale`. A reaper (15 s) closes rows stale for more than 45 s (`heartbeat_timeout`). At start the hub closes every open row (`hub_restart`). Rows of a node whose channel is lost for more than 45 s are closed with `node_lost`; a new `boot_id` closes the old boot's rows with `node_restart`. Closed rows are deleted after 30 days.
- **Producers (Q27):** `/api/ws` and the media WS do not exist yet. Media rows come from the control channel `connection.*` events; a `connection.opened` for an unknown id creates the row (the gateway authz will create it first once GRID-011 exists).

### Configuration keys (Q28, Q29)

TECHNICAL_SPEC §7.4 names win over FEATURE_SPEC names:

| Role | Keys |
|---|---|
| hub | `tls.ca_cert`, `tls.ca_key` (secret); `nodes.<id>.url`, `nodes.<id>.name`, `nodes.<id>.enrollment_token` (secret) |
| node | `tls.cert`, `tls.key`; `hub_trust.ca_cert`, `hub_trust.ca_fingerprint` (added, from §4.2), `hub_trust.hub_identity`, `hub_trust.enrollment_token` (secret); `node.event_buffer.max_events`, `node.event_buffer.max_bytes`; `devices.<id>.*` |

Not adopted: `tls.cert_file`, `tls.key_file`, `tls.ca_file`, `tls.client_ca_file`, `nodes.<id>.ca`, `node.enrollment_token`, node-side `hub.enrollment_token` and `hub.ca_fingerprint`, `grid.event_buffer_size`.

A config-declared node that disappears from the files becomes `origin = 'db'` with no locked fields; `config.prune_removed_entities` is not implemented in M0.

### REST (`openapi.yaml`)

`GET`/`POST /nodes`, `GET`/`PATCH`/`DELETE /nodes/{id}`, `POST /nodes/{id}/enrollment-token`, `GET /nodes/{id}/capabilities`, `POST /nodes/{id}/capabilities/probe`, `GET /devices`, `GET /devices/{id}`, `GET /connections`. All admin-only for now (Q1). Deferred: the public `/nodes` subset, `POST /nodes/{id}/revoke`, `/nodes/{id}/logs`.

### Tickets (Q30)

The PR closes #12, #14, #16, #18 and #21, and references (without closing) #15 (gateway certificate: GRID-011; access-token exception: GRID-012), #13 (admin UI), #17 (admin view), #24 (schedules) and #25 (producers).

## Implementation notes

- **Node private key path.** `tls.key` on the node is a plain path, not a `{ file = … }` secret reference: the node writes it during enrollment, so it cannot be resolved at config load. The file is created 0600 and its mode is checked when it is read.
- **Relative paths.** `tls.*` and `hub_trust.ca_cert` paths are resolved against the config directory, like secret files.
- **Map keys and env.** `nodes.<id>.*` and `devices.<id>.*` are file-only: an env override would need a dynamic variable name (`MESHSDR_NODES__<ID>__URL`) that the env loader cannot enumerate.
- **Migrations** of this part start at `00003` (`00002` belongs to the identity epic): `00003` nodes, revocation list and event cursor, `00004` capabilities, `00005` devices, `00006` connections. Timestamps are Unix milliseconds, UUIDs 16-byte blobs, as in §7.2.
- **Optimistic concurrency on REST.** `PATCH /nodes/{id}` carries the expected `version` in the body and answers 409 `version_conflict` (§7.1), not `If-Match`/412 (§6.10).
- **Device order.** TOML tables lose their order once decoded, so `devices.sort_order` follows the device ids in lexical order.
- **Media presence rows** created from `connection.opened` have an empty `ip` and role 0 until the gateway authz creates them first (GRID-011).
- **`GET /devices`** is admin-only until the listen policy can be evaluated (identity epic); `GET /connections` returns the count to everyone and the rows to admins.
- **WebSocket deadlines.** `wsconn.Accept` clears the server read/write deadlines before the hijack, otherwise `ReadTimeout` would cut long-lived channels.

## Spec inconsistencies

Recorded here; the spec is not edited.

1. §4.2 stores the enrollment token hashed, yet the hub needs the token to compute the HMAC proof.
2. §4.2 steps 4–6 need two requests, but the node serves "only `POST /enroll`".
3. §4.1 calls the node `tls.*` files config written by the node; §7.4 says the product never writes config files.
4. Token TTL: 24 h (§4.2) vs `grid.enrollment_ttl_minutes` = 60 (FEATURE).
5. Token key: `hub.enrollment_token` (§4.2), `hub_trust.enrollment_token` (§7.4 example), `node.enrollment_token` (FEATURE). `hub.ca_fingerprint` (§4.2) is missing from §7.4.
6. `tls.cert`, `tls.key`, `tls.ca_cert`, `tls.ca_key` (TECH) vs `tls.cert_file`, `tls.key_file`, `tls.ca_file`, `tls.client_ca_file` (FEATURE).
7. `nodes.<id>.url` (§7.4) vs `nodes.<id>.address` (§4.2) vs `nodes.<id>.ca` or pinned fingerprint (FEATURE); `name` (§7.1) vs `display_name` (§4.1, §4.2).
8. `nodes` (§7.1) has no `enrollment_token_hash`, `enrolled_at` or `cert_serial`, all needed by §4.2; its `status` enum (online, degraded, offline, unreachable) differs from the §4.5 states (enrolling, online, degraded, offline, incompatible, revoked).
9. Offline after 3 missed heartbeats (≤ 30 s, §2.3, §4.5) vs degraded after 2 missed and offline after `grid.offline_after_s` = 60 s (FEATURE). Clock warning at ±500 ms (§7.1) vs degraded above 1 s (§4.5).
10. An older node is `degraded` (§4.8) vs refused with `node_too_old` (§7.5).
11. TLS 1.3 SHOULD be the only version (§4.3) vs "TLS 1.2 or later" (GRID-007).
12. `node_capabilities` is one row per node with a JSON document (§4.7) vs one row per capability (§7.1); the event is `node.capabilities` (§4.4) vs `capabilities.report` (§7.1).
13. Event buffer: 50 000 events / 64 MiB (§2.3) vs 10 000 / 16 MiB (§7.3) vs `grid.event_buffer_size` (FEATURE); drop by priority (TECH) vs oldest first (FEATURE); `node.events_dropped` (§4.9) vs `events.dropped` (§7.3).
14. Device id conflict is about the owning node (§7.1) vs a different device type (FEATURE). The `device.state` states (§4.4) differ from `devices.runtime_state` (§7.1), which has no `offline`.
15. Presence liveness is `connection.heartbeat` (§4.4) vs `session.stats` (§7.3); the event carries `ip_hash` but the table stores `ip`; the event id is `cid` while `connections.id` is a UUID; `presence.heartbeat_interval` and `presence.stale_after` are not in the §7.4 namespaces.
16. The hub id used in `urn:rx:hub:<hub-id>` is never defined; `ctl.hello.challenge_sig` depends on the token-signing key of another epic.
17. GRID-008 has the hub push "settings, presets, schedules", while §4.4 says `ctl.state.apply` carries no device settings.
18. The `devices` registry is written "from `capabilities.report`" (§7.1) while the event is `node.capabilities` (§4.4, §4.7).
