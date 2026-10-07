# ADR 0024: Backlog scope trim for a hobbyist product

- Status: Accepted
- Date: 2026-10-07
- Deciders: project owner

## Context

After M0, the backlog (generated from FEATURE_SPEC v0.3) held 363 open features (1394 points). About 70 targeted professional or enterprise needs, many issues duplicated each other, and the path to the first "listen in the browser" experience in M1a was blocked by M1b/M2 dependencies and dependency cycles. MeshSDR is for radio hobbyists; the owner wants fast cycles and simplicity.

## Decision

The backlog on GitHub was trimmed in one pass:

- **Closed as not planned (46 issues)**: the diagnostics state machine and its extras (DIAG-001, 005, 006–013), speech/server recording/MP3/satellite decoding, exports and Prometheus metrics, the generic reporting engine (RPT-001), event reporting and MQTT subscribe, Postgres/HA/drain/load tests, Google Maps, CPDLC, receiver-list scraping, per-role session timeouts, listen-time limits, the scheduled backup engine (a documentation section instead), and the "Add device" page.
- **Merged (58 duplicates into 49 keepers)**: each keeper has a "Merged from" section; settings-only ADM issues were folded into their features.
- **M1a critical path**: the receiver works single-node first (RX-040 now depends on RX-002); status chips and diagnostics left M1a; every dependency cycle was broken (node/domain half first, UI half second); no M1a issue depends on a later milestone.
- **Moved to M1a**: broadcast FM (DEM-015, DEM-016) with the 48 kHz audio path (DEM-014). UI-012 and RX-035 moved to M1b.
- **Issue bodies**: boilerplate acceptance criteria, mandatory `audit_log` wording, API twins, `(cfg|db)`/"lockable" and diagnostics wording removed where no longer required (ADRs 0022, 0023).
- **Project and epics**: closed issues archived, stale statuses fixed, 11 empty epics closed, epic tables and Effort recomputed, milestone descriptions updated.

Kept on purpose: all hobbyist decoders (CW, FT8/FT4, WSPR, APRS, …), the reporters (PSKReporter, WSPRnet, APRS-IS, SondeHub; each brings the minimal delivery code it needs), LoRa/Meshtastic, bookmarks and map basics.

## Consequences

- Open features: 363 → 259; points: 1394 → 1040 (M1a 69/242, M1b 37/119, M2 51/240, M3 41/158, M4 28/140, M5 3/37, M6 30/104).
- Minimal "listen" slice of M1a: 24 issues, 110 points (sources, presets, NFM/AM/SSB/WFM, audio, receiver page, waterfall).
- The product specification is not edited; where issues now diverge from it, the issue body is authoritative for implementation.
