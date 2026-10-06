# Handoff: GitHub milestones and tickets

> **Audience:** the agent (or person) who creates the milestones, epics, issues and Project board for **the Product** in its GitHub repository.
> **Inputs (in this folder):**
> - `tickets.yaml`: the machine-readable manifest. It is the single source for every label, milestone, epic, issue and spike.
> - `spec/FEATURE_SPEC.md` and `spec/TECHNICAL_SPEC.md`: product specification v0.3, stack-agnostic.
>
> **Generated:** 2026-10-06, from FEATURE_SPEC v0.3 (437 features).

## 1. What to build on GitHub

| Object | Count | Source in `tickets.yaml` |
|---|---|---|
| Labels | 33 | `labels` |
| Milestones | 8 (M0, M1a, M1b, M2–M6) | `milestones` |
| Project (v2) with custom fields | 1 board, 4 fields | `project_fields` |
| Spike issues | 8 (all in M0) | `spikes` |
| Feature issues | 437 | `issues` |
| Epic issues | 54 (one per Big Feature × milestone) | `epics`, which hold `children` |

**Story points per milestone** (Fibonacci scale, defined in spec §5.3):

| Milestone | Issues | Points |
|---|---|---|
| M0 Foundations (+ 8 spikes, 30 pts) | 77 | 300 |
| M1a Single-node listening | 81 | 270 |
| M1b Multi-node & usability | 50 | 152 |
| M2 Digital modes | 73 | 322 |
| M3 Map & tracking | 48 | 182 |
| M4 Background & reporting | 68 | 270 |
| M5 Hardening & scale | 7 | 76 |
| M6 Low priority | 33 | 110 |
| **Total** | **437** | **1682** |

## 2. Target stack

The specification is stack-agnostic and **must not be edited** to match the stack. The stack is:

- **Go**, as a single binary with the subcommands `hub`, `node` and `all` (spec: CLI roles, GRID).
- **SQLite**, behind a repository + dialect adapter. PostgreSQL is a future adapter (GRID-025).
- **templ + HTMX** for the server-rendered UI, with **JS islands** where HTMX cannot do the job.
- **Caddy embedded as a Go library** as the gateway. The spec allows this explicitly for a Go hub.

The spec leaves the following open for this stack. They become **spikes** in M0 (`type:spike`, P1). Each spike ends with an ADR in `docs/adr/`.

| Spike | Why it exists |
|---|---|
| SPK-01 DSP engine strategy in Go | Go-native DSP vs orchestrating csdr/connector processes vs cgo libcsdr (GPLv3). Drives per-node capacity. |
| SPK-02 SQLite driver + migrations | `modernc.org/sqlite` (pure Go, easy ARM cross-compilation for nodes) vs `mattn/go-sqlite3` (cgo). Migration tool. WAL and backup settings. |
| SPK-03 Embedded Caddy gateway | Dynamic per-node routes, WS proxying, `stream_close_delay`, forward-auth with token injection, reload behaviour |
| SPK-04 Receiver JS island | The htmx `ws` extension only swaps HTML fragments from text messages. The binary `rx.v1` media stream, canvas/WebGL waterfall and AudioWorklet audio need a vanilla JS/TS island, plus a choice of build tooling. |
| SPK-05 Events WS → htmx bridge | The hub events WS uses a JSON envelope (spec). A small dispatcher maps events to `htmx.trigger`, and templ fragments refresh with `hx-trigger="… from:body"` over REST. |
| SPK-06 App shell with templ + HTMX | Layout, design tokens, light/dark/auto, CSP nonces (`templ.WithNonce`), server data for islands (`templ.JSONScript`), CSRF for HTMX requests |
| SPK-07 Go WebSocket library + `rx.v1` framing | Library choice, envelope and 24-byte binary header codec, back-pressure |
| SPK-08 Process supervision | Supervisor for connectors and decoder tools: argv-only spawning, private workdirs, timeouts, back-off, stderr → diagnostics |

**Recommended implementation notes** (non-binding until the spikes conclude):
- `golang.org/x/crypto/argon2` for password hashing.
- Standard CSRF middleware and secure cookies.
- An auth-provider interface ready for OIDC (e.g. `coreos/go-oidc`, later).
- Leaflet as a JS island for the map.

## 3. Conventions

### 3.1 Titles

| Type | Format | Example |
|---|---|---|
| Feature | `[<KEY>] <Feature name>` | `[RX-015] Waterfall` |
| Spike | `[SPK-0n] Spike: <topic>` | |
| Epic | `[Epic] <Big Feature> — <Milestone title>` | |

The `[KEY]` prefix is how the procedure stays **idempotent** (§4.2).

### 3.2 Labels

| Label | Meaning |
|---|---|
| `type:feature`, `type:epic`, `type:spike` | Kind of issue |
| `priority:P1` … `priority:P4` | P1 must (blocks the milestone), P2 should, P3 could, P4 very low (all of M6) |
| `area:<GROUP>` | One per Big Feature (25): GRID, AUTH, ACC, ADM, UI, API, RX, SRC, DEM, BMK, PRS, DEC, DIAG, FIL, REC, MAP, AIR, MAR, SND, LORA, SVC, RPT, INT, DV, BC |
| `needs-split` | 21-point item (currently SRC-028). It must be split before sprint planning. |

### 3.3 Project (v2) fields

| Field | Type | Values |
|---|---|---|
| `Effort` | number | Story points: 1, 2, 3, 5, 8, 13, 21 |
| `Priority` | single select | P1–P4 |
| `Area` | single select | The 25 groups + `SPIKE` |
| `Spec milestone` | single select | M0–M6. M1a and M1b both map to spec **M1**. |

### 3.4 Milestones

Use the native GitHub milestones `M0` … `M6`, with **M1 split into M1a and M1b**. The split exists only in the tickets, not in the spec.

| Milestone | Content |
|---|---|
| **M1a Single-node listening** | Connector SDRs, presets, RX core, analog demodulation, listen policy, device admin |
| **M1b Multi-node & usability** | Multi-node picker and offline behaviour, hub-wide bookmarks + scanner, basic DIAG, presence, shortcuts, mobile bottom sheet, deep links, plus every P3 row of spec M1 |

Milestone descriptions (goal + exit criteria) are in `tickets.yaml`. No due dates are set; leave them empty unless the owner provides dates.

### 3.5 Issue bodies

Bodies are ready to paste. Each one has:
- Summary;
- Entry point / Gate / Milestone / Priority / Effort;
- Rights (4 roles);
- Acceptance criteria (checklist);
- Depends on;
- Spec link.

Two placeholders must be replaced before creating issues:

| Placeholder | Replace with |
|---|---|
| `{{SPEC_BASE}}` | The absolute URL of the spec folder in the product repo, e.g. `https://github.com/<owner>/<repo>/blob/main/docs/spec`. Relative links do not resolve inside issues. |
| Feature keys in **Depends on** (e.g. `DEC-041`) | `#<number>` in the second pass (§4.1, step 7). Keep the key in parentheses: `#123 (DEC-041)`. |

## 4. Procedure

### 4.1 Steps

1. **Ask the owner** for the target `owner/repo` and the Project owner (user or org), unless they are given. Check `gh auth status` (scopes `repo`, `project`).
2. **Copy the specs** to `docs/spec/FEATURE_SPEC.md` and `docs/spec/TECHNICAL_SPEC.md` in the product repo, on a branch, through a PR or a direct commit as the owner prefers. The issue links depend on them.
3. **Labels:** create or update every entry of `labels` (`gh label create … --force`).
4. **Milestones:** create `milestones` in the manifest order (`gh api repos/{owner}/{repo}/milestones -f title=… -f description=…`).
5. **Project:** create the Project (`gh project create`) and the fields in `project_fields` (`gh project field-create`).
6. **Issues:**
   - create the **spikes** first, then the **feature issues**, in milestone order (M0 → M6) and then in key order;
   - set the labels and the milestone;
   - add each issue to the Project and set `Effort`, `Priority`, `Area` and `Spec milestone` (`gh project item-add` and `gh project item-edit`, or GraphQL).
7. **Epics:**
   - create each epic, then attach its `children` as **sub-issues**: GraphQL `addSubIssue`, or REST `POST /repos/{owner}/{repo}/issues/{epic}/sub_issues` with the child's internal `id`;
   - if sub-issues are unavailable, keep the task-list table already present in the epic body.
   - Epics also go into the Project, with `Effort` = `effort_total`.
8. **Second pass for dependencies:**
   - rewrite **Depends on** keys into issue references;
   - add the native "blocked by" relationship where the API supports issue dependencies; otherwise keep the text references;
   - for each spike, add a comment on each issue in `unblocks`: "Blocked by spike #n".
9. **Verify** (§5) and report the counts to the owner.

### 4.2 Robustness rules

- **Idempotent.** Before creating anything, search for an existing issue whose title starts with `[<KEY>]` (`gh issue list --search "[KEY] in:title" --state all`). Update it instead of duplicating it. Keep a `key → issue number` map file, e.g. `.handoff/issue-map.json`, and commit it or attach it to the report.
- **Dry run first.** Print every planned action without calling the API. Then run for real.
- **Rate limits.** Throttle to about 1 write per second and back off on 403/429. Creating 500 issues takes several minutes; resume from the map file if interrupted.
- **Do not edit the spec content.** Report any inconsistency as an issue labelled `type:spike` or as a comment to the owner.

## 5. Verification checklist

- [ ] 33 labels, 8 milestones and 1 Project with 4 fields exist.
- [ ] 437 feature issues + 8 spikes + 54 epics exist. Every title is unique by key.
- [ ] Per-milestone issue counts and the sum of `Effort` match the table in §1.
- [ ] Every feature issue is a sub-issue (or listed child) of exactly one epic, and is in the Project with all four fields set.
- [ ] No `{{SPEC_BASE}}` remains. The spec links open the right section.
- [ ] Every **Depends on** reference points to an existing issue number.
- [ ] `SRC-028` carries `needs-split`.
- [ ] Final report lists the counts, the map file, and anything that failed or was skipped.

## 6. After creation

These are suggestions for the owner, not for the agent:

- Run the M0 spikes first. Their ADRs may refine estimates; re-estimate **only in the Project** (`Effort`), not in the spec.
- Split `SRC-028`, and any issue found larger than 13 points, during M0 refinement.
- Re-export from the spec if it changes. Regenerating `tickets.yaml` keeps the keys stable, and the idempotent procedure updates existing issues.
