# ADR 0029: M3a scope (KISS)

- Status: Proposed
- Date: 2026-10-09
- Deciders: project owner
- Related: ADR 0016 (events WebSocket), ADR 0023 (API for JS only), ADR 0026 (listen policy), ADR 0028 (M2 scope)

## Context

M3 ("Map & tracking", milestone #5) has 46 open tickets in five epics: Map, Aviation, Marine, Radiosondes and LoRa. Debian trixie packages none of the aviation, sonde or LoRa decoders the spec names, except `readsb` (a dump1090 fork). csdr 0.18.41 already ships NAVTEX and DSC, and direwolf decodes AIS. The code has a `/map` placeholder, the `map` events topic and positions parsed into the decode payloads, but no `map_features` table and no map library. The owner wants the smallest M3 that puts what the nodes decode on a map.

## Decision

- **Split**:
  - **M3a** "Map & tracking" keeps every MAP ticket (P3 included), MAR-001/002/003 and AIR-004/006/007.
  - **M3b** "Aviation, sondes & LoRa" (new milestone) takes AIR-001/002/003/005, SND-001..007, LORA-001..007 and the Radiosondes and LoRa epics.
- **Tools**:
  - ADS-B uses the pinned trixie apt `readsb` in the runtime stage, in place of dump1090-fa.
  - NAVTEX and DSC wrap libcsdr++ through the cgo shim (ADR 0014); AIS is `direwolf -B AIS`.
  - The source-built M3b tools come from pinned tarballs in the `natives` stage.
- **Map library**: vendored Leaflet 1.9.4 (BSD-2) only. Great-circle lines, the day/night terminator and the Maidenhead grid are small in-house ES modules; no Leaflet plugin.
- **Feed**:
  - The map island loads `GET /api/v1/map/features` and `GET /api/v1/map/config` (ADR 0023).
  - Updates then arrive as deltas `map.feature.upsert` and `map.feature.remove` on the existing `map` topic of `/api/ws`, filtered per device by the effective listen policy (ADR 0026).
  - On reconnect the island refetches, as fragments do on `msdr:resync` (ADR 0016). There is no `map.snapshot` and no `since` replay.
- **Projection**: the hub projects features at ingest. `decodes.Ingest` upserts `map_features` from the parsed payload in the same transaction. There is no new control message.
- **Activation**: ADS-B and AIS run while a listener has them on a demod, as in M2. Background services stay in M4.
- **Tiles**:
  - The browser loads keyless layers directly: OpenStreetMap, OpenTopoMap and Esri, plus the OpenSeaMap overlay. CartoDB is dropped: its tiles now require an API key.
  - Tile requests send the hub origin as Referer (`strict-origin`, OSM tile usage policy); every other request keeps `same-origin`.
  - CSP `img-src` is built from the enabled layers.
  - There is no tile proxy, no Stadia, no Google Maps and no OpenWeatherMap or radar overlay.
- **Positions**:
  - New node config `gps`, with an optional `devices.<id>.gps`, reported in the capabilities; fallback is `receiver.gps`.
  - Receivers are shown coarse by default (centre of the 4-character locator); the DB setting `map.precise_receivers` allows precise display (SR-32). The same rule applies to the station position in `/api/v1/status`.
  - readsb gets the device or node `gps` only; there is no `receiver.gps` fallback on nodes.
- **Features**: one row per device (`<kind>:<subject>@<device>`), so tracks, kills, report filters and the call cap never cross devices; the browser merges a station heard by several devices. `GET /api/v1/map/features` returns the newest 5000 the caller may listen to, with a `truncated` flag.
- **AIS input**: a 48 kHz ±12.5 kHz channel from the wide IQ tap with its own FM discriminator; the NFM listener chain destroys 9600 Bd GMSK.
- **Aircraft view**: the map (and its list view) is the aircraft display; the Decoders tab keeps text lines.
- **Retention**:
  - The ticket keys are used: `map.position_retention_s` (default 7200) plus the per-mode TTLs (`aircraft.adsb_ttl_s` for ADS-B), which set `expires_at`.
  - A job every 30 s hard-deletes the expired rows and publishes the removals. There are no tombstones. The `map.retention.<kind>` keys are not used.
- **AIS**: one channel per demod.
- **Spots**: M3a writes `decoded_messages` and `map_features` only; spot fields stay in the payload. The reporters come in M4.
- **Static markers**: MAP-017 reads `map.static_markers_dir` only. EIBi, repeaters and online receiver lists stay with their INT tickets.

## Consequences

- M3a keeps 26 open tickets (Map, Marine and Aviation epics). M3b takes 20: AIR-001/002/003/005, SND-001..007, LORA-001..007 and epics #483 and #484. The Aviation epic stays in M3a with a note on the moved tickets.
- The amended ticket bodies start with an "M3a KISS (ADR 0029)" note: MAP-001, MAP-002, MAP-004 (no Google, REST + WS deltas, keyless tiles), MAP-005 (OpenSeaMap and local grid only), MAP-007 (node `gps`, coarse default), MAP-012 (retention keys, hard delete), MAP-017 (static markers only), AIR-004 (readsb) and MAR-001 (one channel, no reporters).
- Spec-inconsistency issues are opened for the `map.snapshot`/`since` feed, the `map.retention.<kind>` keys, the Meshtastic `mesh_node` kind, the AIR-001/002 dependency on the closed AIR-008, the Google/OpenWeatherMap/RepeaterBook/receiver-list rows and dump1090 vs readsb. The product specification is not edited. Where a ticket now diverges from it, the ticket body is authoritative for implementation.
- The runtime image grows by `readsb` and the vendored Leaflet, with their licence notices.
