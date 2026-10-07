# ADR 0019: Node runtime (devices, DSP, media streaming, images)

- **Status:** Accepted
- **Date:** 2026-10-07
- **Deciders:** project owner
- **Scope:** GRID-002 (#10, CLI role `node`), finished in M0. References without closing: DEM-001 (#154), DEM-010 (#163), SRC-004 (#134), SRC-016 (#144), SRC-025 (#153), RX-002 (#101), RX-015 (#114).
- **Builds on:** ADR 0004 (rx.v1, send queue, `TCP_NOTSENT_LOWAT`), ADR 0008 (control channel, event buffer), ADR 0012 (node media WS, `prod-node`), ADR 0014 (cgo libcsdr++, gonum, channelizer, goroutine per chain), ADR 0015 (receiver island), ADR 0017 (process supervision).
- **Amends:** ADR 0001 and ADR 0012 (binaries are built with `CGO_ENABLED=1`; every image is Debian slim).

## Context

GRID-002: "`meshsdr node` starts the device sources and DSP, and exposes the authenticated node API and WS on `node.listen`. It holds no DB connection and writes nothing to disk except temporary DSP scratch files under `paths.tmp_dir`." The node API, the control channel and the authenticated media WS existed (ADR 0008, ADR 0012); devices, connectors and DSP did not. The design proposal was reviewed by the owner; the decisions below record his answers.

## Decision

### Scope

1. **DSP (Q1 b).** Device front-end, shared spectrum, the full §6.8 send queue, and one NFM chain on the gonum channelizer (ADR 0014 d4) with libcsdr++ FM demodulator, de-emphasis and AGC, PCM and IMA ADPCM audio at 12 kHz (all `output_rate` values work). ADPCM is written in Go in the rx.v1 layout. DEM-001 and DEM-010 are referenced, not closed: AGC profiles from settings, NR, Opus and the other modes remain.
2. **Sources (Q2).** `rtl_sdr` and `rtl_tcp` through owrx_connector 0.6.5 (`rtl_connector`, `rtl_tcp_connector`). No Soapy. Other device types are reported `unavailable` (`driver_unsupported`).
3. **No synthetic device type (Q3).** Tests and the dev stack use a fake owrx connector (`internal/radio/infra/connector/fakeconnector`, same argv and sockets) that synthesises an NFM carrier and a steady carrier. It is not shipped in the images.
4. **Tuning without presets (Q4).** A device tunes to its first sample rate with the capture band at the bottom of `freq_range` (centred when the range is narrower). `device.retune` is implemented: live over the connector control socket, for operators of devices with `operator_can_retune` and for admins (the hub grants the `retune` scope); the next start uses the new centre. SRC-016 is referenced.
5. **Demand (Q5, §8.2).** `always_on` devices start with the node; each media `device.attach` adds USER demand; the device stops 10 s after the last detach. BACKGROUND demand comes with schedules.

### Code

6. **Module `internal/radio` (Q6).** `domain` (devices, tuning, driver settings, the §8.2 state machine), `app` (device manager, ports), `infra/process` (the ADR 0017 supervisor, placement E2), `infra/connector` (owrx connectors, port pool, tool resolution), `infra/engine` (DSP runtime), `http` (media stream handler), `wire.go`.
7. **libcsdr++ shim (Q7 a).** `internal/dsp/csdr` is the only cgo package (CI and a test check it). Each stage owns a linear carry buffer as its `Reader` and writes into the caller's buffer through a span `Writer`; `canProcess()`/`process()` run synchronously in the calling goroutine. No csdr thread or ring is used; C++ exceptions never cross the boundary; FFTW planning is serialised by a mutex. csdr fixed-length modules only process when strictly more than one block is available, which would delay every block by one: `FftExchangeSides` is done in Go.
8. **Waterfall FFT (Q8).** csdr `Fft` + `LogAveragePower` (FFTW, SIMD), averaging as OpenWebRX (`fft.size` 4096, `fft.fps` 9, `fft.voverlap_factor` 0.3), dBFS (a full-scale tone reads 0 dB), encoded once per line as FFT u8 dB with the default scale. gonum serves the channelizer.
9. **Channelizer.** Overlap-save fast convolution per device: forward FFT of N = V + L samples (V = 3 kHz-transition filter length, rounded to a multiple of D; L = 3V), channels at fs/D ≥ 24 kHz take M = N/D bins, multiply by their band-pass and run an M-point inverse FFT; the residual below one bin is a csdr shift in the chain. It runs in one goroutine per device and extracts every channel; each demodulator runs its chain in its own goroutine (ADR 0014 decision 5). Offset changes keep the chain (no AGC reset).
10. **Rings.** Every edge is a bounded ring (250 ms) with one cursor per reader; a slow reader loses its oldest blocks and gets a gap marker; audio after a gap carries `discontinuity`.
11. **Send queue (Q15).** `internal/protocol/rxv1/sendq`, pure, injected clock; `wsconn.Options.Queue` makes the media connection write through it (JSON → audio → meters → FFT; frames numbered per stream; header written apart from the shared payload).
12. **Media WS.** The grid media endpoint keeps authentication and scope checks and hands device messages to `media.Streams` (interfaces in `internal/protocol/rxv1/media`). It answers `time.sync`. `preset.select` stays `unsupported_type` (presets epic). `device.config` carries the FEATURE_SPEC defaults (`waterfall.levels` −88/−20, `turbo`, `auto_min_range` 50, squelch auto margin 10, `fft.*`) until the hub pushes settings.

### Configuration (Q13, Q14, Q16)

| Key | Default |
|---|---|
| `node.runtime_dir` | `/run/meshsdr-node` (checked 0700, owner = node user; the node refuses to start otherwise; workdirs swept at start) |
| `node.ipc_port_range` | `40000-40999` |
| `node.ws_notsent_lowat` | `16KiB` (set on every accepted node socket with `x/sys/unix`) |
| `tools.dirs` | `["/usr/local/bin", "/usr/bin"]` |
| `tools.rtl_connector`, `tools.rtl_tcp_connector` | resolved in `tools.dirs` |
| `devices.<id>.driver` | `{device, ppm, rf_gain = "auto" \| dB, iqswap}`; rtl_tcp needs `host:port` |
| `devices.<id>.auto_recover` | `true` (restart a failed device after 15 min) |
| `node.max_demods` | `32` (interim; demodulators of the whole node) |
| `devices.<id>.max_demods` | `16` (interim; `0` means the default) |

The hidden exec-helper argv mode is `__exec-helper`, run first in `main`.

"Temporary DSP scratch under `paths.tmp_dir`" means the per-tool workdirs under `node.runtime_dir/sessions/`: the DSP itself is RAM-only. A test checks the connector workdir; the node writes nothing else except the certificate renewal of ADR 0008.

### Build, images, CI (Q9–Q12, Q17, Q18)

13. **Natives stage.** csdr 0.18.41 (commit f26b520) and owrx_connector 0.6.5 (commit 8702852) are built from GitHub tag tarballs pinned by sha256. The Go stages gain g++, libfftw3-dev, libsamplerate0-dev and librtlsdr0 at pinned Debian versions, the natives, and `CGO_ENABLED=1`.
14. **Runtime images.** `prod` and `prod-node` are `debian:trixie-slim` pinned by digest, apt packages pinned, user `nonroot` with uid/gid 65532 (the distroless ids, so existing volumes keep their owner), `/run/meshsdr-node` created 0700. Compose runs the app with `init: true`. No hub-only distroless build.
15. **arm64.** `docker buildx` with QEMU builds `linux/amd64,linux/arm64` on main and tags; pull requests build amd64 only.
16. **Licences and sources.** Images carry `/usr/share/doc/meshsdr/THIRD_PARTY_NOTICES`, the AGPL text, the csdr and owrx_connector licence texts and the licence texts of every linked Go module (`go-modules.txt`, generated at build). The `sources` target, published as the `-sources` tag of every release, holds the exact csdr and owrx_connector tarballs and the Debian source packages of FFTW, rtl-sdr, libusb and libsamplerate.
17. **AGPL self-source.** The repository becomes public later; until then the About page link stays the source offer of MeshSDR itself.
18. **Dev stack.** The dev node has an `rtl_sdr` device on the fake connector, built by Air into `tmp/`.
19. **CI.** Both builds are linted and tested with cgo; `-race` runs on `internal/dsp`, `internal/radio` (channelizer included) and `sendq`.

### Hardening (security review)

- The exec helper locks its OS thread first: `PR_SET_NO_NEW_PRIVS` and `setpriority` are per thread, and it reads `PR_GET_NO_NEW_PRIVS` back before `execve`. The final group SIGKILL after an exit is sent only while the group has members.
- The media WS enforces the advertised `msg_rate` (20 msg/s, burst 50): `rate_limited` with `retry_after_ms`, close 4429 after 10 refusals in a minute.
- `auth.refresh` reauthorises the stream handler: devices the new token no longer allows to listen to are detached; demodulators it no longer allows (scope or `lim.max_demods`) are removed; `demod.set` checks the `demod` scope.
- `node.max_demods` and `devices.<id>.max_demods` bound the demodulators whatever the tokens grant (`capacity_exceeded`). The defaults are interim; the owner tunes them.
- Demodulator channels (filter design, rings) are built outside the engine lock, so ingestion never waits on them; demodulators created before a device runs are validated against its tuning.
- Supervisor instance ids of long device ids (up to 63 characters) are shortened with a hash of the id.
- Each audio frame carries the codec of the framer that produced it; `audio.configure` refuses unknown codecs, answers `opus` with `adpcm-ima` explicitly, and applies to every demodulator or none.
- **Loopback ports.** `node.ipc_port_range` ports are probed free on 127.0.0.1 before a run, but another local process may bind one between the probe and the connector (TOCTOU): the connector then fails and the next attempt takes new ports. The connector sockets are unauthenticated: any process of the same host, notably of the same user, can read the IQ or send control lines. The node therefore assumes a single-tenant host (ADR 0017 decision 11).
- **USB devices in containers.** Pass the bus (`devices: ["/dev/bus/usb:/dev/bus/usb"]`, or one device node) and give the nonroot user its group (`group_add: ["<gid of /dev/bus/usb/*>"]`, often `plugdev` or the host's USB group); the host must not load the DVB kernel driver of RTL-SDR dongles (`blacklist dvb_usb_rtl28xxu`). rtl_tcp devices need no USB access.
- **Pins.** The Go and Debian base images are pinned by digest, the shipped Debian packages (libusb included) by version, used by both the runtime and the `sources` stages. csdr and owrx_connector publish no release tarballs: the GitHub tag archives are pinned by sha256. The `-sources` image also holds MeshSDR's own source, and `THIRD_PARTY_NOTICES` carries a written offer naming the repository and tag.

### Interim values (until settings reach the node)

AGC (libcsdr `Agc<float>`, reference 0.8, max gain 10): slow profile attack 0.05, decay 0.0001, hang 200 ms (used for NFM, `dsp.agc_profile.nfm` default `Slow`); fast profile attack 0.1, decay 0.001, hang 20 ms. Default NFM pass band ±4 kHz. Maximum 1 demodulator when the token carries no `lim.max_demods`.

## Spec inconsistencies

Recorded here; the spec is not edited.

1. **`paths.tmp_dir`** (FEATURE_SPEC GRID-002, settings table) vs **`node.runtime_dir`** (§7.4, §8.4, ADR 0017). Only `node.runtime_dir` exists.
2. **No initial tuning** without presets: §7.4 has no device key for it (decision 4).
3. **`fft.*`, `waterfall.*`, `dsp.*` are hub DB settings**, but nothing delivers them to the node yet; the node uses the FEATURE_SPEC defaults.
4. **RX-015** "float32 or ADPCM per `fft_compression`" vs §6.7 (u8 dB, no ADPCM FFT) — already in ADR 0014.
5. **§8.3 fixed worker pool** vs one goroutine per chain — already in ADR 0014.
6. **SRC-004 retries every 15 s** vs §8.2 2/5/15/30/60 s — already in ADR 0017; §8.2 is implemented.
7. **`time.sync`** is listed with an ack result `time.sync.reply` (§6.4) and as a node → client message (§6.5); the node sends a `time.sync.reply` message carrying the request id.
8. **`device.state`** to media clients (§6.5) has no `listeners`/`center` while the control-channel event (§4.4) has them; the node sends each its own shape.

## Consequences

- One binary per role still, now cgo: cross-compilation goes through QEMU; the dev image is bigger (C/C++ toolchain).
- Capacity is unmeasured on reference hardware (ADR 0014 decision 3).
- Admin reset of a failed device (SRC-004), presets (SRC-006), the receiver UI (RX-002, RX-015), Opus, other modes and drivers come with their tickets.
