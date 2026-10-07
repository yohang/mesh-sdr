# ADR 0026: M1a scope (KISS)

- Status: Accepted
- Date: 2026-10-07
- Deciders: project owner

## Context

M1a ("Single-node listening", milestone #2) held 69 features (242 points) after the backlog trim (ADR 0024). Many tickets still carried settings, overrides and UI extras that a first "listen in the browser" release does not need. The owner wants the smallest product that meets the M1a goal.

## Decision

- **Sources**: M1a supports `rtl_sdr` and `rtl_tcp` only. Basic Soapy (SRC-015) moves to M1b. The milestone exit criterion is amended. Admin › Nodes shows a read-only device list with status, with no per-node device-type listing. A device that is no longer declared is reported `absent`; no "gone" history is kept. The admin Reset button is dropped: the node retries, then marks the device `failed`.
- **Audio**: the codec is ADPCM or PCM, with no Opus. WFM is mono with de-emphasis on the 48 kHz HD path, with no stereo and no RDS (DEM-014 merged into DEM-015). AGC profiles are constants, one per family. Compression and de-emphasis are each one global setting, with no per-device override. Squelch is on/off plus a level, with fixed timing. Auto squelch is a one-shot with a constant margin.
- **NR**: libcsdr `Csdr::NoiseFilter` through the existing cgo shim, controlled by on/off plus a threshold.
- **Presets**:
  - The hub pushes each device's presets in `ctl.state.apply`. The browser sends `preset.select` on the media WS. The node resolves the preset and retunes the device for all its listeners (shared centre).
  - Preset and centre switches write no `audit_log` entry. SRC-017 is merged into SRC-006, and RX-042 into RX-006.
  - The presets admin is HTML pages plus a minimal read-only schedules list. Once those pages work, the presets and schedules REST endpoints are removed (ADR 0023).
- **Listen policy**: unchanged from the spec. `listen_policy = anonymous | registered` is global, and the per-device override `devices.<id>.listen_policy` stays in the node config only (SRC-023, ADM-008). The hub refuses an anonymous token for a `registered` device, and the node rejects one itself. There is no separate Map, Files or Decodes policy.
- **Receiver UI**:
  - Two waterfall palettes (Default, Turbo) and no custom palette.
  - Waterfall levels have one source, a hub setting, plus an auto button. There are no per-device or per-preset levels.
  - The side panel has the Info tab only. Station identity is set on the hub only.
  - There are no `ui_layout_defaults` and no admin default device: the receiver opens the first device the visitor may use.
  - There is no digital-mode select, no bandplan or bookmark ribbon (RX-029 moves to M1b), and no deep link.
  - There are no scanner hooks, no 8.33 kHz triplets, no extra zoom levels, and no per-digit wheel or k/M/G/T hotkeys.
  - Reconnection is a simple capped retry, with no server `backoff` message.

## Consequences

- M1a: 69 → 64 features, 242 → 202 points. M1b: 37 → 39 features, 119 → 127 points.
- The trimmed ticket bodies start with an "M1a KISS (ADR 0026)" note. Merged duplicates are closed as not planned. The epic tables, Effort values and milestone descriptions are updated.
- The product specification is not edited. Where a ticket now diverges from it, the ticket body is authoritative for implementation.
