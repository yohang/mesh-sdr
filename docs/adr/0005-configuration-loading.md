# ADR 0005: Configuration loading

- Status: Accepted
- Date: 2026-10-06

## Context

TECHNICAL_SPEC §7.4 defines the hub and node configuration: TOML v1.0 files with drop-ins, env overrides, locked keys shown with their origin, secret references, unit strings, validation at startup and a published JSON Schema. The stack is fixed by AGENTS.md: `github.com/BurntSushi/toml`, `github.com/caarlos0/env/v11` and `github.com/invopop/jsonschema`. Epic #446 (part "foundation") needs the loader and only the keys used by M0.

`BurntSushi/toml` records the position of every key internally but does not export it: `MetaData` exposes only `Keys()`, `IsDefined`, `Type`, `Undecoded` and `PrimitiveDecode`, and positions are public only on `ParseError`.

## Decision

### Files, merge and precedence

- Config dir: `--config-dir`/`-c`, then `MESHSDR_CONFIG_DIR`, then `/etc/meshsdr`.
- The role reads `<role>.toml` (required), then `<role>.d/*.toml` in lexical order. Each file is decoded into the same Go struct, which gives the merge rule for free: tables merge recursively and scalars and arrays in a later file replace earlier ones.
- Every file must have `schema_version`, and this binary accepts only `1`.
- Unknown keys (`MetaData.Undecoded()`) are errors, with a "did you mean" suggestion (Levenshtein distance).
- Env overrides are applied after the files with caarlos0/env: prefix `MESHSDR_`, one `envPrefix:"<TABLE>__"` per nested table and one explicit `env` tag per key (`db.dsn` gives `MESHSDR_DB__DSN`). A test checks that every env name is the upper-cased key with `.` replaced by `__`.
- Defaults are set in Go code (`DefaultHub`, `DefaultNode`), never with `envDefault` tags, because those would overwrite file values.
- Effective precedence is env > files > defaults.
- A `MESHSDR_*` variable that matches no key of any role is an error. `MESHSDR_CONFIG_DIR` is allowed, and so are the keys of the other role (a hub container may carry `MESHSDR_NODE__ID`).

### Origin tracking

- `config.Origins` maps every dotted key to `default`, `<file>:<line>` (path relative to the config dir) or `env:<VAR>`. A key with an origin other than `default` is locked.
- Lines come from a small in-house TOML position scanner (`internal/config/tomlpos.go`, stdlib only). It runs only on files that already parsed, and understands comments, every string form, table and array-of-table headers, dotted and quoted keys, arrays and inline tables.
- Env origins come from caarlos0/env's `OnSet` callback. The callback also fires for unset variables, so the loader keeps only variables that are present in the environment.

### Secrets

- `config.Secret` accepts `{ file = "…" }`, `{ env = "VAR" }`, or an inline string only when the same file sets `allow_inline_secrets = true`. An inline secret then produces a startup warning.
- Secret files that are group- or world-writable, or world-readable, fail with `insecure_secret_file`. Missing or empty values fail with `secret_unresolved`.
- A `MESHSDR_*` env override of a secret key is a literal value: env is not a world-readable file.
- `String()` and `MarshalText()` redact the value; `Reveal()` returns it.
- No M0 key is secret yet, so the type is covered by tests only.

### Units (§7.4 Format rule 4)

- `Duration`: compound `<int><unit>` with `ms`, `s`, `m`, `h`, `d`, `w` (`"15s"`, `"7d"`, `"1h30m"`).
- `Size`: an integer (bytes) or `<int><unit>` with IEC (`KiB` … `TiB`) or SI (`kB` … `TB`) units.
- `Frequency`: an integer in Hz, or a decimal with `Hz`, `kHz`, `MHz` or `GHz`, parsed exactly. A value that is not a whole number of Hz is rejected.

### Validation

- Validation is Go code mirroring the schema. It runs at load time:
  1. parse,
  2. check `schema_version`,
  3. reject unknown keys,
  4. apply env,
  5. resolve secrets,
  6. run semantic checks.
- Any problem returns a `*config.Error` listing every problem with its origin and a stable code. The process exits with 78 (`EX_CONFIG`).
- No JSON Schema validator library is added.

### JSON Schema and publication

- `config.Schema(role)` reflects the structs with invopop/jsonschema:
  - `FieldNameTag: "toml"`, inlined, draft 2020-12, `additionalProperties: false`.
  - Every key carries `x-scope: "global"` and `lockable: false`, because bootstrap keys are config-only and never DB settings.
  - Secret keys carry `secret: true`.
  - Defaults come from `DefaultHub`/`DefaultNode`.
- Required keys that may come from env (`hub.url`, `node.id`) are not listed as `required`, so a file without them stays valid.
- Publication:
  - `meshsdr hub config schema` and `meshsdr node config schema` print the schema.
  - `meshsdr <role> config check` validates without starting.
  - CI attaches `hub.schema.json` and `node.schema.json` to each `v*` release.

### Keys for M0

| Key | Default | Notes |
|---|---|---|
| `hub.listen` | `0.0.0.0:8073` | An IPv4 literal listens on IPv4 only, IPv6 on IPv6 |
| `hub.url` | required | https unless `hub.allow_insecure_url` |
| `hub.allow_insecure_url` | `false` | |
| `db.dsn` | `sqlite:///var/lib/meshsdr/hub.db` | see ADR 0006 |
| `db.max_read_connections` | `4` | 1..64 |
| `node.id` | required | `grid/domain.NodeID` |
| `node.listen` | `0.0.0.0:8074` | |
| `log.level`, `log.format` | `info`, `json` | `--debug`, `--silent` and `--json` override them |

The sample configs live in `.infra/config/*.toml.example`, the dev configs in `.infra/docker/dev/config/`, and the image ships minimal configs from `.infra/docker/prod/etc/meshsdr/`. A test loads all of them.

## Divergences from the spec and open points

The spec is not edited; these are recorded here instead of in issues.

1. **`tls.*` names differ between the specs.** TECHNICAL_SPEC §7.4 uses `tls.cert`, `tls.key`, `tls.ca_cert` and `tls.ca_key`; FEATURE_SPEC §9.1 uses `tls.cert_file`, `tls.key_file`, `tls.ca_file` and `tls.client_ca_file`. No `tls.*` key is defined yet; GRID-007 decides.
2. **Is `db.dsn` a secret?** FEATURE_SPEC marks it secret, but the TECHNICAL_SPEC example writes it inline. It is a plain string here, because a SQLite DSN holds no credential.
3. **`node.id` pattern.** §4.1 says `^[a-z0-9][a-z0-9-]{1,62}$`; §7.1 "Slugs" says `^[a-z0-9][a-z0-9_-]{0,62}$`. The §4.1 pattern is used.
4. **Hub listen key.** FEATURE_SPEC names it `hub.listen`; the TECHNICAL_SPEC namespace table lists `hub.listen_internal`. `hub.listen` is used, as in AGENTS.md.
5. **DSN form.** FEATURE_SPEC shows `sqlite:///…`; TECHNICAL_SPEC §7.2 shows `sqlite:/…`. Both are accepted.
6. **Secret reference syntax.** FEATURE_SPEC §9.1 describes secrets as `<key>_file` or an env reference; TECHNICAL_SPEC §7.4 uses `{ file = … }` / `{ env = … }`, which is what is implemented.
7. **Config CLI shape.** The spec's `<product> config schema --role …` and `config check --role …` are role subcommands here (`meshsdr hub config schema`), following the AGENTS.md CLI layout.
8. **Schema validation.** §7.4 step 3 is implemented as Go validation equivalent to the generated schema, not with a schema validator.
9. **Enrollment key names conflict.** `hub.enrollment_token` (§4.2), `[hub_trust].enrollment_token` (§7.4 example) and `node.enrollment_token` (FEATURE §9.1) all appear. None is implemented yet; GRID-006 decides.
10. **Unknown env vars.** Treating an unknown `MESHSDR_*` variable as an error is not stated by the spec; it follows the "unknown keys are errors" rule.
11. **Not implemented yet:** `SIGHUP` reload and `config explain`.
