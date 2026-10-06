# ADR 0010: Settings store, retention jobs and admin pages

- Status: Accepted
- Date: 2026-10-06
- Deciders: project owner
- Scope: epic #449 (Administration), part `epic/adm-1`: ADM-002 #60, ADM-010 #68, ADM-011 #69, ADM-006 #64, ADM-003 #61, ADM-004 #62, ADM-005 #63, ADM-001 #59, ADM-008 #66, ADM-009 #67, and UI-001 #70.

## Context

TECHNICAL_SPEC §7.1 (`settings`, `files`, `file_blobs`, `job_runs`), §7.3 (retention jobs, file blobs) and §7.4 (locking semantics, the `settings` namespace, the JSON Schema with `x-scope`, `lockable` and `secret`) describe admin-editable settings that a config file can lock. FEATURE_SPEC §6.4 (ADM), §9.1 and §10.13 describe the admin pages. ADR 0005 loads `hub.toml`, the env and origins. ADR 0007 added the first `[settings]` keys (`ui.theme_mode`, `receiver.usage_policy_text`) behind interim config-only adapters. ADR 0009 left session lifetimes, login throttling and session retention as code constants until this store exists.

Owner decisions taken before this part: precedence env > config files > DB settings > defaults; config/env keys are locked (409 `setting_locked`) and shown with their origin; Go structs generate the JSON Schema through invopop/jsonschema; spec-first OpenAPI with `x-meshsdr-access` on every operation; JSON only on `/api/v1`; htmx fragments on the page URL; pages render through the shell renderer; light DDD; stdlib tests; no third-party dependency without approval.

## Decision

Numbers refer to the questions of the design proposal. The owner accepted every recommendation.

### Module

`internal/settings/{domain,app,infra,http}` with `wire.go`:

- `domain`: the `Key` and `Value` value objects; the `Setting` aggregate (one `settings` row); `Definition` (default, flags and input constraints of a key); `Effective` (value, source `cfg|db|default`, origin, lock, version, shadowed DB value); `Change`; the repository interface; typed errors `unknown_setting`, `invalid_setting` (with one violation per key), `setting_locked`, `version_conflict`, `secrets_unavailable`.
- `app`: `Store` (load, resolve, validate, save, audit, publish), `EffectiveConfig` (ADM-010).
- `infra/sqlite`, `infra/repotest`.
- `http`: REST handlers and the admin pages.

### Schema and validation (Q1)

- `config.Settings` (the `[settings]` table of `hub.toml`) stays the single source of the keys, Go types, defaults, descriptions and constraints. invopop/jsonschema generates the schema. Keys carry `x-scope: global`, `lockable: true`, `secret` when relevant, and `x-apply`, `x-public`, `x-widget` from `jsonschema_extras`.
- One validator, `config.ValidateSetting(key, json)`, checks a value whatever its source (file, env, DB, API):
  1. decode the JSON into the leaf's Go type (the unit types of ADR 0005, such as `Duration`);
  2. check the keywords the generated schema holds (`enum`, `minimum`, `maximum`, `minLength`, `maxLength`, `pattern`, `format`, object `properties`, plus `x-min-duration` and `x-max-duration`) with a small in-house checker that reads the schema itself;
  3. run the key's semantic hook (domain value objects, such as `shell/domain.NewPolicyText`).
- Cross-key rules (`auth.lockout.lock_after` above `delay_after`, `max_lock` at least `lock_for`) run on the resulting effective set, at load and on every write.
- The config loader runs the same validator for every `[settings]` key set in a file or the env. It replaces the hand-written checks of these keys.
- DB keys drop the `settings.` prefix (§7.1: `ui.theme_mode`). `GET /settings/schema` serves the `settings` sub-schema with those names.

### Storage (Q2)

- Migration `00008_settings.sql`: `settings` as in §7.1 (`key`, `value` JSON, `value_enc`, `schema_version`, `updated_by` → `users` ON DELETE SET NULL, `updated_at`, `version`, check that exactly one of `value` and `value_enc` is set) plus a one-row `settings_revision` counter.
- Every write takes the next global revision as the row `version`, so a version never repeats after a reset (no ABA). A reset (`null`) deletes the row.
- `PATCH /settings` carries `{"values": {key: value|null}, "versions": {key: n}}`, where version 0 means "the key has no DB row". The write is atomic. Answers: 422 `invalid_setting` with `errors[]` (one per key), 409 `setting_locked` (audited `denied`), 409 `version_conflict` naming the keys. `GET /settings` sends `ETag: "<revision>"` for information.

### Precedence and origin

Effective value = env > files (locked, origin `hub.toml:42` or `env:MESHSDR_…`) > DB row > default. A locked key keeps its DB value, shown as overridden. At start every DB row is validated: an invalid row or a row whose key is unknown is ignored (the default applies), logged at Warn and, for invalid values, audited with the system actor (§7.4 step 6).

### Live apply (Q3)

- `Store` holds an immutable `Snapshot` (effective values, decoded to their Go types) in an `atomic.Pointer`, rebuilt after each committed write.
- Consumers read it on every use through small ports declared on their side (below). Holders of derived state register `Store.Subscribe(func(*Snapshot))`, called after the swap.
- `x-apply` marks keys that need a restart (`restart`); every key of this part applies live.

### Consumer ports

Each consumer declares its port in its own `app` package. An adapter in its `infra` package reads the store through a small interface declared on the consumer side and satisfied by `*settings/app.Store` (`String`, `Bool`, `Int`, `Duration`, `Rate` by key, each reading the current snapshot):

| Consumer | Port | Adapter | Keys |
|---|---|---|---|
| shell | `shell/app.Settings` (`ThemeMode`, `SiteName`, `PolicyURL`), `shell/app.PolicySettings` (`UsagePolicy`) | `shell/infra.StoreSettings` | `ui.theme_mode`, `receiver.name`, `receiver.usage_policy_url`, `receiver.usage_policy_text` |
| identity | `identity/app.Policies` (`SessionPolicy()`, `ThrottlePolicy()`) | `identity/infra/settingsrc.Policies` | `session.*`, `auth.lockout.*` |
| identity | per-address limiter rate (`settingsrc.Policies.LoginRate`, read by `memory.NewDynamicIPLimiter` on each attempt) | same | `auth.login_rate_limit` |
| identity jobs | `identity/app.Retention` (`SessionRetention()`, `AuditRetention()`, never under 30 days) | same | `retention.sessions`, `retention.audit_log` |

The identity part 2 (`epic/acc-1`) reads these through the same ports, and adds its keys to `config.Settings` and its jobs to the scheduler (`jobs/app.Scheduler.Register`, `jobs/app.Store` for the retention page). Without a store (CLI commands), identity uses `app.DefaultPolicies()` and the built-in retention.

### Keys of this part

| Key | Type | Default | Page |
|---|---|---|---|
| `listen_policy` | `anonymous` \| `registered` | `anonymous` | Access |
| `receiver.name`, `.location`, `.photo_title` | string | `MeshSDR`, —, — | Site |
| `receiver.altitude_m` | integer, m | `0` | Site |
| `receiver.admin_email`, `.admin_email_public` | e-mail, boolean | —, `false` | Site |
| `receiver.gps` | `{lat, lon}` | — | Site |
| `receiver.country` | ISO 3166-1 alpha-2 | — | Site |
| `receiver.help_url` | http(s) URL | — | Site |
| `receiver.photo_desc` | Markdown | — | Site |
| `receiver.usage_policy_text` (ADR 0007), `receiver.usage_policy_url` | Markdown, URL or path | built-in, `/policy` | Site |
| `bandplan.region` | `0`–`3` | `0` | Site |
| `ui.theme_mode` | `light` \| `dark` \| `auto` | `auto` | Look & feel |
| `ui.shortcut_set` | `default` \| `off` | `default` | Look & feel |
| `ui.tuning_precision` | `0`–`6` | `2` | Look & feel |
| `ui.layout.side_panel_open`, `.spectrum`, `.bandplan` | boolean | `true`, `false`, `true` | Look & feel |
| `ui.layout.default_tab` | side panel tab | `decoders` | Look & feel |
| `ui.layout.frequency_format` | `radio` \| `locale` | `radio` | Look & feel |
| `ui.recorder_enabled` | boolean | `true` | Access |
| `bookmarks.eibi_range_km`, `.repeater_range_km` | `0`–`20000` km | `0` | Look & feel |
| `session.idle_timeout`, `.absolute_timeout`, `.remember_me_timeout` (Q4) | duration | `24h`, `24h`, `30d` | Access |
| `auth.login_rate_limit` (Q5) | `<count>/<window>` per client address | `5/1m` | Access |
| `auth.lockout.delay_after`, `.lock_after`, `.lock_for`, `.max_lock` (Q5) | count, duration | `5`, `10`, `15m`, `24h` | Access |
| `retention.sessions` | duration ≥ 1d | `30d` | Retention |
| `retention.audit_log` | duration ≥ 30d | `365d` | Retention |

Keys without a consumer yet (Q14) are defined, editable and exposed by `GET /settings/public` (`x-public`). The GPS map picker waits for the MAP epic; the page has latitude and longitude fields (Q15). `receiver.usage_policy_url` is added next to ADR 0007's text key (Q17). These keys live in `[settings]`, apart from the config-only `[auth]` bootstrap table.

### Secrets (Q16)

`value_enc` exists in the table, but no key of this part is secret. Writing a key the schema marks `secret` answers 409 `secrets_unavailable` until `secrets.master_key` and the AEAD exist.

### Audit

Every save writes one `audit_log` row per key (`settings.update` or `settings.reset`, target `setting`/`<key>`, before and after values, secrets masked) in the transaction of the write, through an `Auditor` port adapted to identity's `AuditLog`.

### REST (`openapi.yaml`)

| Operation | Access |
|---|---|
| `GET /settings` | admin |
| `PATCH /settings` | admin |
| `DELETE /settings/{key}` (same as PATCH `null`; optional `version` query, the current version when omitted) | admin |
| `GET /settings/schema` | admin |
| `GET /settings/public` | anonymous |
| `GET /config/effective` (ADM-010, `?download=true` for an attachment) | admin |
| `GET /retention`, `POST /retention/{store}/purge` | admin |
| `PUT /branding/{slot}` (multipart), `DELETE /branding/{slot}` | admin |
| `GET /branding/{slot}` (`If-None-Match` answers 304) | anonymous |

The error code for a locked key is `setting_locked` (Q18). Path, query and header parameters need `github.com/oapi-codegen/runtime`, added at the version `epic/grid-2` introduces (v1.7.0).

### Admin UI (Q6, Q7, Q8)

- Sections follow FEATURE_SPEC §10.13: `/admin` (Overview), `/admin/site`, `/admin/access`, `/admin/look-and-feel`, `/admin/retention` (Data & retention), `/admin/system` (effective configuration). A section list sits on the left on wide screens and above the content on narrow ones.
- Every admin page is guarded by `Require(admin)`, which also checks `admin.allowed_networks`. The top bar shows an "Admin" link to admins coming from an allowed network; UI-006 adds the full navigation later.
- Forms are rendered from the key definitions: input type from the Go type, `format` and `x-widget`; bounds and options from the schema. A test checks that every key belongs to exactly one form section. A locked key is a read-only (focusable, not submitted) text field with a lock label and its origin; the server ignores locked keys in a submission. Each section posts form-encoded data with htmx to the page URL and gets the section back, with inline errors linked from a summary, or a saved notice. Hidden inputs carry the versions. Only fields that differ from the effective value are written, so untouched defaults never become DB rows. An empty field resets its key to the default.
- `static/js/admin-form.js` marks a dirty form, asks before leaving it (boosted navigation and unload) and focuses the error summary of a rejected form.
- The overview shows the settings counts (locked, set by an admin) and failing retention jobs. The nodes, devices and web-cache parts of the ADM-001 health summary come with their epics.

### Retention and jobs (Q10, Q11)

- `internal/jobs`: a `Scheduler` worker runs registered jobs at start then every period. `job_runs` (`00009_job_runs.sql`) records each run. A job never overlaps itself: starting takes the row atomically, and a run older than one hour is considered dead. Errors are logged once by the scheduler.
- Jobs of this part: `sessions.reap` (hourly; replaces the identity `SessionReaper` worker with the same behaviour) and `audit.purge` (daily). Each deletes in batches of 10 000 rows.
- `jobs/app.Retention` lists the stores that exist (sessions, audit log), with their setting key, row count and size (`dbstat` when available), the last run and a "purge now" action that runs the job synchronously (409 `job_running` when it is already running).
- "Purge now" on the audit log applies the policy only. `retention.audit_log` cannot go below 30 days, and the purge is audited.

### Receiver images (Q9, Q9b)

- `00010_files.sql` creates `files` and `file_blobs` as in §7.1. A minimal `internal/files` module owns them. FIL extends it later.
- `PUT /branding/{avatar|panorama}` takes `multipart/form-data` with one `file` part. The JSON-only middleware accepts multipart on operations whose OpenAPI request body declares it.
- The declared size is checked before reading the body, which is capped: 250 KiB for the avatar, 2 MiB for the panorama. The upload must be PNG, JPEG or WebP by magic bytes; SVG and GIF are refused. Dimensions are read first (`image.DecodeConfig`) and must not exceed 8192 × 8192.
- The image is decoded with the stdlib and `golang.org/x/image/webp`, then re-encoded: avatar to PNG, panorama to JPEG at quality 85. Re-encoding drops metadata (EXIF, GPS). The result must fit `files.receiver_image_max_size` (2 MiB).
- The file is stored as a `files` row (`receiver_avatar` or `receiver_photo`) with 1 MiB chunks. A new upload replaces the previous file of the slot in the same transaction. "Restore default" deletes the row. Without a row the slot is empty and the page shows no image. Every change is audited (`receiver_image.update`, `receiver_image.remove`).
- `GET /branding/{slot}` serves the image with its type, an ETag (SHA-256), `nosniff` and `Cache-Control: no-cache`.
- The admin Site page has an images section; its upload (`POST /admin/site/images`, multipart) and "restore default" (`POST /admin/site/images/remove`) actions return the section as an htmx fragment.

### Devices (Q12)

grid-2 merged first, so ADM-008 and ADM-009 are part of this epic part. They need no migration:

- The node-config flags of a device (`enabled`, `listen_policy`, `operator_can_retune`, `always_on`, `scheduler_enabled`) are already mirrored into the registry by grid-2 (`devices.capabilities` JSON).
- The "no longer reported" marker already exists: when an online node reports its devices without one, grid-2 sets `runtime_state = 'unavailable'` with `runtime_reason = 'not_reported'` and the time in `runtime_state_at`. `Device.Missing()` reads it; a device of an offline node is not missing. `GET /devices/{id}` exposes it as `missing_since`.
- `GET /devices/{id}` is readable by operators (ADM-008); `GET /devices` stays admin-only.
- `DELETE /devices/{id}` (admin) forgets a missing device. The deletion is conditional in SQL, so a device reported again meanwhile is kept; otherwise it answers 409 `device_reported`. It is audited (`device.forget`). Presets are device-independent and schedules do not exist yet, so nothing else changes.
- `/admin/devices` lists the registry and `/admin/devices/{id}` shows a device read-only, "defined in the node config of <node>". Operators open both (their admin section list holds only Devices); the "Forget this device" action (`POST /admin/devices/{id}/forget`) is for admins. The list is the minimum needed to reach the detail; ADM-007 (device list with capabilities, active preset and listener count) stays its own ticket. The presets compatible with a device come with the presets epic.

### Accessibility (Q13)

The a11y job seeds an admin (`meshsdr --noninteractive hub user add`, compose service `seed`), signs in once per hub through the login page, checks the pages marked `admin` in `urls.txt` in every theme mode, and checks a rejected admin form (inline errors and summary).

## Implementation notes

- `/api/v1` authorises an operation (x-meshsdr-access) before reading its body and bounds every JSON body to 1 MiB (413 beyond); uploads keep their own caps. The strict policy middleware checks the access level again after decoding.
- Receiver images: each slot caps the pixel count before decoding (avatar 1024 × 1024, panorama 40 MP), 16-bit colour models are refused, and one image is decoded at a time.
- An invalid DB setting is ignored but keeps its row version, so the UI or the API can replace or reset it.
- `audit.purge` records each purge that deleted entries (`retention.purge`, system actor). The scheduler ends, as failed, the runs a stopped hub left in progress, and holds one lock per job.
- `auth.login_rate_limit` allows at most 100 attempts per window of at least one minute.

- The new admin settings keys and their defaults are documented in `.infra/config/hub.toml.example`.
- `config.Duration` formats whole weeks with `w` and other values from days down (`30d`, not `4w2d`). A default of 24 hours shows as `1d`.
- Receiver image chunks are written in the transaction that replaces the previous image, not one transaction per chunk (§7.3): an image is at most 2 MiB, two chunks.
- `meshsdr hub config check` does not open the database yet: unknown and invalid DB settings are reported at hub start (Warn, and audit for invalid values), not by the command (§7.4 step 6, read-only mode).
- The `db.maintenance` job (`PRAGMA optimize`, `incremental_vacuum`) is not part of this part.

## Spec inconsistencies

Recorded here; the spec is not edited.

1. The code for a write to a locked key is `locked_by_config` (§6.10) or `setting_locked` (§7.4). `setting_locked` is used.
2. Concurrency is `If-Match`/412 (§6.10), "version or ETag" (ADM-002) or a body version with 409 (ADR 0008). Body versions with 409 are used.
3. Key names: `ui_theme`, `ui_layout_defaults`, `ui_shortcut_set` and `receiver_name` (UI-001, §10) vs `ui.theme_mode`, `ui.layout.*`, `ui.shortcut_set` and `receiver.name` (§7.1, §7.4, ADM). The dotted names are used.
4. Session lifetimes are config-only `auth.session_idle_timeout`/`auth.session_max_lifetime` (§7.4) or DB `session.idle_timeout`/`session.absolute_timeout` (FEATURE §9.1), with other defaults (ADR 0009 #2). The DB keys are used, plus `session.remember_me_timeout`.
5. `auth.login_rate_limit` is a DB "5/15m" (FEATURE) while §5.12 describes another algorithm, and `auth.*` is a config-only bootstrap namespace in §7.4.
6. `retention.sessions` is 30 days (§7.3), 7 days (FEATURE §9.1) or "deleted at absolute expiry" (§2.3). 30 days is kept (ADR 0009).
7. `retention.decoder_diagnostics` is 7 days (§2.3, FEATURE) or 14 days (§7.3); hourly rollups 180 or 90 days.
8. `connections` keep 24 h of live rows and 90 days of closed rows with truncated IPs (§2.3), or 30 days (§7.3, FEATURE).
9. File retention keys: `files.retention_count`, `files.retention_days`, `files.max_total_bytes` (FEATURE) vs `files.keep_per_kind`, `files.max_age`, `files.max_total_size`, `files.max_file_size` (§7.2, §7.3) vs `files.max_bytes` 5 GiB and `files.keep_days` (§2.3) vs 2 GiB.
10. Receiver images are kind `branding` at `/branding/{avatar|panorama}` (ADM-004) or kinds `receiver_avatar`/`receiver_photo` at `POST /uploads/images` (§6.10, §7.1). The avatar is capped at 250 KiB (ADM-004) while `files.receiver_image_max_size` is 2 MiB for both. "Restore default deletes the row" vs the soft delete of `files.deleted_at`.
11. Altitude is `receiver.altitude_m` (FEATURE) or `asl` (§7.4 example).
12. The usage policy is `receiver.usage_policy_url` (FEATURE) or `receiver.usage_policy_text` (ADR 0007).
13. The audit log "MUST NOT be deletable through the UI" (§2.3), yet ADM-011 offers "purge now" for it.
14. ADM-002 shows "applies after restart of node X", but settings are hub-only and the DB stores no device setting; only grid timings reach nodes, at reconnect.
15. Admin IA: ADM rows place pages under "General › Receiver, Images, Access, Retention, Effective configuration", §10.13 has Site, Access, Data & retention and System; the ADM-001 sub-page list differs from §10.13. `bandplan.region` is under Receiver (ADM-003) or Look & feel (§10.13).
16. `x-scope` allows `device` and `preset` (§7.4), but the DB stores no device setting: every settings key is global.
17. §7.1 `settings.value` is null for secrets and exactly one of `value`/`value_enc` is set, so a reset cannot be a row: it deletes the row.
18. A secret is exposed as `{set: true}` (§6.10) or `{set: true, origin: "config"}` (§7.4).
19. ADM-008 grants operators read access, while `GET /devices` is admin-only (ADR 0008). ADM-008 lists `rf_gain`, `ppm`, `lfo_offset` and `services`, which nodes do not report yet.
20. ADM-009 needs "no longer reported", which §7.1 `devices` cannot express, and "disable schedules", which have no table yet.
21. The ADM-001 health summary needs web caches and failed devices, which have no table or producer in M0.
22. Retention deletes in "bounded batches" (§2.3) or ≤ 10 000 rows per transaction (§7.3); `job_runs` appears only in the auxiliary table list.

## Consequences

- `shell/infra.ConfigSettings` and the identity constants are replaced by store-backed adapters; their `app` and `http` packages keep their ports.
- `wire.Hub` takes the config origins, loads the store before serving and runs the job scheduler instead of the session reaper.
- The a11y job needs an admin account and signs in.
