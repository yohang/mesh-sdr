# ADR 0014: DSP engine strategy

- **Status:** Accepted
- **Date:** 2026-10-06 (spike), decided 2026-10-07
- **Deciders:** project owner
- **Spike:** SPK-01 (#1). Unblocks RX-015 (#114, waterfall), DEM-001 (#154, NFM), DEM-010 (#163, audio output path), SRC-016 (#144, live retune).

## Context

A node does all real-time signal work (TECHNICAL_SPEC §8). The DSP engine contract (§8.3) requires:

- one device front-end per running device: CF32 IQ from the connector, a `sample_index` and a UTC anchor, written into a device ring;
- one shared spectrum per device: FFT, log power and averaging computed **once**, encoded once per codec and fanned out to every subscriber;
- a channelizer, shared per device where possible. A polyphase channelizer MAY replace per-session shifts above `dsp.channelizer_threshold` (default 4) sessions;
- one per-listener chain per media session: selector (shift, decimation, band-pass, squelch, power meter) → demodulator → audio chain (NR, AGC, resampling to the client rate, codec) → media WS;
- a **fixed worker pool** (dataflow scheduler), with no thread per block or per module, and blocking I/O kept off the DSP workers;
- bounded rings on every edge, with no silent overflow: drops are counted and leave gap markers;
- chains built off the hot path and swapped atomically on a mode change;
- "The node MUST implement DSP natively (SIMD where available). Whether it uses libcsdr++ through FFI or implements the blocks natively is an implementation choice, subject to the licensing note" (§8.3, §8.7).

The NFRs (§2.3):

- per-listener DSP ≤ 25 ms p95, total audio ≤ 250 ms p95, tuning response ≤ 150 ms + RTT, FFT frame age ≤ 200 ms;
- node-internal audio ≤ 100 ms p95 (§8.3);
- reference hardware A (4-core ARM64, Raspberry Pi 5 class, one device ≤ 2.4 MS/s) MUST sustain 20 listeners (NFM/AM/SSB, Opus, FFT at 10 fps) at ≤ 80 % CPU;
- reference hardware B (8-core x86-64, ≤ 10 MS/s) SHOULD sustain 100 listeners;
- one FFT per device, shared by all its listeners.

Project constraints:

- **Licence.** The project is AGPL-3.0-or-later (owner statement; FEATURE_SPEC asks for a source link as an AGPL obligation). There is no `LICENSE` file in the repository yet.
- **Build.** The binary is built with `CGO_ENABLED=0` and shipped in `gcr.io/distroless/static-debian13:nonroot`, which has no C runtime (ADR 0001). Nodes run on ARM boards, so cross-compilation must stay simple (ADR 0001). ADR 0012 adds a node-only artifact (`-tags nogateway`, Dockerfile target `prod-node`).
- **Dependencies.** Any third-party Go dependency other than the stdlib and `golang.org/x/*` needs the owner's approval (AGENTS.md).
- **Media WS.** The node media WS speaks `rx.v1` (ADR 0004). It uses a 24-byte header with a `timestamp_us` of the first IQ sample of each frame, FFT codecs u8 dB (`0x10`) and f32 (`0x11`), and audio codecs PCM, IMA ADPCM and Opus. Its send queue is our own code: producers never block, FFT is latest-wins, audio drops the oldest frames. Kernel socket buffers are capped with `TCP_NOTSENT_LOWAT`.
- **Process supervision.** Connectors and decoders are external processes behind the adapter contract (§8.2, §8.4). Supervising them is SPK-08 (#8).

**Owner decision for this spike:** no benchmark on reference hardware (no Raspberry Pi). The issue asked for "listeners per node and CPU per mode family for each option". Those numbers are **not** produced here. The prototype ran only small functional tests and micro-benchmarks on an x86 laptop. They are labelled indicative and only compare options with each other.

### What the spike did

- It read the spec, ADRs 0001, 0002, 0004, 0008 and 0012, the tickets, and the archived OpenWebRX+ audit (`docs/archive/openwebrxplus-technical-audit.md` §6.10.2–6.10.3, which covers the licences and threading models of csdr and owrx_connector).
- It built a throwaway Go-native prototype in `spikes/spk-01-dsp/`, a separate module (see its README):
  - the chain: IQ → shift → multi-stage FIR decimation → NFM / AM / USB / LSB demodulation → audio decimation → PCM s16;
  - the shared spectrum: window → radix-2 FFT → dBFS → averaging → u8;
  - a fixed worker pool fanning one device block out to N chains;
  - tests with synthetic IQ, and indicative x86 benchmarks, including four Go FFT implementations.
- It checked that the prototype cross-compiles to `GOARCH=arm64` with `CGO_ENABLED=0`. It did not run on ARM.

## Options

### (a) Go-native DSP

Every block is written in Go (stdlib, plus at most one FFT dependency), compiled with `CGO_ENABLED=0`, and runs on goroutines scheduled by our own worker pool. Connectors and decoders stay processes, as the spec already requires.

FFT candidates:

| Candidate | Licence | Maintenance | Types / sizes | Notes from the prototype |
|---|---|---|---|---|
| Hand-written radix-2 (prototype, ~60 lines) | ours | ours | complex64, powers of two, in place | 0 allocations. Fastest or on par with the fastest on this machine. No SIMD. |
| `gonum.org/v1/gonum/dsp/fourier` v0.17.0 | BSD-3-Clause | Active (release 2025-12); large, well-known project | complex128 only; any length (FFTPACK port) | 0 allocations with a `dst` buffer. Slowest of the four here (complex128 plus conversions). Brings the whole gonum module. |
| `github.com/mjibson/go-dsp` → `github.com/madelynnblue/go-dsp` v1.0.0 | ISC | Moved (the old path no longer resolves: its go.mod declares the new path); one maintainer | complex128; radix-2 plus Bluestein | Allocates on every call (115–139 allocations, 166–658 KB). Uses its own internal worker goroutines. |
| `github.com/scientificgo/fft` v0.0.2 | BSD-3-Clause (pkg.go.dev) | Pre-v1 (2025-03), small | complex128; radix 2–7, Stockham, Bluestein | Fast, but allocates the output on every call. |
| `github.com/argusdusty/gofft` v1.2.1 | MIT | Last tagged release 2019 | complex128, powers of two, in place | Not benchmarked. |

The spectrum costs little (see "Indicative measurements"), so FFT speed is not the deciding factor. What matters is allocations, complex64 support and dependency weight.

### (b) Orchestrating external csdr / owrx_connector / nmux processes

Each listener chain is a pipeline of processes. One option uses `csdr` CLI blocks connected by pipes. Another runs a whole chain in a C++ helper. The Go node only spawns, connects and supervises them.

- **owrx_connector** (GPLv3+) and the other connectors are **already processes by spec** (§8.2), whatever this ADR decides. The node implements the client side: spawn, wait for samples, `key:value` control. Live retune (SRC-016) goes through that control socket. It does not depend on the DSP strategy.
- **nmux** (BSD-3) is only needed for pipeline sources (Perseus, FiFi). §8.7 allows replacing it with the node's own fan-out. In Go that fan-out is a ring with per-reader cursors, which the device front-end needs anyway.
- For the DSP itself, OpenWebRX+ no longer uses the `csdr` CLI. It links libcsdr++ through pycsdr (audit §6.10.3). A pipeline of CLI blocks would give 5–8 processes per listener, so 100–160 processes for 20 listeners on a Pi.
- **Spec fit:** §8.3 says "MUST implement DSP natively" and forbids "a thread per block or per module". A process per block goes further than a thread per block, so option (b) for DSP blocks would need a spec change, and therefore a separate spec issue.
- **Media WS fit:** pipes add hidden buffering. A default Linux pipe holds 64 KiB, about 0.7 s of 48 kHz s16 audio. This is the same class of problem as the kernel socket buffers measured by SPK-07 (ADR 0004 finding 2). Per-block drop and gap accounting, `sample_index` tracking (needed for `timestamp_us` and for decoder slot timing) and atomic chain swaps on mode changes all cross process boundaries and become heuristic.
- **Packaging:**
  - the node image needs `csdr`, `libcsdr++`, `libfftw3f`, `libsamplerate` and `libstdc++` built and pinned for amd64 and arm64;
  - a distroless static image is no longer possible. The image becomes `distroless/cc` or Debian slim, plus the shared libraries;
  - GPL sources must be shipped or offered (§8.7 licensing note 3).
- **Supervision:** the SPK-08 supervisor (argv-only spawning, process groups, back-off) could run these processes. But a crashed DSP block silences a listener, which is a different failure class from a decoder crash, and §8.4's diagnostics were not designed for it.

### (c) cgo bindings to libcsdr++

The Go node links libcsdr++ (luarvique/csdr 0.18.x, C++11) through cgo.

- **API:** libcsdr++ exposes C++ templates (`Csdr::Module<T,U>`, `Ringbuffer<T>`, `AsyncRunner`). cgo calls C, not C++, so we would write and maintain an `extern "C"` shim in C++, compiled with g++. pycsdr shows the size of that work.
- **Threading:** libcsdr's own framework runs **one `std::thread` per module** (AsyncRunner) over mirrored-mmap rings that never block the writer and let slow readers be overrun silently. That conflicts with §8.3 (fixed pool, no thread per block, no silent overflow). Only the leaf `process()` kernels could be called synchronously from our Go worker pool. The scheduling, rings and gap markers would still be ours, as in option (a).
- **Build and images:**
  - this breaks `CGO_ENABLED=0` (ADR 0001) for every binary that contains the node: `node` and `all`. The hub could stay pure Go behind a build tag;
  - cross-compiling to arm64 then needs a C/C++ cross toolchain and arm64 builds of fftw3f, libsamplerate and libcsdr, or QEMU builds or native arm64 CI runners;
  - the image needs `distroless/cc` plus the shared libraries, or a fully static C++ link, which is fragile with glibc;
  - the race detector and fuzzing work with cgo, but CI needs the C libraries.
- **Cost per call:** a cgo call costs tens of nanoseconds. That is negligible per block of thousands of samples, so performance is not the argument against it.
- **SIMD:** libcsdr++ uses function multi-versioning (ifunc) and NEON flags. That is the one real advantage over pure Go, which has no SIMD on arm64 without assembly.
- **Wire compatibility:** csdr's `AdpcmEncoder` inserts `SYNC` words every 1000 samples. rx.v1 IMA ADPCM frames are self-contained, with a 4-byte state prefix and no sync words (§6.7). So csdr's encoder is **not** bit-compatible with rx.v1 and would be rewritten anyway.

### (d) Hybrids

- **(d1) Go-native plus assembly kernels.** Pure Go everywhere, with hand-written Go assembly for the 2–3 hot kernels (complex×real FIR dot product, FFT butterflies) behind `GOARCH` build tags: AVX2 on amd64, NEON on arm64. A pure-Go fallback stays as the reference implementation in tests. `CGO_ENABLED=0` is kept.
  - Go 1.26's `simd/archsimd` is experimental (`GOEXPERIMENT=simd`) and amd64-only. arm64 (NEON) support is reported for Go 1.27 but was not verified in this spike.
- **(d2) Go-native, ported from csdr.** The algorithms are ported from csdr where useful. The BSD-3 files can be ported under any licence with attribution: FIR, FM demod, shift, ADPCM, fractional decimator, de-emphasis, CW, FAX, SSTV, noise filter, SNR. The GPLv3+ files (AGC, AFC, NAVTEX/DSC/SITOR-B) can also be ported into an AGPL-3.0-or-later work, as long as their notices are kept; see "Licence assessment".
- **(d3) Go-native core, cgo for optional blocks.** libcsdr is linked behind a `csdr` build tag only for the "+" decoders (SSTV, FAX, NAVTEX, DSC). The cost is two build variants and two images.
- **(d4) Shared FFT channelizer.** This is an architecture choice that works with any of the above. Each device runs an overlap-save fast-convolution filter bank (one large forward FFT per block, shared by all sessions). Each listener then takes its bins and runs a small inverse FFT at its own rate. The per-listener cost no longer grows with the device sample rate, which matters for hardware B at 10 MS/s with 100 listeners. This is the "channelizer" row of §8.3. The waterfall FFT cannot be reused for it as is: the waterfall is decimated in time (fps), while the channelizer must see every sample.

### Comparison

| Criterion | (a) Go-native | (b) csdr processes | (c) cgo libcsdr++ | (d1) Go + asm kernels |
|---|---|---|---|---|
| Fit with §8.3 (natively, fixed pool, no thread per block, bounded rings with gap markers) | Direct: the prototype shows the fixed pool, stateful blocks and 0 allocations | Conflicts: needs a spec issue | Only if the leaf kernels are called from our pool; the libcsdr framework itself conflicts | Direct |
| "SIMD where available" (§8.3) | No: scalar Go, no auto-vectorisation | Yes (inside csdr) | Yes (ifunc, NEON) | Yes, for the kernels written in assembly |
| Per-listener chain and shared FFT | Both trivial in process. One FFT per device, encoded once, the same `[]byte` sent to every connection (ADR 0004) | The shared FFT is one process per device plus parsing. The chains are pipelines | As (a), with C kernels | As (a) |
| Media WS fit (rx.v1 header `timestamp_us`, seq, `discontinuity`, back-pressure) | Sample accounting is exact through decimation; drops and gaps in process; never blocks DSP | Pipe buffering hides latency; drops and sample index cannot be tracked across pipes | As (a) | As (a) |
| Tuning response (`demod.set` ≤ 150 ms) and SRC-016 | Offset change in place (prototype: `SetOffset`, no rebuild); chain swap is a pointer swap | Respawn or FIFO control per block | As (a) | As (a) |
| CPU (qualitative) | Scalar float32. Cost is dominated by taps × rate, so the filter planner and the channelizer matter more than the language | Native SIMD, but pipe copies and context switches for every block and listener | Native SIMD kernels, small cgo cost per block | Close to native in the kernels |
| Memory (qualitative) | Rings and filter state per listener (tens of KB); one Go heap; GC quiet with a 0-allocation hot path; `GOMEMLIMIT` available | Processes × (stack + ~50 MB nmux-style rings if used) | As (a) plus C heaps | As (a) |
| ARM cross-compile | `GOARCH=arm64 CGO_ENABLED=0`, checked by the prototype | Needs C/C++ arm64 builds of csdr and its libraries | Needs a C/C++ cross toolchain or arm64 runners | Pure Go toolchain (assembly is part of the Go toolchain) |
| Image | distroless static unchanged (ADR 0001, ADR 0012 `prod-node`) | distroless/cc or Debian slim, plus binaries and libraries | distroless/cc plus `.so` files, or a static C++ link | distroless static unchanged |
| Licence impact | None beyond our own (and attributions for anything ported) | GPL binaries shipped in the image: source offer, pinned versions | Node binary combines GPLv3+ and GPLv2+ code (compatible, see below) | None |
| Maintenance | We own all DSP code: the whole DEM catalogue, NR, AGC profiles, resampler, ADPCM; Opus is a separate choice | We depend on the cadence of a single-maintainer fork (luarvique/csdr, active, last commit 2026-09-24) and on CLI stability | We maintain a C++ shim plus build plumbing on top of the fork's C++ API | As (a), plus two small assembly files per architecture |
| Testing | Stdlib `testing`, synthetic IQ, golden fixtures, fuzzing, race detector; arm64 tests under QEMU | Integration tests only, with pinned binaries | C libraries in every CI image | As (a); the pure-Go fallback is the oracle for the assembly |
| Decoder adapter (§8.4) and SPK-08 | DSP taps feed decoder stdin through the I/O reactor, 2 s bounded buffer and gaps; same for every option | Same supervisor, many more processes to supervise | Same as (a) | Same as (a) |

### Indicative measurements (x86 laptop only, not reference hardware)

These were measured on an Intel i7-1355U (hybrid P/E cores, no frequency pinning) inside Docker. Two runs differed by up to 2×. They are **not** evidence about the Pi 5 or the 20-listener requirement, and no extrapolation is made. Full tables are in `spikes/spk-01-dsp/README.md`.

- One NFM or AM chain (2.4 MS/s in, 12 or 48 kHz out, pure Go scalar, unoptimised filters) used about 3–10 % of one x86 core in real time.
- A USB chain at 48 kHz used 15–39 % because the naive planner runs the narrow 300 Hz SSB filter at 240 kS/s (4470 taps). At 12 kHz the same chain uses 611 taps and 4–9 %. Filter planning dominates cost.
- The shared spectrum costs 0.2–0.4 % of one core at 4096 bins and 10 fps, and about 1 % at 16384 bins. The FFT library choice is irrelevant to capacity.
- The hand-written complex64 FFT was the fastest candidate or on par with scientificgo. It was about 1.5–10× faster than gonum (complex128) and allocation-free, unlike go-dsp and scientificgo.
- 20 mixed chains fanned out on an 11-worker pool used about a quarter of the laptop.
- The hot path allocates nothing (0 B/op), so GC pressure is not a concern for the DSP itself.

## Licence assessment

The facts below are "to verify before bundling" (§8.7). They are not legal advice.

| Component | Licence | Combined with an AGPL-3.0-or-later work |
|---|---|---|
| libcsdr++ (luarvique/csdr) | Per file: GPLv3+ (Ketterl's 2021+ framework: module, ringbuffer, async, fft, firdecimate, agc, amdemod, exec…; luarvique's afc, navtex, dsc, sitorb, ccir*) and BSD-3 (Retzler-era fir, fmdemod, shift, adpcm, fractionaldecimator, deemphasis, window, logpower…; luarvique's cw, fax, sstv, mfrtty, noisefilter, snr) | **Compatible.** GPLv3 §13 and AGPLv3 §13 explicitly allow combining GPLv3 and AGPLv3 works. Each part keeps its own licence, and AGPL §13 (network use) applies to the combination. Linking (c) or porting (d2) is allowed. |
| FFTW3 (libcsdr dependency) | GPLv2-or-later | Compatible (usable under GPLv3). Only relevant for (b) and (c). |
| libsamplerate | BSD-2 (since 0.1.9) | Compatible. |
| owrx_connector | GPLv3+ | Process boundary (already by spec). The image must ship or offer its source. |
| nmux | BSD-3 | Compatible. Replaceable by our own fan-out. |
| gonum | BSD-3 | Compatible. |
| libopus (added by Decision 7) | BSD-3 | Compatible. Notices reproduced in binary distributions. |
| madelynnblue/go-dsp | ISC | Compatible. |
| scientificgo/fft | BSD-3 | Compatible. |

Consequences:

1. For **this** project (AGPL-3.0-or-later), none of the options creates a licence incompatibility. The real differences are elsewhere:
   - (b) and (c) bring GPL source-distribution duties for the image: pinned versions and a source offer;
   - (c) and (d2) with GPL files make the node binary a combined work that can never be relicensed under a permissive licence without removing that code (§8.7 licensing note 2).
2. The "or later" clauses matter: the csdr files are GPLv3-**or-later** and FFTW is GPLv2-**or-later**, so they can all be used under GPLv3 together with AGPLv3. A GPLv2-**only** component could not be combined in process; none was found among the DSP candidates.
3. Porting a GPL file is a derivative work, exactly like linking it. Ported files keep their copyright and licence notices.
4. The repository has no `LICENSE` file yet. Recording AGPL-3.0-or-later formally is a prerequisite for shipping anything that combines GPL code.

## Recommendation (from the spike)

The owner decided otherwise; see Decision. The recommendation is kept as the record of the spike's analysis.

Option **(a) Go-native**, structured to leave room for (d1) and (d4):

1. **Pure Go, `CGO_ENABLED=0`.** This keeps ADR 0001's toolchain, the distroless static image and the ADR 0012 node-only artifact, and arm64 cross-compilation as it is today. DSP blocks run on goroutines under our own fixed worker pool, sized by `node.dsp_workers`. Blocks are stateful and work on complex64/float32 blocks, with a 0-allocation hot path, as the prototype does.
2. **Hand-written complex64 radix-2 FFT, no new dependency.** It needs no dependency approval, it is allocation-free, and it was among the fastest here. If the owner prefers a library, gonum (BSD-3, best maintained) is the fallback, at the cost of complex128 conversions.
3. **Algorithms ported or written from the literature.**
   - BSD-3 csdr files may be ported with attribution.
   - Whether GPLv3+ csdr files may be ported is an owner question (Q6).
   - IMA ADPCM is written to the rx.v1 layout (§6.7), not csdr's sync-word variant.
4. **Connectors and decoders stay processes** (§8.2, §8.4, SPK-08). nmux is replaced by the device front-end fan-out. Live retune (SRC-016) uses the connector control socket and is independent of this choice.
5. **Performance is decided by architecture, not by language:**
   - a proper filter planner: narrow filters at the lowest rate, half-band and polyphase stages, and a rational resampler for 11025/22050/44100 Hz;
   - a shared channelizer (d4) when a device has more than `dsp.channelizer_threshold` sessions, as §8.3 already allows.
6. **SIMD (d1) as a measured escape hatch.** Assembly kernels for the FIR dot product and FFT butterflies would be added only if the reference-hardware benchmark (a follow-up, not this spike) shows the scalar engine misses §2.3. cgo to libcsdr++ (c) stays the last resort, because it gives up the static image and simple cross-compilation, and its framework conflicts with §8.3.
7. **Do not adopt (b) for DSP blocks.** It contradicts §8.3 and defeats the rx.v1 back-pressure and timestamp model.

Main risk: the §2.3 capacity requirement (20 listeners on a Pi 5 class node at ≤ 80 % CPU) is **unverified** for every option. A reference-hardware benchmark must gate M1a (Q4).

## Questions raised by the spike (answered in Decision)

1. **Strategy:** adopt (a) Go-native with (d1)/(d4) as planned extensions, or choose (b), (c) or another hybrid?
2. **"SIMD where available" (§8.3):** does scalar pure Go satisfy it for M1a, with assembly kernels added only if the reference benchmark requires them? Or must SIMD kernels (amd64 AVX2 and arm64 NEON) be there from the start? Go 1.26's `simd/archsimd` is experimental and amd64-only.
3. **FFT:** hand-written (no dependency), or approve `gonum.org/v1/gonum` (or another candidate) as a dependency?
4. **Reference-hardware benchmark:** this spike did not measure a Pi.
   - Should a follow-up ticket measure the §2.3 capacity (listeners and CPU per mode family on a Pi 5 class node and on x86) once the first real chain exists, and should it gate M1a?
   - Who provides the hardware?
5. **Worker pool:** §8.3 requires a fixed pool with no thread per block. Must DSP run on an explicit pool of `node.dsp_workers` goroutines, as prototyped? Or is a goroutine per chain acceptable, since goroutines are not OS threads and `GOMAXPROCS` bounds the threads?
6. **Porting GPL code:** may GPLv3+ csdr files (AGC, AFC, NAVTEX/DSC/SITOR-B…) be ported into the node? That is legal under AGPL-3.0-or-later, but closes the door to a permissive relicence. Or must ports be limited to the BSD-3 files, with the rest clean-room?
7. **Licence file:** confirm AGPL-3.0-or-later and add `LICENSE` (and per-file notices for ported code)? This is outside this ADR's scope but a prerequisite.
8. **Channelizer timing:** start with independent per-listener chains and add the shared channelizer (d4) when needed? Or design the channelizer in from M1a?
9. **Opus encoder:** Opus SHOULD be the default audio codec (§6.7), and it belongs to the per-listener chain.
   - Pure-Go encoders now exist: `github.com/thesyncim/gopus` (BSD-3, pre-v1, claims parity with libopus 1.6.1) and `github.com/kazzmir/opus-go` (BSD-3, libopus transpiled with ccgo). Neither was evaluated here.
   - Should DEM-010 evaluate them, or is a cgo libopus exception acceptable?
10. **DSP code placement:** a shared `internal/dsp` package (like `internal/protocol/rxv1`), or the `infra` layer of a node-side receiver module?
11. **Spec issues below:** record only (as in ADR 0004), or open issues?

## Decision

The project owner decided as follows (2026-10-07). It differs from the spike's recommendation.

1. **Strategy: option (c), cgo bindings to libcsdr++** (luarvique/csdr). The DSP blocks of the per-listener chains and the shared spectrum use libcsdr++ through cgo.
2. **FFT in Go: `gonum.org/v1/gonum/dsp/fourier`**, approved as a dependency, for the FFT work done on the Go side (BSD-3).
3. **No performance gate and no benchmark ticket.** §2.3 capacity is not measured on reference hardware before M1a.
4. **Porting GPLv3+ csdr files is allowed.** The project is AGPL-3.0-or-later; a `LICENSE` file is added by another PR. Ported files keep their copyright and licence notices.
5. **One goroutine per listener chain.** There is no explicit worker pool; `GOMAXPROCS` bounds the OS threads.
6. **Shared FFT channelizer per device from M1a** (option d4): an overlap-save filter bank per device, from which each listener chain takes its band.
7. **Opus encoder: libopus through cgo.** The Go binding is chosen by DEM-010.
8. **Placement: `internal/dsp`**, shared code like `internal/protocol/rxv1`.
9. **Spec issues are recorded in this ADR only.** No GitHub issues are opened.

Answers to the spike's questions: Q1 → 1; Q2 → SIMD comes from libcsdr++ (ifunc, NEON) through 1; Q3 → 2; Q4 → 3; Q5 → 5; Q6 → 4; Q7 → 4 (`LICENSE` in another PR); Q8 → 6; Q9 → 7; Q10 → 8; Q11 → 9.

### Rules that follow from the decision, for the implementation

- **The libcsdr framework is not used.** Its `AsyncRunner` (one `std::thread` per module) and its rings, which let slow readers overrun silently, conflict with §8.3 and with Decision 5. The bindings call the leaf `process()` kernels synchronously from the listener goroutine (or from the channelizer goroutine of the device). Rings, gap markers, `sample_index` and timestamps stay in Go.
- **A C ABI shim.** libcsdr++ is C++ (templates), and cgo calls C. `internal/dsp` holds a small `extern "C"` shim compiled by cgo with g++, plus the Go wrappers. No C++ type crosses the boundary; buffers are passed as pointer and length, with Go memory pinned only for the duration of the call (cgo pointer rules).
- **cgo calls are per block, never per sample** (cost per call: tens of nanoseconds).
- **IMA ADPCM** follows the rx.v1 layout (§6.7, no `SYNC` words). It is written or ported in Go, not taken from csdr's `AdpcmEncoder`.
- **Gonum works in complex128.** Where gonum computes FFTs (on the Go side), complex64 IQ is converted at the boundary. The planner reuses `CmplxFFT` instances and `dst` buffers so the hot path does not allocate.
- **Connectors and decoders stay processes** (§8.2, §8.4, SPK-08). nmux is replaced by the device front-end fan-out.

## Spec issues

Found during the spike. The spec is not edited.

1. **FFT compression.** RX-015 (#114) says FFT frames come "in float32 or ADPCM per `fft_compression`", and §8.3 says the spectrum is encoded "once per codec variant (`adpcm`, `none`)". §6.7 says "FFT data MUST NOT be ADPCM-compressed" and defines u8 dB (`0x10`, MUST) and f32 dB (`0x11`, MAY).
2. **Reference hardware differs between sections.** §8.3 latency targets use "4-core ARM64 at 1.8 GHz, one 2.4 MS/s device, 10 NFM listeners". §2.3 capacity uses "Raspberry Pi 5 class" (whose cores run at 2.4 GHz) with 20 listeners.
3. **"SIMD where available"** (§8.3) has no acceptance criterion. It is unclear whether it is a requirement or a hint, given the capacity NFR.
4. **§8.7 libcsdr++ row:** "The ADPCM wire variants MUST be bit-compatible with `rx.v1`". csdr's ADPCM encoder inserts `SYNC` words that rx.v1 forbids, so it cannot be bit-compatible as is.
5. **§8.3 output rates:** `output_rate` includes 11025, 22050 and 44100 Hz. These are not integer divisors of common SDR rates (2.4 MS/s, 2.048 MS/s), so a rational resampler is mandatory. This is implied by "resample", but worth stating because it affects the chain cost.
6. **Issue #1 deliverables** ask for "listeners per node and CPU per mode family for each option". The owner decision for this spike excludes hardware benchmarks, and the owner decided there is no performance gate and no benchmark ticket (Decision 3).
7. **§8.3 worker pool vs Decision 5.** §8.3 requires "a fixed worker pool (dataflow scheduler) … A thread per block or per module is forbidden", sized by `node.dsp_workers`. One goroutine per listener chain is a different scheduling model, bounded only by `GOMAXPROCS`; `node.dsp_workers` has no direct meaning there.
8. **§1 summary vs Decision 1.** §1 describes the DSP as "external SDR connectors and decoder tools reused as processes"; that still holds for connectors and decoders. The DSP blocks are linked in process (FFI), which §8.3 and §8.7 allow.

## Consequences

### Build and toolchain

- **`CGO_ENABLED=1` for every binary that contains the node**: the default binary (`hub`, `node`, `all` in one) and the node-only artifact (`-tags nogateway`, ADR 0012). This amends ADR 0001's "built with `CGO_ENABLED=0`" for those builds. The SQLite driver stays `modernc.org/sqlite` (pure Go); only the DSP and Opus packages use cgo.
- **A hub-only pure-Go build is still possible, but not decided here.** It would need a build tag that leaves the node (and `internal/dsp`) out of the binary, the mirror of `nogateway`. Until such a tag exists, the hub ships in the cgo binary too. The `nogateway` tag keeps its meaning (no Caddy) and is orthogonal: the node-only artifact is `CGO_ENABLED=1 -tags nogateway`.
- **C/C++ toolchains in the build stage**: gcc/g++, pkg-config, and the development packages of libcsdr++, libfftw3f, libsamplerate and libopus, at pinned versions. libcsdr++ (luarvique/csdr 0.18.x) is not packaged by Debian at the version needed and is built from source at a pinned commit.
- **arm64 cross-compilation** needs one of: a cross toolchain (`aarch64-linux-gnu-gcc/g++`, `CC`/`CXX` set, `GOARCH=arm64`) with arm64 builds of every C library (Debian multiarch `:arm64` packages, plus libcsdr++ cross-built); `docker buildx` with QEMU emulation (simple, slow); or native arm64 CI runners. The choice belongs to the ticket that sets up the image build.
- **Tests and lint** need the same C libraries in the dev image (`make test`, `make lint`, golangci-lint with cgo). The dev `base` stage gains the toolchain and libraries.

### Images

- **The node image (and `all`) moves from `distroless/static` to Debian slim** (owner decision from SPK-08: Debian slim instead of distroless for the node image; a `.deb` package comes much later). It carries the shared libraries (libcsdr++, libfftw3f, libsamplerate, libopus, libstdc++), runs as a non-root user, and keeps the config, volume and port layout of ADR 0012.
- A hub-only image could stay `distroless/static-debian13:nonroot` only if the hub-only pure-Go build above is introduced.

### Licences and notices

- The node binary is a combined work of AGPL-3.0-or-later code with libcsdr++ (GPLv3+ and BSD-3 per file), FFTW3 (GPLv2+), libsamplerate (BSD-2) and libopus (BSD-3), plus gonum (BSD-3). This is compatible (see "Licence assessment"); it can never be relicensed permissively while these remain.
- Images MUST ship or offer the corresponding sources of the bundled GPL libraries at the exact pinned versions (§8.7 licensing note 3), and carry their licence texts and copyright notices (for example under `/usr/share/doc/<lib>/` or a `THIRD_PARTY_NOTICES` file in the image). BSD licences require the notices to be reproduced in binary distributions too.
- Ported csdr files keep their headers (GPLv3+ or BSD-3) in `internal/dsp`.

### Engine

- SIMD comes from libcsdr++ (function multi-versioning, NEON), which addresses "SIMD where available" (§8.3) without Go assembly.
- The scheduling, rings, gap markers, timestamps and back-pressure remain Go code, as in option (a); only the numeric kernels are C++.
- The shared channelizer (Decision 6) bounds the per-listener cost independently of the device sample rate; it is built in M1a, together with the first chains.
- One goroutine per chain: chain swaps on a mode change are a goroutine handover; there is no global DSP scheduler to tune, and §8.3's `node.dsp_workers` is not used (spec issue 7).
- Capacity on reference hardware stays unmeasured (Decision 3). Any shortfall would only be found in the field.

### Maintenance

- We maintain the C ABI shim over a C++ API that belongs to a single-maintainer fork; csdr upgrades need the shim and the pinned versions to move together.
- Two languages in the node (Go, C++), and cgo pointer rules to respect in every wrapper.

### Follow-ups

- Image build ticket: Debian slim node image, C/C++ toolchain, libcsdr++ from source, arm64 strategy, licence notices and source offer.
- Decide whether a hub-only pure-Go build tag is wanted.
- DEM-010: choose the libopus Go binding.
- AGENTS.md (stack line "`CGO_ENABLED=0`", Dockerfile stages) and ADR 0001 / ADR 0012 references are updated by the first implementation PR.

## References

- TECHNICAL_SPEC §1, §2.3 (latency budget, capacity), §6.4–6.9 (media WS messages, binary frames, back-pressure, limits), §8.1–8.4 (node internals, device manager, DSP engine contract, decoder adapter), §8.7 (dependencies plan, licensing note).
- Issues #1 (SPK-01), #114 (RX-015), #154 (DEM-001), #163 (DEM-010), #144 (SRC-016), #8 (SPK-08), #4 (SPK-04).
- ADR 0001 (`CGO_ENABLED=0`, distroless static), ADR 0002 and ADR 0012 (gateway, node media WS, node-only artifact), ADR 0004 (rx.v1 framing, send queue, `TCP_NOTSENT_LOWAT`), ADR 0008 (grid and control channel).
- `docs/archive/openwebrxplus-technical-audit.md` §4.3.1, §6.10.2 (owrx_connector), §6.10.3 (csdr: per-file licences, AsyncRunner threads, ADPCM SYNC), §8.3.5 (licensing).
- Prototype: `spikes/spk-01-dsp/` (README, tests, indicative benchmarks) on branch `spike/spk-01-dsp` (throwaway, not merged).
- gonum `dsp/fourier`: https://pkg.go.dev/gonum.org/v1/gonum/dsp/fourier; go-dsp: https://pkg.go.dev/github.com/madelynnblue/go-dsp/fft; scientificgo/fft: https://pkg.go.dev/github.com/scientificgo/fft; gofft: https://pkg.go.dev/github.com/argusdusty/gofft.
- Opus in Go (not evaluated): https://pkg.go.dev/github.com/thesyncim/gopus, https://pkg.go.dev/github.com/kazzmir/opus-go.
- Go 1.26 release notes (`simd/archsimd`, `GOEXPERIMENT=simd`): https://go.dev/doc/go1.26; archsimd proposal: https://github.com/golang/go/issues/73787.
- GNU GPLv3 §13 and GNU AGPLv3 §13 (use with the other licence): https://www.gnu.org/licenses/gpl-3.0.html, https://www.gnu.org/licenses/agpl-3.0.html.
