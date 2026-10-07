# ADR 0017: Process supervision of external tools (node)

- Status: Accepted
- Date: 2026-10-07
- Deciders: project owner
- Spike: SPK-08 (#8). Unblocks SRC-004 (#134), DIAG-003 (#213), DEC-001 (#210).
- Prototype: `spikes/spk-08-supervisor/` (separate Go module, stdlib only, see its README; on the spike branch only).
- Amends: ADR 0012 (the node image is no longer distroless, see Decision 6).

## Context

A node runs external programs for everything it does not do natively: SDR connectors (`rtl_connector`, `soapy_connector`, …, TECHNICAL_SPEC §8.2) and decoders (`jt9`, `direwolf`, `dump1090`, …, §8.4, §8.7). The spec fixes what the supervision must do:

- **Spawning (§8.4 "Spawning and isolation", SR-51…SR-56):**
  - `execve` with an argv array, never a shell; pipelines are connected by the node;
  - a scrubbed, fixed environment (`PATH` = tool directories, `LANG=C.UTF-8`, `TZ=UTC`, `HOME` = workdir);
  - a private workdir per instance, `<node.runtime_dir>/sessions/<session_id>/` (0700), removed at the end, with a startup sweep; files created with `O_EXCL`/`O_NOFOLLOW`;
  - its own process group, resource limits (memory 512 MiB by default, open files, `nice`, no new privileges), no network where the platform allows.
- **Lifecycle:**
  - devices (§8.2): start timeout 15 s, sample-stall watchdog 3 s, back-off 2/5/15/30/60 s, 10 attempts then `failed`, stop = SIGTERM to the group, 5 s grace, SIGKILL;
  - decoders (§8.4): back-off 1 s doubling to 60 s, crash loop at 5 exits in 5 min (then one retry every 10 min or on mode re-selection), `idle_output` rule with 3 strikes, batch `job` deadline, stop grace 3 s.
- **Classification (§8.4):** ordered stderr regex rules → `info`, `signal_info`, `warn`, `input_error`, `fatal_config`, `resource`, `unknown` (10 lines/s); exit codes 0/126/127/signal; the last 20 stderr lines go with `DECODER_ERROR`. Connectors: 200-line ring, classes `device_lost`, `usb_error`, `overflow` (§8.2 rule 5).
- **Execution rules (§8.1 rule 4):** every wait is asynchronous with a timeout and no lock is held while waiting.
- **Outputs:** `device.state` (§8.2, ADR 0008 events) and diagnostics states `UNAVAILABLE`, `DECODER_ERROR`, `TIMEOUT` (§9.3), carried by the bounded node event buffer (ADR 0008, Q16).

Constraints in force:

- stdlib and `golang.org/x` only;
- `CGO_ENABLED=0` at the time of the spike (SPK-01 has since chosen cgo for the node, see Decision 6);
- Linux nodes;
- light DDD;
- `slog` with an injected component;
- stdlib tests.

ADR 0012 (`epic/grid-3`) ships the node as `meshsdr-node` in the distroless static image (`prod-node` target, `gcr.io/distroless/static-debian13:nonroot`). That image contains no external tool and no C library.

## What the spike built

A generic supervisor (`Supervisor`, `Spec`, `Instance.Run`), a fake connector and a fake decoder (small Go programs built by `TestMain`), and 24 tests, stable with `-race -count=5` (one of them, the network-namespace probe, skips under Docker).

| Area | Prototype behaviour | Covered by |
|---|---|---|
| Spawn | `exec.Cmd` with an absolute `Path`, explicit argv and a **non-nil** `Env` (a nil `Env` inherits everything). The tool runs in its workdir. Arguments with NUL/CR/LF, relative paths, non-clean paths, bad env keys and overrides of reserved keys are rejected. `ResolveTool` looks only in the configured directories. | `TestSpecValidation`, `TestEnvironmentScrubbedAndWorkdirIsCwd` |
| Environment | `PATH`, `LANG`, `TZ`, `HOME=workdir`, `TMPDIR=workdir` (an addition), plus the variables the descriptor declares. A secret in the node environment does not reach the child. | same |
| Workdir | Runtime dir checked (absolute, real directory, owner = euid, no group/other bits). `sessions/<id>` created exclusively, 0700 enforced despite the umask, removed when `Run` returns. `SweepSessions` at node start. `CreateFile` uses `os.Root` with `O_EXCL\|O_NOFOLLOW` and a full-string name pattern. | `TestWorkdirSafety`, `TestPdeathsigKillsToolWhenNodeDies` (sweep after a hard kill) |
| Process group | `Setpgid`. Stop: SIGTERM to `-pgid`, grace, SIGKILL. After every exit, a SIGKILL to the group removes grandchildren. | `TestStopEscalatesToSIGKILL`, `TestProcessGroupKill` |
| Parent death | `Pdeathsig: SIGKILL`. All `cmd.Start` calls run on one goroutine locked to an OS thread that never exits, because the signal is tied to the forking **thread**. | `TestPdeathsigKillsToolWhenNodeDies` |
| Stdio | Stdout and stderr are `os.Pipe` files passed as fds, so `os/exec` runs no copy goroutines and `Wait` returns when the leader exits. Stdout is the data pipe; its pipe buffer gives back-pressure. When the consumer returns, the supervisor keeps draining. After the exit, readers get a bounded time (1 s) to reach EOF, then the read ends are closed, because a process that left the group can hold the pipe. Stdin is fed from `DropOldest`, a bounded buffer that drops the oldest data and counts overruns (§8.3). | `TestStdoutBackPressure`, `TestStreamingDecoderWithStdinFeed`, `TestDropOldestBuffer`, `TestBatchJobCompletes` |
| Stderr | Lines capped at 4 KiB (the rest is dropped and the line is marked truncated). Text is sanitised: invalid UTF-8 replaced, control characters other than `\t` removed. Ordered rules map lines to classes. A 200-line ring keeps them. A token bucket limits `unknown` lines (10/s) and counts what it drops. `OnLine` passes lines to the adapter, for example to parse `signal_info`. `fatal_config` and `input_error` make the supervisor stop the tool and not restart it. `resource` restarts it with back-off. | `TestStderrClassification`, `TestStderrTerminalClasses`, `TestStderrRateLimitAndLineCap` |
| Timeouts | `Start`: readiness is the first stdout byte or `Instance.Touch()`, which a side channel (IQ socket, stats file) calls. `IdleOutput` + `IdleStrikes`: each strike emits `TIMEOUT/no_tool_output`; the last strike restarts the tool. `Job` (batch) kills the group at the deadline: `TIMEOUT/job_timeout`. `Stop` is the grace period. | `TestStartTimeout`, `TestIdleOutputTimeout`, `TestBatchJobDeadline` |
| Restart | Explicit steps (device) or exponential (decoder), ± jitter, `MaxAttempts` → `failed`, `ResetAfter` (a stable run resets the counter), crash-loop window → `crash_loop` + `DECODER_ERROR`, slow retry, and `Kick()` for an immediate retry. | `TestRestartBackoffAndMaxAttempts`, `TestBackoffDelayAndJitter`, `TestCrashLoopDetectionAndKick` |
| Exit classes | ENOENT or 127 → `UNAVAILABLE/tool_missing` + `Reprobe`; EACCES or 126 → `UNAVAILABLE/permission`; a signal the supervisor did not send → `crash`; 0 on a streaming tool → `unexpected_exit`; other codes → `exit_error`. | `TestExitCodeMapping` |
| Limits | Applied between fork and exec by an **exec helper**: the node binary re-executed with a hidden argv[1]. It sets the rlimits (`AS`, `NOFILE`, `CPU`, `CORE`), `setpriority` and `PR_SET_NO_NEW_PRIVS`, then calls `execve` on the tool. A failed exec exits 127 or 126, so it is classified like a direct spawn. | `TestLimitsViaExecHelper`, `TestExitCodeMapping/enoent_via_helper` |
| Observability | `slog` component `node.infra.supervisor` with the attributes `instance`, `kind` and `pid`. Levels: Debug for spawns and stderr lines, Info for running/stopped, Warn for retry and stop escalation, Error for terminal states. Every transition is an `Event` (state, reason, suggested diagnostics state, attempt, delay, exit info with the last stderr lines) sent to a non-blocking `Sink`. `Metrics` hooks: started, exited by class, restarted, stderr lines by class and dropped. | all |

### Measurements and findings

1. **Back-pressure works with no extra code.** With a stalled consumer, the fake connector blocks after 69,632 bytes (the 64 KiB pipe plus one 4 KiB chunk).
2. **Spawn cost.** Spawning a batch job and waiting for its exit takes 1.5 ms direct and 4.0 ms through the exec helper (dev container, i7-1355U). The difference is negligible for streaming tools. For batch jobs it is negligible next to the FT8 deadline of 13 s.
3. **`RLIMIT_AS = 512 MiB` breaks Go tools.** The Go runtime aborts at start (page allocator reservation); it runs under 1 GiB. Address-space limits also count the reserved virtual memory of threaded tools (8 MiB per thread stack) and of anything that maps large regions it never touches. The §8.4 default cannot be applied blindly.
4. **No unprivileged network isolation in Docker.** `CLONE_NEWUSER|CLONE_NEWNET` fails with `EPERM` under Docker's default seccomp profile. "No network where the platform allows" is therefore not available in the default container.
5. **Pdeathsig covers only the direct child.** When the node is SIGKILLed, the connector dies, but a process the connector spawned survives. Two things cover the rest: the group kill (graceful paths) and the deployment (see option C).
6. **Gotchas recorded in the code:**
   - Pdeathsig is per thread: spawns go through a locked thread.
   - A nil `Cmd.Env` inherits the whole environment.
   - With `StdoutPipe`, `Wait` closes the pipe before the reads are done, and a grandchild holding the pipe blocks the copy goroutines. Use `os.Pipe` instead, with a bounded drain.
   - `Mkdir` applies the umask.
   - `kill(-pgid)` after the leader is reaped has a tiny pid-reuse window. A pidfd does not cover a group; only a cgroup removes that window.
   - A tool that calls `setsid` leaves the group. Only a cgroup contains it.
   - In Docker the node is PID 1: orphans are reparented to it and stay zombies unless something reaps them.

## Options

The shape of the supervisor (the table above) is common to all options. The open choices are on five axes.

### A. Where the resource limits are applied

| Option | How | Pros | Cons |
|---|---|---|---|
| **A1** Deployment only | systemd unit (`MemoryMax`, `TasksMax`, `LimitNOFILE`, `NoNewPrivileges`), `docker run --memory --pids-limit --security-opt no-new-privileges` | Nothing to build. Matches SR-56. | Limits apply to the node and all its tools together: a runaway decoder can starve the node. |
| **A2** Exec helper (prototype) | The node re-execs itself as `__exec-helper`, sets the rlimits, `nice` and `no_new_privs`, then calls `execve` on the tool | Per instance, no race, stdlib only. Works in distroless. The same exit-code semantics as a direct spawn. | About 2.5 ms per spawn. A hidden argv mode in the binary. The memory cap is only RLIMIT_AS/DATA (finding 3). |
| **A3** `prlimit(2)` after `Start` | `golang.org/x/sys/unix.Prlimit(pid, …)` | No helper. | The tool runs unconstrained between exec and prlimit, and `no_new_privs` cannot be set this way. |
| **A4** cgroup v2 per instance | One child cgroup per instance with `memory.max`, `pids.max`, `cpu.weight`; `cgroup.kill` kills the whole tree; the cgroup is passed at spawn with `SysProcAttr.UseCgroupFD` | A real RSS-based memory cap, plus atomic kill of the whole tree, including processes that escaped with `setsid`. Covers findings 3 and 5. | Needs a delegated, writable cgroup: systemd `Delegate=yes`, or in Docker `--cgroupns=private` with a writable cgroupfs (not the default). Every deployment variant must be handled, with a fallback. |
| **A5** External sandbox (bwrap, nsjail, `systemd-run --scope`) | argv prefix | Namespaces, seccomp. | Not in distroless. Third-party binaries. Most of them need privileges or user namespaces (finding 4). |

### B. The memory cap

- **B1** RLIMIT_AS at 512 MiB by default (§8.4). It breaks Go tools (finding 3) and threaded tools.
- **B2** RLIMIT_AS only on adapters that opt in (per descriptor `resources.memory`), validated by the contract tests; the default memory cap comes from the deployment (A1).
- **B3** cgroup `memory.max` (A4).
- RLIMIT_DATA (Linux ≥ 4.7) was not measured.

### C. Containment when the node dies hard (SIGKILL, OOM)

- **C1** `Pdeathsig` + process group + startup sweep (prototype). Direct children die; grandchildren may survive.
- **C2** C1, plus the deployment:
  - **systemd:** `KillMode=control-group` (the default) kills the whole service cgroup when the node stops or crashes.
  - **Docker:** the node is PID 1 of its PID namespace, so the kernel kills every process of the container when it dies. Zombies need a reaper, either Docker's `init: true` (docker-init, which works with distroless) or option C3.
- **C3** The node calls `PR_SET_CHILD_SUBREAPER` and reaps orphans itself. It must not reap the pids that `os/exec` waits for, so it needs its own wait loop. That is more code, for a case C2 already covers.
- **C4** cgroup `cgroup.kill` (A4).

### D. External tools and the distroless node image

`prod-node` is `distroless/static`: it has no glibc, no libusb and no tools. Connectors and decoders are dynamically linked C/C++ programs (librtlsdr, SoapySDR and its modules, libusb, FFTW, Qt for some WSJT builds). The SDRplay API and AMBE codecs must not be bundled (§8.7 licensing note 4).

| Option | Description | Trade-offs |
|---|---|---|
| **D1** Keep `prod-node` tool-less | Only devices and modes whose capability probe succeeds are offered (DEC-001), so a stock container node has none. | Simple, minimal attack surface, but a stock container node is useless. |
| **D2** `prod-node-tools` variant | `debian:trixie-slim` (or `distroless/cc` plus copied libraries), nonroot, read-only rootfs, with **pinned** tool versions installed (owrx_connector, SoapySDR + free modules, csdr, direwolf, multimon-ng, rtl_433, dump1090, wsjtx CLI, …). | Turnkey and needed anyway for the §8.4 contract tests (pinned tool images). Costs: a bigger image and CVE surface, and GPL source offer obligations (§8.7 note 3). Proprietary drivers still need a user-built layer. |
| **D3** Distroless node + tools mounted by the user | Tools are bind-mounted or read from a volume under `/opt/meshsdr/tools`, with `tools.*` pointing there. | They must be static, or bring their loader and libraries (`ld-linux` + `LD_LIBRARY_PATH`, which then has to be a declared env var). Fragile, and support-heavy. |
| **D4** Tools image as a data source | A separate pinned "tools" image provides `/opt/meshsdr/tools` (with its libraries) to the node container through a shared volume, or an image mount (Podman, Kubernetes image volumes; Docker support varies by version, not verified). | The tools are versioned apart from the node, and the node image stays distroless. But the libraries must match the tool image's loader, and it is more moving parts. |
| **D5** Sidecar containers running the tools | Connectors already talk loopback TCP (IQ and control), so they could run in a sidecar sharing the network namespace. | The supervisor loses process control: spawn, signals, exit codes, pdeathsig and the workdir. Decoders use stdin/stdout pipes. This breaks the §8.4 contract unless a remote-exec agent is added. Not viable as the general model. |
| **D6** Bare-metal or package install for SDR nodes | A `.deb` with a systemd unit (DynamicUser or a dedicated user, `RuntimeDirectory=meshsdr-node`, `MemoryMax`, `KillMode=control-group`, device access through udev or the `plugdev` group). Docker stays for hubs and tool-less nodes. | The usual setup for SDR hosts (USB passthrough is easier). One more artifact to build and test. |

USB access applies to every container option: `--device /dev/bus/usb/...` plus the device's group for the nonroot user, or udev rules on the host.

### E. Package placement

- **E1** A technical package shared by the device manager and the decoder supervisor, for example `internal/platform/process`. It has no domain, like `internal/db`.
- **E2** `internal/<module>/infra/process` in the first module that needs it (devices, SRC-004), later moved to E1.
- **E3** A `node` module that owns the device manager, the decoder supervisor and this package.

The supervisor's states and policies are technical, not domain. The domain mapping lives in the modules: device lifecycle → `device.state`; decoder session → diagnostics evaluator (§9).

## Recommendation (from the spike, not a decision)

1. **Supervisor:** adopt the prototype's design as the shared base for SRC-004, DIAG-003 and DEC-001 (probes are batch instances with a 5 s job deadline and stdin closed):
   - stdlib `os/exec` with an explicit env and absolute paths;
   - `os.Pipe` stdio with a bounded drain;
   - `Setpgid` + `Pdeathsig` from a locked spawner thread;
   - group SIGTERM → grace → SIGKILL, plus a group SIGKILL after every exit;
   - workdirs under `<runtime_dir>/sessions/` with a startup sweep and `os.Root` for file creation;
   - an ordered-rule stderr classifier with a ring and a rate limit;
   - `DevicePolicy`/`DecoderPolicy` presets with ±10 % jitter;
   - events through a non-blocking sink into the node event buffer (ADR 0008). The supervisor logs each transition once, as the background-worker boundary, and callers do not log the terminal error again.
2. **Limits:**
   - A2 (exec helper) for `nofile`, `nice`, `core=0` and `no_new_privs` on every spawn;
   - B2 (RLIMIT_AS opt-in per adapter) instead of a blanket 512 MiB;
   - A1 for the node-wide memory and task caps;
   - A4 (cgroup v2 per instance) recorded as the target once a delegated cgroup is available, not built in v1.
3. **Containment:** C2. Document `init: true` (or `--init`) for the Docker node, and `KillMode=control-group` for systemd.
4. **Network isolation:** not in v1 (finding 4). Rely on loopback-only binding (§8.2 rule 4) and document the single-tenant host requirement.
5. **Image:** the spike leans towards D2 (a `prod-node-tools` variant with pinned tools, which the §8.4 contract tests need anyway), with D1 kept for tool-less nodes and D6 for SDR hosts. This is the owner's call (questions 1 and 2).
6. **Placement:** E2 now, E1 when the decoder supervisor (DEC-*) needs it.

## Owner questions (answered in Decision)

1. **Tools in containers:** which option (D1–D6), or which combination? If D2, which tools ship in v1, and is the GPL source offer (§8.7 note 3) acceptable for an official image?
2. Is a bare-metal or package node (D6, systemd) a supported v1 deployment, or is Docker the only one? This decides where the limits and containment (A1/C2) are documented and tested.
3. **Memory cap:** accept B2 (no default RLIMIT_AS, opt-in per adapter, node-wide cap from the deployment) instead of the §8.4 default of 512 MiB? Or require A4 (per-instance cgroup) in v1, with the delegation it needs?
4. **Exec helper (A2):** is a hidden argv mode of the node binary acceptable (about 2.5 ms per spawn)? The alternatives are A3 (`x/sys/unix.Prlimit`, racy, no `no_new_privs`) or no per-tool limits.
5. **Jitter:** add ±10 % jitter to every back-off delay (the spec has none)?
6. **Back-off reset:** after how long a stable run resets the attempt counter? The proposal is 60 s for devices and decoders; the spec is silent.
7. **Device retry schedule:** §8.2 (2/5/15/30/60 s, 10 attempts) or SRC-004 (every 15 s, 10 attempts)?
8. **DECODER_ERROR timing:** emit `DECODER_ERROR` on every unexpected exit (DIAG-003) or only on a crash loop (§9.3)? Is the crash-loop threshold the fixed 5 in 5 min (§8.4) or `decoders.max_restarts (db)` (DIAG-003), and what window does that key use?
9. **Fatal stderr classes:** on a `fatal_config` or `input_error` line, should the supervisor stop the tool at once (prototype) or wait for it to exit by itself?
10. **Environment:** may `TMPDIR=<workdir>` be added to the fixed environment, so that tools never fall back to `/tmp` (§8.4 rule 3)?
11. **Tool location:** `tools.<name>` absolute paths (§7.4) vs "`PATH` = configured tool directories" (§8.4). Keep both, with a new key for the directories (for example `tools.dirs`, default `/usr/local/bin:/usr/bin`), and resolve a tool by its `tools.<name>` path, else by name in those directories?
12. **Package placement:** E1, E2 or E3?
13. **Docker PID 1:** require `init: true` in the documented compose files, or make the node a subreaper that reaps orphans (C3)?
14. **Stderr rate limit:** is 10 lines/s for `unknown` lines only (§8.4) enough, or add a global per-instance cap on all classes (for example 200 lines/s) against floods of classified lines?
15. **Network isolation:** drop "no network where the platform allows" for v1 containers (finding 4), or keep it for bare-metal nodes only (D6, where systemd `PrivateNetwork=` or user namespaces may be available)?

## Decision

The owner accepted the spike's recommendation and decided the image question. For the other questions, the spike's recommendation applies; it is written out explicitly below. Numbers in parentheses are the owner questions.

### Supervisor

1. **Shared base.** The prototype design is the base for SRC-004, DIAG-003 and DEC-001:
   - stdlib `os/exec` with an explicit, non-nil env and absolute tool paths;
   - stdio through `os.Pipe` (stdout is the data pipe, with pipe back-pressure), with a bounded drain after exit;
   - `Setpgid` + `Pdeathsig: SIGKILL`, with every spawn on one locked, never-exiting OS thread;
   - stop: SIGTERM to the group, grace (device 5 s, decoder 3 s, per descriptor), SIGKILL; a group SIGKILL after every exit;
   - workdirs `<node.runtime_dir>/sessions/<id>/` (0700, exclusive, removed at the end), a startup sweep, and `os.Root` + `O_EXCL|O_NOFOLLOW` for files;
   - ordered-rule stderr classification, a 200-line ring, 4 KiB line cap, sanitising;
   - start, idle-output (3 strikes), job and stop timeouts;
   - events through a non-blocking sink into the node event buffer (ADR 0008). The supervisor logs each transition once (background-worker boundary); callers do not log its terminal errors again.

   Capability probes (DEC-001) are batch instances: a 5 s job deadline and stdin closed.
2. **Placement (Q12): E2.** The package lives in the first module that needs it (devices, SRC-004), under `infra/`. It moves to a shared technical package when the decoder supervisor needs it.

### Restart and classification

3. **Back-off.**
   - **Device schedule (Q7):** the §8.2 schedule (2/5/15/30/60 s, then `failed` after 10 attempts). SRC-004's "every 15 s" is recorded as a spec inconsistency.
   - **Decoder schedule:** the §8.4 schedule (1 s doubling to 60 s).
   - **Jitter (Q5):** every delay gets ±10 % jitter.
   - **Reset (Q6):** a run that reached readiness and lasted at least 60 s resets the attempt counter (devices and decoders).
4. **Crash loop and DECODER_ERROR (Q8).**
   - Each unexpected exit of a decoder is reported with `DECODER_ERROR` evidence (exit class, code, last 20 stderr lines), as DIAG-003 asks; the session evaluator owns the displayed state.
   - The crash-loop threshold is `decoders.max_restarts (db)` (default 5) within a 5 min window. After it, the session stays `DECODER_ERROR` (`crash_loop`), with one retry every 10 min or an immediate retry when the user re-selects the mode.
   - Spawn failure with ENOENT or 127 follows §8.4: re-probe, then `UNAVAILABLE`.
5. **Fatal stderr classes (Q9).** On a `fatal_config` or `input_error` line, the supervisor stops the tool at once and does not restart it until the configuration changes.
   - **Rate limit (Q14):** only `unknown` lines are rate-limited, at 10 lines/s per instance (§8.4). There is no global per-instance cap in v1; the line cap and the bounded ring bound the memory.

### Isolation and limits

6. **Node image (Q1, Q2).** The node image moves from distroless to **Debian slim** (`debian:trixie-slim`), nonroot, with the tools the implemented adapters need installed at **pinned** versions.
   - There is no separate tools variant.
   - This amends ADR 0012 (`prod-node` was distroless static). It matches SPK-01: the node is built with cgo (libcsdr, libopus) and needs shared libraries at runtime.
   - The image ships GPL programs, so the source offer of §8.7 note 3 applies. Proprietary components (SDRplay API, AMBE) stay out (§8.7 note 4).
   - Docker is the only supported node deployment in v1; a `.deb` package with a systemd unit (D6) comes much later.
7. **Limits (Q3, Q4).**
   - **A2:** the node binary has a hidden exec-helper argv mode, called first in `main`. Every spawn goes through it: `nofile`, `nice`, `core = 0` and `no_new_privs`, then `execve`.
   - **B2:** RLIMIT_AS is applied only when an adapter descriptor opts in (`resources.memory`), validated by its contract tests. The §8.4 blanket default of 512 MiB is not applied.
   - **A1:** node-wide memory and task caps come from the deployment (`docker run --memory --pids-limit`, `no-new-privileges`).
   - **A4:** per-instance cgroups v2 (`memory.max`, `pids.max`, `cgroup.kill`) are the later target. They are not built in v1.
8. **Containment (Q13): C2.** `Pdeathsig` + process group + startup sweep, plus the deployment.
   - In Docker the node runs with `init: true` (`--init`), so orphans are reaped and everything dies with the container.
   - The node does not become a subreaper.
   - `KillMode=control-group` applies when the systemd unit exists.
9. **Environment (Q10).** The fixed environment is `PATH`, `LANG=C.UTF-8`, `TZ=UTC`, `HOME=<workdir>` and `TMPDIR=<workdir>`, plus the variables the descriptor declares.
10. **Tool location (Q11).** A tool is resolved from its `tools.<name>` absolute path when set (§7.4). Otherwise it is resolved by name in a new key, `tools.dirs` (default `["/usr/local/bin", "/usr/bin"]`), which is also the child's `PATH`. The node's own `PATH` is never used.
11. **Network isolation (Q15).** None in v1: unprivileged namespaces are not available in Docker's default profile (finding 4). Connector sockets bind to loopback only (§8.2 rule 4), and the documentation states the single-tenant host requirement.

## Spec inconsistencies

Recorded here; the spec is not edited.

1. **Device retry schedule.** §8.2 retries after 2/5/15/30/60 s; SRC-004 retries every 15 s. Both stop after 10 attempts.
2. **DECODER_ERROR on failures.** §9.3 enters `DECODER_ERROR` on a crash loop, an input format error, a resource error or a parse-error ratio. DIAG-003 enters it on any non-zero exit, signal or spawn failure, then restarts. DIAG-003's `decoders.max_restarts (db)` is not in §8.4, which fixes 5 in 5 min.
3. **Tool location.** §7.4 has `tools.*` absolute paths per tool. §8.4 sets `PATH` to "the configured tool directories", but no key configures them.
4. **Spawn failures.** §8.4 maps spawn failure (ENOENT or 127) to a re-probe, then `UNAVAILABLE`. DIAG-003 maps spawn failure to `DECODER_ERROR`.
5. **Memory cap.** §8.4 rule 5 defaults to an address-space cap of 512 MiB, which Go-based tools cannot run under (finding 3). This is a finding, not a contradiction, but the default needs a decision (question 3).

## Consequences

- One supervisor serves connectors, decoders and probes. Tool-specific code stays in descriptors and parsers (§8.4), and the supervisor never parses tool output beyond line classification.
- Node container deployments must provide a writable private runtime directory for `node.runtime_dir`, for example a tmpfs at `/run/meshsdr-node` owned by the node user with mode 0700. The node refuses to start when the directory check fails (SR-53).
- The node image (Debian slim, cgo, pinned tools) is bigger and has a larger CVE surface than distroless. Tool versions are pinned in the Dockerfile, and the same pins serve the §8.4 contract tests.
- `tools.dirs` is a new `node.toml` key (schema, `.infra/config/node.toml.example`).
- The node binary gains a hidden helper argv mode; `main` must call it before anything else (cobra included).
- Restarts are bounded and jittered. Each restart produces a few events in the bounded event buffer (ADR 0008), where diagnostics are dropped first on overflow.
- Without A4, a tool that leaves its process group (`setsid`) and survives a SIGKILL of its leader is only cleaned up by the deployment (C2). The bounded pipe drain keeps the supervisor from hanging on it.
- Tests use real child processes built by `TestMain`, as in the prototype. They need the Go toolchain in the test environment, which the dev image provides.
- Per-adapter contract tests (§8.4) need pinned tool binaries. Whatever is chosen for D also decides where those tests run.

## References

- TECHNICAL_SPEC §7.4 (`node.runtime_dir`, `tools.*`), §8.1 (rule 4), §8.2 (driver strategy, lifecycle), §8.3 (buffers), §8.4 (adapter contract), §8.7 (dependencies, licensing), §9.3 (states), §10.7 (SR-51…SR-56).
- FEATURE_SPEC rows SRC-004, DIAG-003, DEC-001.
- ADR 0008 (control channel, event buffer and drop priority), ADR 0012 (`prod-node` distroless image, on `epic/grid-3`).
- Prototype: `spikes/spk-08-supervisor/` (README, tests, `BenchmarkSpawn`).
- Linux: `prctl(2)` (`PR_SET_PDEATHSIG`, `PR_SET_NO_NEW_PRIVS`, `PR_SET_CHILD_SUBREAPER`), `setrlimit(2)`, `cgroups(7)` (`cgroup.kill`, `memory.max`), `pipe(7)` (capacity). Go: `syscall.SysProcAttr` (`Setpgid`, `Pdeathsig`, `UseCgroupFD`), `os.Root`.
