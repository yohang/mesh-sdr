# ADR 0025: Pragmatic DDD

- Status: Accepted
- Date: 2026-10-07
- Deciders: project owner
- Amends: the "light DDD" rules of AGENTS.md, ADR 0006 (repository contract suites), ADR 0009, ADR 0010, ADR 0016 and ADR 0020 (module layouts)

## Context

Every module followed the same `domain`/`app`/`infra`/`http` split with a `wire.go`, ports on each side and value objects for every string. Small modules paid for it with more packages than code: four copies of `DeviceID` and their converters, six audit ports with six adapters in `internal/wire`, three LRU rate limiters, repository contract suites written for several database engines while there is one (ADR 0022), and wrappers re-checking a pattern the constructor had already checked.

## Decision

1. **Layers only in `identity`, `grid` and `radio`.** `settings`, `shell`, `events`, `jobs`, `files`, `presets` and `schedules` are one package each: model, use cases, SQLite repository, handlers and templates. They take concrete dependencies; an interface is declared only where a consumer needs another implementation.
2. **Value objects only for real invariants** (ranges and units, windows, normalised or secret values, ids). Presets `Slug`, `Name`, `ModeID`, `Tags`, jobs `Name` and settings `Key` become strings checked where they enter.
3. **Shared kernel**: one `DeviceID` in `internal/shared/domain`; `internal/shared/audit` (`Record`, actors `Caller`/`System`/`CLI`, `Appender`) implemented once in `internal/wire` over identity's `audit_log` (the caller, its client address and request id come from the request context; settings still masks secrets); `internal/shared/ratelimit` (token bucket per key, LRU-bounded, IPv4 address or IPv6 /64 keys) for the gateway, identity and the events socket admission.
4. **Tests**: repositories are tested by ordinary package tests against SQLite (`dbtest.NewSQLite`); the `repotest` packages go.
5. **Wiring**: `internal/wire` builds settings and the node device module itself; only `identity` keeps a module `wire.go`.

No functional change: same routes, error codes, rate-limit keys and settings precedence. Audit rows keep their actors and targets; those of presets, schedules and grid actions now also carry the request id.

## Consequences

- 28 fewer directories under `internal/` (105 → 77), about 2 700 lines of non-test Go less (part of them, the contract suites, are now tests).
- Moving a flat module to layers later is a move of files, decided by the owner when its domain grows.
- The media WebSocket keeps its own per-connection bucket (strike counting); the identity throttle and refusal gate stay in `identity/infra/memory` on top of `ratelimit.LRU`.
- `config` imports `internal/settings` (definitions and values) instead of `internal/settings/domain`.
