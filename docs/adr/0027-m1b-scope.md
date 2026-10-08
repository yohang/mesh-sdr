# ADR 0027: M1b scope (KISS)

- Status: Proposed
- Date: 2026-10-08
- Deciders: project owner
- Amends: ADR 0022 (settings re-added when their feature lands)

## Context

M1b ("Multi-node & usability", milestone #3) holds 39 features (127 points) after ADR 0026. Soapy, decoder probing, a full device picker, MP3 recording and the ReceiverId challenge are more than a "use several nodes comfortably" release needs. The owner wants the smallest M1b that meets its goal, and wants bookmarks and bandplan data to exist without hand-curating them.

## Decision

- **Sources**: Soapy (SRC-015) and the Soapy parts of SRC-009 (per-stage gain) and SRC-022 (gain stages, AGC capability, antennas) move to M5 with SRC-028. M1b supports `rtl_sdr` and `rtl_tcp` only. SRC-009 moves to M5 as a whole: auto and manual `rf_gain` were done in M1a. SRC-022 stays in M1b and reports, per device, the node-config values `rf_gain` (auto|dB), `ppm`, `bias_tee`, `direct_sampling`, `iqswap` and `lfo_offset`.
- **Decoders**: DEC-001 (decoder capability probing) moves to M2. There are no decoders before M2, so the Data decoders epic leaves M1b.
- **Bookmark packs and bandplan**:
  - Both are imported from OpenWebRX+ (luarvique/openwebrx): the `bookmarks.d` general files and the `r1`, `r2`, `r3` region packs, and `bands-r1|r2|r3.json`. OpenWebRX+ is AGPL-3.0, compatible with this project. A NOTICE file ships with the binary and credits the source.
  - Only the region pack is used. The country pack and `receiver.country` are deferred.
  - The data is embedded with `go:embed`. `meshsdr hub migrate` syncs the bookmarks of all three regions into `bookmarks` (origin `builtin`), each row tagged `region:r1|r2|r3`. Queries filter by the `bandplan.region` setting (R1|R2|R3, default R1), so a region change needs no resync.
  - The bandplan ribbon (RX-029) and dial frequencies (RX-030) read the same embedded `bands-r*.json` data.
  - One flat module, `internal/bookmarks`, holds bookmarks and the bandplan (ADR 0025: a new module starts flat).
- **Recording**: the browser writes a WAV from the played PCM, named `REC-<yymmdd-HHMMSS>-<kHz>.wav`. There is no encoder dependency (REC-001).
- **Public status**: API-003 ships `/api/v1/status`, the `/status.json` alias and `receiver.admin_email_public`. The ReceiverId challenge and `listing.receiver_keys` stay with RPT-019 (M4).
- **Device picker** (UI-021): a native `select` grouped by node (`optgroup`). The option text shows state, lock and listener count, and unavailable entries are disabled. There is no search and no band or status filter.
- **Device log** (SRC-005): the node pushes device log records over the control channel; the hub relays them on the `/api/ws` staff topic; Admin › Devices › Log shows them. The hub stores no device log.
- **Deep link**: M1b implements the RX-028 form `/receiver/{nodeId}/{deviceId}?f=&m=&m2=&sql=`. BMK-004 uses a different form (`/receiver/{deviceId}?freq=…&mod=…`); this is a spec inconsistency, reported as an issue, and the spec is not edited.
- **Settings re-added** (amends ADR 0022, which removed settings without a feature): each is added in the PR of the feature that reads it, with its Admin form and a default.
  - `bandplan.region`: R1|R2|R3, default R1 (RX-029, RX-030, BMK-002).
  - `receiver.admin_email`, `receiver.admin_email_public` (API-003).
  - `ui.recorder_enabled` (REC-001).
  - `privacy.mask_ips` (presence, admin connections view).

## Consequences

- M1b: 39 → 38 features, 127 → 122 points. M2: 51 → 52 features, 240 → 248 points (DEC-001). M5: 3 → 5 features, 37 → 45 points (SRC-015, SRC-009). The SRC epic of M1b keeps SRC-005 and SRC-012; the Data decoders epic of M1b is closed as not planned.
- The amended ticket bodies start with an "M1b KISS (ADR 0027)" note. Epic tables, Effort values and milestone descriptions are updated.
- The binary grows by the embedded bookmark and bandplan data and ships a NOTICE for the OpenWebRX+ content.
- The product specification is not edited. Where a ticket now diverges from it, the ticket body is authoritative for implementation.
