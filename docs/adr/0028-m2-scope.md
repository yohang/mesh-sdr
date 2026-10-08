# ADR 0028: M2 scope (KISS)

- Status: Proposed
- Date: 2026-10-08
- Deciders: project owner
- Related: ADR 0017 (process supervision), ADR 0020 (desired state), ADR 0024 (diagnostics trim), ADR 0026 (listen policy)

## Context

M2 ("Digital modes", milestone #4) holds 52 features (248 points) in four epics (RX, DEM, DEC, FIL). It adds the digital decoders on the nodes, each with a running / unavailable / error status, plus decode persistence and the Files gallery. Several tickets reach into M3 (map) and M4 (reporting and background services), and the product specification disagrees with itself on rx.v1 message names, decoder queue keys and file settings. The owner wants the smallest M2 that decodes on a node and keeps what it decodes.

## Decision

- **Tools**:
  - Pinned Debian trixie apt packages in the runtime stage: `wsjtx` 2.7 (`jt9`, `wsprd`), `js8call` 2.2 (`js8`), `direwolf` 1.7, `multimon-ng` 1.3.1 and `rtl-433` 25.02.
  - `csdr-cwskimmer` (luarvique) is built from a pinned tarball in the `natives` stage.
  - The native decoders (PSK, RTTY, SITOR-B, CW, SSTV, FAX) wrap libcsdr++ through the cgo shim (ADR 0014).
  - **MSK144** (DEC-028) moves to M6 Low priority: no decoder source is available.
- **Map and spotting**: M2 persists `decoded_messages` only. Parsers keep the call, locator and position in the JSON payload. `map_features` comes in M3; the outbox and the reporters come in M4.
- **Files source**:
  - Every complete SSTV or FAX image of a listener session, and the CW/RTTY skimmer text logs, are auto-saved node → hub.
  - Per-file caps are 8 MiB, or 16 MiB for FAX. Short images are discarded. Retention bounds the volume.
- **Decodes page** (FEATURE_SPEC §10.11): a simple paginated htmx table of `decoded_messages`, with mode, device and time filters and JS8 thread grouping. Rights follow `listen_policy`.
- **rx.v1 names**: the code catalogue (TECHNICAL_SPEC) is kept. The client sends `decoder.set {demod_id, decoder|null, offset_hz, options}`; the node sends `decode` events; the secondary FFT is stream kind `fft2` on binary frame `0x03`. The FEATURE_SPEC names (`decoder.start`, `secondary_config`, `secondary_offset_freq`) are not implemented.
- **Duplicates**: one decoder per listener demod. The hub drops duplicates with a unique `dedup_key` = hash(device, mode, slot or second, frequency, text).
- **Settings split**:
  - Node config: `decoders.batch_workers` (default cores ÷ 2) and `decoders.queue_length`.
  - Every decoding parameter is a DB setting on a new **Admin › Decoding** page: WSJT/JS8 depths, FST4/FST4W/Q65 intervals, JS8 profiles, `fax_*`, `paging_*`, `cw_showcw`, `digimodes_fft_size`, `ism_report_levels`.
  - The hub sends the decoder settings to the nodes inside `ctl.state.apply` (the desired-state message, ADR 0020); there is no new control message.
- **Decoder queue overflow**: the queue drops the oldest job and sets the session status to `error` with reason `queue_overflow` (DEC-025 wins over the TECHNICAL_SPEC skip-newest rule).
- **Files visibility**: follows `listen_policy` only (ADR 0026). Rows are stored as `public`; there is no `files.visibility` setting.
- **Files delete**: operators and admins delete one file; admins also bulk delete by filter. Both are audited, through htmx forms only.
- **Files retention**: the ticket keys `files.retention_count` (per kind, default 20), `files.retention_days` (default off) and `files.max_total_bytes` (default off), applied after each insert and by a periodic job. The TECHNICAL_SPEC keys (`files.keep_per_kind`, `files.max_age`, `files.max_bytes`, `files.max_total_size`, `files.max_file_size`) are not used.
- **`decoded_messages` retention**: one days setting (`retention.decoded_messages`, default 30) plus one row cap, on the jobs retention view. Per-family overrides come later.

## Consequences

- M2: 52 → 51 features, 248 → 243 points. M6 gains DEC-028 (5 points). The DEC epic table drops DEC-028.
- The amended ticket bodies start with an "M2 KISS (ADR 0028)" note. DEC-002, DEC-004 and DEC-005 use the rx.v1 names above; the map and spotting tickets keep their fields in the payload; DEC-025, DEC-047, FIL-001, FIL-002, FIL-004 and FIL-005 state their keys and rules. The M2 milestone exit criteria drop MSK144 and note that browser recording was delivered in M1b (WAV).
- Spec-inconsistency issues are opened for the rx.v1 names, the queue keys and overflow rule, the files retention, visibility and size keys, the `/api/v1/decodes` vs `/api/v1/decoded-messages` path, and the M2 → M3/M4 cross-dependencies. The product specification is not edited. Where a ticket now diverges from it, the ticket body is authoritative for implementation.
- The runtime image grows by the decoder packages and `csdr-cwskimmer`, with their licence notices.
