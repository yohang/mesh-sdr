# ADR 0020: Presets, schedules, desired state and the reporting engine

- Status: Accepted
- Date: 2026-10-07
- Amended by: ADR 0022 (reporting engine and outbox removed)
- Deciders: project owner
- Scope: epic part `epic/sched-1` of epic #446 (grid). It closes GRID-001 (#9) and GRID-016 (#24) in M0 and completes the schedule parts of ADM-009 (#67) and GRID-015 (#23).

## Context

GRID-001 says the `hub` role starts "schedulers, reporting". GRID-016 says the hub "disables schedules whose device is stale or whose preset no longer fits the device's capabilities". Neither exists yet:

- **Scheduler.** TECHNICAL_SPEC §3.2 has a Scheduler container: it applies `schedules` rows `(device_id, preset_id, time window)` and checks each preset against the device, besides the retention jobs. The retention jobs exist (`internal/jobs`, ADR 0010); schedules do not.
- **Reporting worker.** §3.2 also has a Reporting worker: it drains `reporting_outbox` per network, with retry and back-off (§7.3, §8.6).
- **Where the data comes from.**
  - §7.1 defines `presets`, `schedules` and `reporting_outbox`.
  - §4.4 `ctl.state.apply` pushes the presets, the start preset and the schedule timeline of every device.
  - §8.5 computes a 24 h timeline per device.
  - §2.3 and §7.3 set the outbox retention.
- **Later tickets.** The full features belong to later milestones:
  - presets: SRC-006, ADM-017/018/020 (M1a);
  - schedules and services: ADM-037, SVC-010/011/012/022 (M4);
  - reporting: RPT-001 to RPT-021 (M4).
- **Nothing to run in M0.** No decode producer exists before M2 (DEC-047), and no node device manager before M1 (SVC-001).

The owner asked to finish GRID-001 and GRID-016 in M0 with the minimal subset that makes them true, without pulling whole M1/M4 epics.

Constraints in force:

- light DDD and stdlib tests;
- no new dependency;
- spec-first OpenAPI with `x-meshsdr-access`, plus the contract tests (rights matrix, a validated 2xx for every operation, HTML/API parity);
- accessibility checks on new pages;
- migrations numbered 00030 and up.

## Decision

Numbers refer to the questions of the design proposal; the owner accepted every recommendation.

### Scope (Q1, Q2)

1. **Three modules**, each `domain`, `app`, `infra/sqlite`, `infra/repotest`:
   - `internal/presets`;
   - `internal/schedules`;
   - `internal/reporting`.

   They are wired in `internal/wire/scheduling.go`.
2. **REST only for presets and schedules.** There are no admin CRUD pages.
   - Admin › Devices › {device} gets a read-only Schedules section. It flags a schedule the hub disabled and says why.
   - Admin › Overview counts the schedules the hub disabled.
   - No new HTML form, so the HTML/API parity list is unchanged.

### Domain

- **Presets.**
  - The `Preset` aggregate is built from these value objects: `Slug`, `Name`, `ModeID`, `WaterfallLevels`, `Tags`.
  - A `Spec` is validated from a `Draft`, with one violation per invalid field (`invalid_preset`).
  - Invariant: `start_freq` lies within half the sample rate of `center_freq`.
  - Defaults follow FEATURE_SPEC §9.1 4.6: `start_mod = nfm`, `tuning_step = 1000`, `start_freq = center_freq`. A slug is derived from the name when absent, with a numeric suffix if it is taken.
  - `Fits(DeviceLimits)` checks that `center_freq ± samp_rate/2` lies within `[freq_min, freq_max]` and that `samp_rate` is one of the device's sample rates. A failure is `preset_incompatible`, with the failed check.
  - **Mode check (Q9).** Only the syntax of `start_mod` is checked. M0 nodes report decoder modes only, so a mode-support check would refuse every analog preset. It comes with the mode catalogue.
- **Schedules.**
  - The `Schedule` aggregate holds a `Window` (static UTC minutes, `end` before `start` wrapping over midnight and `start = end` lasting the whole day from `start`, or a daylight phase), `DaysOfWeek` (Monday = bit 0), `Priority`, `enabled` and a `DisabledReason` (`device_stale`, `device_removed`, `preset_incompatible`).
  - **Daylight windows (Q3)** answer 422 `schedule_kind_unsupported`. The columns exist, so SVC-011 needs no migration.
  - **Disabling (Q10).** `Disable(reason)` sets `enabled = false` with the reason and its time. An admin re-enables the schedule with `PUT`, which clears the reason and checks the schedule again like a creation. A schedule kept disabled by an edit keeps its reason. `device_removed` replaces an earlier system reason; a schedule an admin disabled is left alone.
- **Timeline (Q4, Q6).** `Evaluate(schedules, from, until)` is a pure function:
  - windows are UTC;
  - `days_of_week` applies to the day a window starts, so an overnight window belongs to its start day;
  - overlaps are resolved by the highest `priority`, then the earliest start, then the lowest id;
  - the horizon is `[start of the current hour, +25 h)`, so the timeline is stable within an hour and always covers at least 24 h.
- **Outbox.**
  - The `Entry` aggregate follows §7.1. `Failed` retries after `min(30 s × 2^(attempts−1), 1 h)`. An entry is `dead` after 20 attempts or 24 h (§7.3), or with the reason `disabled` or `overflow`.
  - The repository claims entries with a lease in the single writer (`UPDATE … RETURNING`, no `SKIP LOCKED`). A save only applies while the worker still holds the lease.

### Tables (migrations 00030–00033)

| Migration | Content |
|---|---|
| `00030_presets` | §7.1 `presets`; `slug` unique |
| `00031_devices_active_preset_fk` | `devices` rebuilt with `active_preset_id → presets ON DELETE SET NULL` (ADR 0008 Q26, Q13). Ingest stores NULL for a preset id the hub does not know (sub-select in `UpsertDevice`). |
| `00032_schedules` | §7.1 `schedules` plus `disabled_reason` and `disabled_at`. `device_id` has **no FK** (Q11), so a schedule outlives its device and is disabled instead. `preset_id → presets ON DELETE RESTRICT` (Q12). |
| `00033_reporting_outbox` | §7.1 `reporting_outbox`; unique `(network, dedup_key)`; index `(network, status, next_attempt_at)` |

### Desired state and its push (Q5–Q8)

- **Payload.** `ctl.state.apply` (`internal/protocol/rxv1/ctl/state.go`) carries `{revision, presets, devices: {<id>: {presets, active_preset_id, schedule: {from, until, slots}}}, policy: {listen_policy}}`.
  - It carries no device setting and no `reporting` part.
  - The data of each preset is sent once, in the top-level `presets`; each device lists the ids of the presets that fit it.
- **Revision (Q6).** It is a positive 63-bit prefix of the SHA-256 of the message without its revision. Equal content gives an equal revision, so there is no table.
- **Push (`grid/app.States`).**
  - **Welcome.** The state is sent unless `ctl.welcome.last_applied_revision` already equals it.
  - **Changes.** It is sent again only when the revision changed on that channel. Triggers:
    - after every ingested batch and every node answer (`OnLinkChange`), which covers device reports;
    - after every preset or schedule change;
    - after every settings change (`listen_policy`).
  - **Hourly.** The `schedules.publish` job runs every hour (Q7).
  - **Size.** A state larger than the 64 KiB control message limit (§6.9) is not sent and the error is logged.
- **Planner (`schedules/app.Planner`).** For each device of a node it computes:
  - the presets that fit, by sort order;
  - the start preset: the device's active preset if it still fits, otherwise the first one that fits (§7.1);
  - the timeline, only when the node config sets `scheduler_enabled` (Q17: `services.enabled` does not gate schedules) and the device is still reported. A schedule whose preset no longer fits is left out of the timeline.
- **Node (Q8).**
  - `grid/agent.DesiredState` checks every device against the node config: the device exists, its presets fit the frequency range and sample rates, and its timeline is ordered and uses only listed presets.
  - It keeps the state in RAM, announces its revision in `ctl.welcome`, and answers `ctl.state.applied {revision, errors[]}`. That answer is a plain message, not a sequenced event.
  - A refused device keeps its previous state (§4.9). The hub then marks the node `degraded` with the hint `state_apply_failed`.
  - Executing the presets and the timeline belongs to the device manager (SVC-001, SVC-010).

### Disabling schedules (GRID-016, ADM-009, GRID-015)

`schedules/app.Guard` runs inside the transaction of the change that triggers it, and audits every disable (`schedule.disable`, system actor, with before and after):

| Event | Hook | Reason |
|---|---|---|
| A capability report no longer lists the device | `grid/app.Devices` listener, `DevicesStale` | `device_stale` |
| A capability report lists the device: re-check its presets | `DeviceReported` | `preset_incompatible` |
| Admin › Devices › Forget (`DELETE /devices/{id}`) | `DevicesRemoved`, in the forget transaction | `device_removed` |
| Node deleted (its devices cascade) | `Nodes.OnDelete`, in the delete transaction | `device_removed` |
| `PUT /presets/{id}` | `PresetReplaced`; the response lists `disabled_schedules` (Q12) | `preset_incompatible` |
| Safety net for out-of-process changes (CLI): `schedules.publish`, at start then hourly | `Reconcile` | any of the above |

### Reporting engine (Q14, Q15)

- **Engine (`reporting/app.Engine`).**
  - It runs one worker per registered transport, polls every 5 s, and re-reads `Enabled()` on every poll, so settings changes apply without a restart.
  - `Enqueue` is the producer port for decode ingest (DEC-047). It caps the pending entries per network at 50 000; beyond that the oldest become dead with the reason `overflow`.
  - `Status` serves `GET /reporting/status`.
  - The hub runs the engine as a worker (GRID-001). **No transport ships in M0**, so it starts with none.
- **Job `outbox.purge`** (hourly):
  - the waiting entries of a network without an enabled transport become dead (`disabled`) after 1 h (§7.3 rule 4);
  - sent entries are deleted after `retention.reporting_outbox.sent` (7 d);
  - dead entries are deleted after `retention.reporting_outbox.dead` (30 d);
  - deletes go in batches of 10 000.
- **Settings.** Both retention keys are DB settings, lockable, under Admin › Data & retention. The outbox is a store of the retention view.

### REST (`openapi.yaml`) and rights (Q16)

| Operation | Access |
|---|---|
| `GET /presets` (`?device_id=`: only the presets that fit, 422 `unknown_device`), `POST /presets`, `GET`/`PUT`/`DELETE /presets/{id}` | admin |
| `GET /schedules` (`?device_id=`), `GET /schedules/{id}` | listener, filtered: global operators and admins see every schedule, device-scoped operators those of their devices, others none (404 on one) |
| `POST /schedules`, `PUT`/`DELETE /schedules/{id}` | admin |
| `GET /reporting/status` | admin |

- **Optimistic concurrency.** `PUT` carries `version` (409 `version_conflict`).
- **Error codes:**
  - `invalid_preset` and `invalid_schedule` (with field errors);
  - `preset_slug_taken`, `preset_in_use` (409, names the schedules);
  - `preset_incompatible`, `unknown_device`, `unknown_preset`, `device_unavailable`, `schedule_kind_unsupported` (422);
  - `preset_not_found`, `schedule_not_found` (404).
- **Audit.** Every change is audited: `preset.create|update|delete`, `schedule.create|update|delete|disable`.

### Tests

- **Domain.** Value objects, field violations, `Fits`, `Replace`, slugs, the timeline (wrap, clipping, week days, priority, earliest start), back-off and entry lifecycle.
- **Repository suites** for presets and schedules (`repotest`).
- **Application.**
  - preset and schedule services;
  - the guard: stale, re-report, preset change, reconcile;
  - the planner;
  - the engine: dedup, batches, back-off, lease expiry and stale saves, overflow, pending TTL, purge;
  - `States`: revision, welcome, unchanged push, too large, degraded node;
  - the device listener and node delete hooks;
  - the node `DesiredState`.
- **Integration.**
  - `TestDesiredStateEndToEnd`: a real node over mTLS answers each revision, and removing the node disables its schedule.
  - `TestDeviceSchedules`: the device page, the overview, and forget disabling a schedule.
  - The contract suite now covers every new operation.

### Tickets (Q18)

- **Closed:** GRID-001 #9, GRID-016 #24.
- **Commented (closed already):** ADM-009 #67 and GRID-015 #23: forgetting a device or removing a node now disables its schedules.
- **Referenced, still open:**
  - presets: SRC-006 #135, ADM-017 #93, ADM-018 #94, ADM-020 #95;
  - schedules and services: ADM-037 #340, SVC-010 #364, SVC-011 #365, SVC-012 #366, SVC-022 #376;
  - reporting: RPT-001 #377.

## Implementation notes

- **Construction order.** The preset service and the schedule services depend on each other (usage and listener in one direction, the preset catalogue in the other). The composition root fills the catalogue after both exist.
- **Lazy desired state.** The grid's desired-state source is a lazy adapter (`wire.lazyDesired`), because the scheduling modules are wired after the grid.
- **Forget transaction.** `grid/app.Devices.SetListener` registers the guard and the transactor, so a forget and its schedule disables commit together.
- **Hub start.** The `schedules.publish` job runs at hub start like every job, so a schedule whose device went away while the hub was down is disabled at once.

### Review fixes

- **Refusals survive a reconnect.** The node records a desired-state revision only when it accepted every part (devices and `listen_policy`), so its next `ctl.welcome` asks for the state again. The hub keeps the refusals of the last answer across a welcome: the node stays `degraded` until it accepts a state.
- **Pushes per node, on change.** Each node has its own lock. An ingested batch pushes only when a changed capability report marked the node (`MarkChanged`), so heartbeats cost nothing. Settings changes go through one coalescing worker tied to the hub lifecycle.
- **Oversized state.** It is logged at Error once per node and revision, then at Debug.
- **Node checks.** The node checks the preset values like the hub (positive bounded frequencies, sample rate and step, start frequency within the band) and the policy enum, with overflow-safe bounds.
- **Edit after removal.** A schedule that stays disabled on its own device can be edited after the device left the registry.
- **Outbox.** The overflow cap counts the pending entries first (index `network, status, id`). The pending TTL of a disabled network counts from when an entry became due, or from its expired lease for an entry in flight, not from its enqueue time.
- **Rebuild risk.** Migration 00031 documents the cascade risk of rebuilding `devices` once a table references it.

## Spec inconsistencies

Recorded here; the spec is not edited.

1. The schedule travels in `ctl.state.apply` `devices.<id>.schedule` (§4.4) or in a `device.schedule_timeline` message (§8.5) that the §4.4 catalogue lacks, as it lacks `device.preset_applied` and `device.preset_deferred`. `ctl.state.apply` is used.
2. `schedules.device_id` cascades, and forgetting a device deletes its schedules (§7.1), while ADM-009, GRID-016 and ADR 0012 disable and flag them. They are disabled.
3. `schedules.preset_id` is `ON DELETE CASCADE` (§7.1), while `DELETE /presets` answers 409 when schedules reference the preset (§6.10, ADM-020). `RESTRICT` and 409 are used.
4. ADM-037 rejects overlapping windows, while §7.1 resolves them with `priority`. `priority` is used.
5. A preset that is incompatible when its window fires is skipped and audited `schedule.skip` (§7.1), while GRID-016 disables the schedule. The schedule is disabled; the planner also leaves an incompatible schedule out of the timeline.
6. The greyline is ±30 min with `schedule.greyline_minutes` (§8.5) or ±1 h (SVC-011). The position is the node's (INT-002), `receiver.gps` (§8.5) or `receiver_gps` (SVC-011). This is deferred to SVC-011.
7. Compatibility checks the frequency range and sample rates (FEATURE) or also the mode (§7.1), but nodes report only decoder modes. The mode is not checked.
8. Outbox retention:
   - §2.3: delivered rows kept 24 h, failed rows 7 days;
   - §7.1 and §7.3: sent 7 d, dead 30 d;
   - FEATURE: only `retention.reporting_outbox.sent`.

   §7.3 is used. §2.3 calls `failed` what §7.1 calls `dead`.
9. The reporter gate keys are `pskreporter_enabled` and the like (RPT), or `pskreporter.enabled` and the like (§9.1); the `aprs_igate_*` keys differ from the `aprs_is` network name.
10. Preset order is `sort_order` (§7.1) or `position` (ADM-021, §9.1). `waterfall_levels` is `{min, max}` (§7.1) or may be `auto` (§9.1). `sort_order` and `{min, max}` are used.
11. `GET /presets` is "anon LP" (§6.10), while presets are admin data in M0 and the access contract is static. It is admin-only until the receiver picker (RX-006).
12. ADM-037 gates schedules on `services.enabled`, while §7.1 runs them on devices with `scheduler_enabled` only. Only `scheduler_enabled` is used.
13. The day `days_of_week` applies to for an overnight window is not specified. It is the start day. §7.1 does not say what `start_minute = end_minute` means: it is the whole day.
14. GRID-001 still says the hub applies migrations at start and is gated by `hub.listen`. ADR 0006 refuses pending migrations, and ADR 0012 removed `hub.listen`.
15. A full `ctl.state.apply` with every compatible preset per device (§4.4) can exceed the 64 KiB control message limit (§6.9). Preset data is sent once at the top level, and devices list ids.

## Consequences

- GRID-001's "schedulers, reporting" and GRID-016's schedule rule hold in M0.
- The node receives its desired state. Running it waits for SVC-001 and SVC-010.
- Later tickets build on this ADR:
  - presets: SRC-006 and ADM-017+ add the admin pages, the picker and the shared switch;
  - schedules: SVC-011 adds daylight windows, SVC-012 makes the scheduler yield to listeners;
  - reporting: RPT-001+ add the transports, their settings, mode filtering and the admin page.
