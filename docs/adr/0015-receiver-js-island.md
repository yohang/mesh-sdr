# ADR 0015: Receiver JS island (waterfall, spectrum, audio)

- Status: Accepted
- Date: 2026-10-07
- Deciders: project owner
- Spike: SPK-04 (#4). Unblocks RX-002 (#101), RX-015 (#114), DEM-010 (#163). Related: RX-004, RX-008, RX-014, RX-016–RX-019, UI-009, UI-013, ADM-013.
- Builds on: ADR 0003 (islands, nonce-only CSP, no Node in the app image), ADR 0004 (`rx.v1` framing, `internal/protocol/rxv1`), ADR 0007 (UI shell), ADR 0012 (gateway, media access; on branch `epic/grid-3`).

This ADR comes from a spike. It records the decision, then the measurements, the options considered, the spike's recommendation and the questions the owner answered. No dependency is added.

## Context

The Receiver page cannot be built with htmx alone. It needs a JS island that:

- opens the node media WebSocket `/nodes/{nodeId}/ws` (subprotocol `rx.v1`) through the gateway, whose upgrade is authorised by the session cookie and the hub authz, which injects the first access token (§5.16, ADR 0012). Later tokens come from `POST /api/v1/auth/token` and are sent with `auth.refresh`; they live in JS memory only (§5.8);
- parses the 24-byte little-endian binary frames (§6.7): FFT u8 dB lines (`0x10`, MUST) and audio as IMA ADPCM (`0x01`, MUST), Opus (`0x02`, SHOULD be the default) or PCM (`0x00`, MAY);
- draws a full-width waterfall (RX-015) and an optional spectrum with peak hold redrawn every 150 ms (RX-016) with admin palettes (UI-013) and levels (RX-017/018);
- plays audio with an adaptive 60–150 ms jitter buffer and ≤ 20 ms output stage (§2.3), concealment on gaps and no time-stretching (§6.7);
- survives boosted navigation: long-lived resources live outside `#main` (ADR 0003, ADR 0007);
- meets UI-009: text equivalents for canvases, throttled `aria-live`.

Constraints already decided: templ + htmx 4, islands as custom elements with `templ.JSONScript` initial state, ES modules under `/static/js/`, **`script-src` nonce-only** (no `'self'`, no `'unsafe-eval'`), 2024 browser floor (Chrome/Edge 123+, Firefox 128+, Safari 17.5+), no Node in the app or dev image (Node only in the CI-only `.infra/a11y` image), no new dependency without the owner.

### What the prototype does

Throwaway code in `spikes/spk-04-receiver-island/` (own `go.mod`, `replace github.com/yohang/mesh-sdr => ../..`; the main `go.mod` is untouched):

| Part | Files |
|---|---|
| Test server: page under the **real** app CSP (`internal/http.ContentSecurityPolicy`, `?csp=` adds one variant), vendored htmx, boosted `#main` | `cmd/spk04/main.go` |
| Fake node media WS: `session.hello`/`welcome`, `time.sync`, `device.attach` → `device.config` + `stream.open`, `audio.configure`, `stream.configure`, frames built with `rxv1.AppendFrame`, `AppendFFTU8`, `AppendADPCM` | `cmd/spk04/main.go`, `synth/` |
| Synthetic spectrum (noise, steady, drifting and keyed carriers), 700 Hz tone, IMA ADPCM encoder + reference decoder | `synth/` |
| `rx.v1` JS codec (header, FFT u8, ADPCM prefix, envelope), IMA ADPCM decoder | `web/static/js/receiver/rxv1.js`, `adpcm.js` |
| Shell-level engine (WS, `time.sync` clock offset, AudioContext, decoders, 1024-line FFT history, back-off reconnect, FFT pause on hidden tab) | `engine.js` |
| `<msdr-receiver>` island in `#main`, `<msdr-audio-dock>` outside `#main` | `island.js`, `dock.js` |
| Waterfall: Canvas 2D and WebGL2, same interface; spectrum (Canvas 2D, max per column, peak hold, theme tokens) | `waterfall-2d.js`, `waterfall-gl.js`, `spectrum.js`, `palette.js` |
| Audio: AudioWorklet jitter buffer + linear resampler, blob-URL fallback, main-thread scheduled-buffer fallback | `audio.js`, `rx-worklet.js` |
| Feature/CSP probe | `probe.js`, `probe-worker.js` |
| Bench (Playwright Chromium/Firefox/WebKit in the `.infra/a11y` image), bundle sizes (esbuild as a Go tool) | `bench/run.mjs`, `bench/run.sh`, `bench/bundle.sh` |

`go vet` and `go test` pass for the spike module (ADPCM round trip ≥ 20 dB SNR). The JS ADPCM decoder matches the Go reference decoder sample for sample (720/720, 0 mismatches) in all three engines.

## Decision

The owner accepted the spike recommendation except for the renderer: **Canvas 2D only in M1, no WebGL2.** The open questions are settled with the spike's recommendations, listed below.

1. **Renderer (Q6): Canvas 2D** for the waterfall and the spectrum (option 1A). No WebGL2 or WebGPU in M1. The renderer stays behind the small interface prototyped (`pushLine`, `setLevels`, `replay`, `draw`, `destroy`) so another one can be added later through a new ADR. Level and palette changes re-colour the history from the raw lines kept by the engine.
2. **Wide FFTs (Q7): decimate.** Before a line is written, the client reduces it to at most the canvas pixel width with a peak-preserving max per bin group, so narrow carriers survive. `fft.size` is not capped by the client.
3. **Audio (Q2): AudioWorklet** with the prototyped jitter buffer (option 2A). The media WebSocket and decoding run on the main thread first. A dedicated Worker (2C) comes only if jank is measured on reference clients, through a new ADR (it needs a second CSP entry). The engine API keeps that move internal. Scheduled `AudioBufferSourceNode`s (2B) stay as the degraded path when the worklet cannot load.
4. **Stall behaviour (Q3): bounded latency.** The jitter target adapts between 60 and 150 ms (+20 ms per underrun, −10 ms per 5 s without one); audio above target + 60 ms is dropped; gaps are filled with silence; no time-stretching. Under main-thread stalls the listener hears glitches rather than a latency that keeps growing (§2.3).
5. **CSP (Q1): exact worklet file URLs (option 3A).** `script-src` keeps its nonce and adds the absolute URL of the worklet file for the origin of `hub.url` and for each `gateway.extra_origins` entry, if any. No `blob:`, no `'self'`, no `'wasm-unsafe-eval'`. The listed file is a static embedded asset, never user-controlled, and is served without redirect. The `internal/http` CSP tests change with it.
6. **Codecs (Q4, Q5):** IMA ADPCM decoded in plain JS everywhere (4A); Opus through WebCodecs `AudioDecoder` (4B), advertised in `session.hello.capabilities.audio_codecs` only when `AudioDecoder.isConfigSupported({codec: "opus"})` is true. Browsers without it (Firefox 128–129, Safari 17.5–18.x) get ADPCM. **No WASM in M1.** Opus gaps are concealed with silence, as for ADPCM (no PLC entry point in WebCodecs).
7. **Sample rate (Q9):** one long-lived `AudioContext` at the device default rate; the worklet resamples from the stream rate (linear for speech-band audio; a better filter when wideband modes need it). The context is not recreated when the stream rate changes.
8. **Build (Q8): no build step.** Plain ES modules with JSDoc types, served from `embed` (ADR 0003 5A). No bundler, no `tsc` in M1.
9. **Mounting (Q10, Q11):** `<msdr-receiver>` custom element with its initial state from `templ.JSONScript` (6A). The engine (WebSocket, AudioContext, decoders, FFT history) is a module singleton imported by `shell.js`, never owned by `#main`; a shell-level audio dock outside `#main` starts and stops audio on every page. The receiver JS lives with the shared presentation code in `internal/web/static/js/receiver/`. The engine keeps a bounded history of raw FFT lines (1024 lines) so the waterfall is redrawn after boosted navigation.
10. **FPS targets (Q12):** the client must draw every FFT line at up to 8192 bins × 30 fps without falling behind (measured in all three engines), and must stay usable above that thanks to decimation. FFT frame age stays within §2.3 (≤ 200 ms p95). Reference client hardware is still to be named; real-client measurements are owed before M1 exit.
11. **Cross-origin isolation (Q13): out of scope.** No COOP/COEP, no `SharedArrayBuffer`.
12. **Accessibility:** the prototyped text equivalents (canvases with `role="img"`, a non-live text list of frequency, mode and level refreshed every second, a `role="status"` region throttled to one announcement per 2 s). The receiver page joins the `.infra/a11y` job, and a real-browser receiver test (Playwright, CI-only image) covers boosted navigation with audio, CSP and the Go/JS codec golden tests.
13. **Spec divergences (Q14):** recorded in this ADR only; no spec edit and no issue.

## Measurements

Setup: everything in one container from the `meshsdr-a11y-a11y` image (Playwright 1.63: HeadlessChrome 153, Firefox 155, WebKit 26.6), server on loopback, viewport 1280×800, waterfall canvas 1248×400 CSS px. Each receiver run: 2 s warm-up, then an 8 s window.

Caveats, which limit what these numbers prove:

- **Headless, no GPU.** Chromium's WebGL2 is SwiftShader (software); headless Firefox refuses WebGL2 ("AllowWebgl2:false"), so its `webgl` runs used the 2D fallback. WebKit reports "Apple GPU" but runs on Linux. GPU-backed numbers on reference clients are **not measured**.
- **Not the floor versions.** The engines are current (Chromium 153, Firefox 155, WebKit ≈ Safari 26.6). Support at Chrome 123 / Firefox 128 / Safari 17.5 comes from documentation (MDN, caniuse), not from runs.
- **Firefox audio not measured.** Headless Firefox in the container has no audio sink; its AudioContext stayed `suspended`, so no audio latency or underrun numbers for Firefox.
- **Timer resolution.** Firefox and WebKit clamp `performance.now()` to 1 ms, Chromium to 0.1 ms: a p50 of 0 means "below resolution". Draw times are JS submission time only; rasterisation and compositing happen elsewhere.
- **Latency** is the client-side part only: frame age at receipt (node `timestamp_us` corrected by the `time.sync` offset; the synthetic node stamps audio one 20 ms frame before sending) + jitter-buffer level + `baseLatency + outputLatency`. No node DSP, no real network.

### Rendering (FFT 25–30 fps, ADPCM audio at 12 kHz)

| Engine | Renderer used | FFT size @ fps | Lines drawn/s | rAF/s | JS per line, p95 (push + draw) | FFT age p95 | Main thread busy (Chromium CDP) |
|---|---|---|---|---|---|---|---|
| Chromium | 2D | 2048 @ 25 | 24.6 | 59 | 0.1 + 0.1 ms | 0.9 ms | 13 % |
| Chromium | WebGL2 (software) | 2048 @ 25 | 23.9 | 56 | 0.1 + 0.1 ms | 57 ms | 34 % |
| Chromium | 2D | 8192 @ 30 | 30.2 | 60 | 0.2 + 0.1 ms | 0.6 ms | 18 % |
| Chromium | WebGL2 (software) | 8192 @ 30 | 29.8 | 59 | 0.2 + 0.1 ms | 8.8 ms | 34 % |
| Chromium | 2D | 32768 @ 10 | 10.1 | 57 | 1.0 + 7.9 ms | 7.9 ms | 34 % |
| Chromium | 2D (fallback: 32768 > MAX_TEXTURE_SIZE 8192) | 32768 @ 10 | 9.7 | 57 | 0.6 + 4.1 ms | 127 ms | 22 % |
| Firefox | 2D | 2048 @ 25 | 25.0 | 60 | 1 + 5 ms | 0.5 ms | n/a |
| Firefox | 2D | 8192 @ 30 | 30.0 | 60 | 1 + 6 ms | 1.2 ms | n/a |
| Firefox | 2D | 32768 @ 10 | 10.0 | 60 | 1 + 17 ms | 1.2 ms | n/a |
| WebKit | 2D | 2048 @ 25 | 25.0 | 62 | 1 + 1 ms | 0.3 ms | n/a |
| WebKit | WebGL2 | 2048 @ 25 | 25.1 | 62 | 1 + 1 ms | 1.1 ms | n/a |
| WebKit | 2D | 8192 @ 30 | 30.0 | 62 | 1 + 0 ms | 0.9 ms | n/a |
| WebKit | WebGL2 | 8192 @ 30 | 30.0 | 62 | 1 + 1 ms | 0.6 ms | n/a |
| WebKit | 2D | 32768 @ 10 | 10.0 | 50 | 5 + 34 ms | 1.5 ms | n/a |
| WebKit | 2D (fallback: 32768 > 16384) | 32768 @ 10 | 10.0 | 52 | 6 + 32 ms | 3.0 ms | n/a |

Reading: every engine keeps up with the FFT rate at 2048 and 8192 bins with either renderer. Canvas 2D cost grows with the FFT width (one `putImageData` row at full FFT width, then a scaled blit); at 32768 bins it reaches 8–34 ms per line on the CPU, and WebKit's rAF drops to ~50/s. WebGL2 could not be judged on a GPU here: software GL costs more main-thread time in Chromium (34 % vs 13–18 %) and adds FFT age, which says nothing about hardware GL. FFTs wider than `MAX_TEXTURE_SIZE` (8192 SwiftShader, 16384 WebKit; §6.9 allows 32768) need tiling or decimation before upload.

Bandwidth seen by the client: 2048 @ 25 + ADPCM 12 kHz ≈ 475 kbit/s; 8192 @ 30 ≈ 2.0 Mbit/s; 32768 @ 10 ≈ 2.7 Mbit/s; PCM 48 kHz instead of ADPCM 12 kHz ≈ 1.2 Mbit/s with 2048 @ 25.

### Audio (idle main thread)

| Engine | Path | Client latency p50 / p95 | Underruns in 8 s | Reported output latency | Decode per 20 ms ADPCM frame |
|---|---|---|---|---|---|
| Chromium | AudioWorklet | 145–175 / 153–210 ms | 0–2 | 42 ms (base 10 ms) | 2.4 µs (benchmark) |
| Chromium | scheduled buffers (strict CSP) | 143 / 145 ms | 0 | 42 ms | — |
| WebKit | AudioWorklet | 69–95 / 78–131 ms | 0–2 | 2.9 ms | 3.4 µs |
| WebKit | scheduled buffers (strict CSP) | 103 / 104 ms | 0 | 2.9 ms | — |
| Firefox | not measured (context suspended, no sink) | — | — | — | 3.8 µs |

ADPCM decode cost is negligible (a few µs per frame). WebCodecs Opus decode, measured by an encode → decode round trip in the probe: 0.06–0.3 ms per 20 ms packet in Chromium, 1.1 ms in Firefox, 0.12 ms in WebKit.

### Audio under main-thread jank

A busy loop of 150 or 300 ms once per second on the main thread (WS receive and ADPCM decode run there in the prototype):

| Engine | Jank | Path | Underruns in 8 s | Latency p50 / p95 | What happened |
|---|---|---|---|---|---|
| Chromium | 150 ms | scheduled | 0 | 281 / 283 ms | the backlog grew to ~220 ms and never shrank |
| Chromium | 150 ms | worklet | 3 | 211 / 282 ms | target rose to 130 ms; glitches; shrinks back |
| Chromium | 300 ms | scheduled | 0 | 453 / 459 ms | backlog ~390 ms |
| Chromium | 300 ms | worklet | 8 | 204 / 411 ms | target 150 ms (cap); 1.5 s of audio dropped |
| WebKit | 150 / 300 ms | scheduled | 0 / 0 | 251 / 401 ms p50 | same as Chromium |
| WebKit | 150 / 300 ms | worklet | 4 / 8 | 188 / 166 ms p50 | same as Chromium |

Reading: a worklet alone does not protect the audio from main-thread stalls when the main thread feeds it. Scheduled buffers hide stalls by letting latency grow, which breaks "shrink back towards 60 ms" (§2.3). The worklet keeps latency bounded at the price of audible glitches. Moving the WS and decoding off the main thread (a dedicated Worker) is the only way to avoid both; it was **not prototyped**.

### CSP, worklets, workers and WASM (same result in all three engines)

| Load | App CSP (nonce-only) | + exact file URLs in `script-src` | + `blob:` | + `'wasm-unsafe-eval'` |
|---|---|---|---|---|
| `audioWorklet.addModule(same-origin URL)` | **blocked** | allowed | blocked | blocked |
| `addModule(blob: URL)` | blocked | blocked | allowed | blocked |
| `addModule(data: URL)` | blocked | blocked | blocked | blocked |
| `new Worker(url, {type: "module"})` | **blocked** (`worker-src` → `script-src`) | allowed | blocked | blocked |
| `WebAssembly.instantiate` | **blocked** | blocked | blocked | allowed |

Findings:

- A worklet or worker request carries no nonce, so the nonce-only policy blocks every way to load one. Firefox and WebKit report the worklet violation as `script-src-elem` (CSP3: worklet destinations fall under `script-src-elem`, then `script-src`); **Chromium blocks it without firing a `securitypolicyviolation` event**, so a CSP report endpoint would not see it there.
- A host-source with a full path (`https://hub.example/static/js/receiver/rx-worklet.js`) allows exactly that file and nothing else on the origin; it needs the absolute origin (from `hub.url`, plus `gateway.extra_origins`) because CSP has no path-only source.
- Without any exception, the only audio path is main-thread scheduling (`AudioBufferSourceNode`), measured above.

### Boosted navigation (ADR 0003 1A)

With the engine as a module singleton and `<msdr-audio-dock>` in the header: clicking a boosted link to another page and back keeps the same document, the same WebSocket and a running AudioContext. Audio frames kept arriving and playing (150 frames in 3 s on the other page in Chromium and WebKit, 0 new underruns); the island detached its view on `disconnectedCallback` and re-attached on return, replaying the waterfall from the engine's history. Firefox: same document and socket; audio not measured (see caveats).

### Feature support

| Feature | Measured (Playwright engines) | Floor: Chrome 123 / Firefox 128 / Safari 17.5 (docs) |
|---|---|---|
| AudioWorklet | yes / yes / yes | yes / yes / yes (secure context only) |
| `AudioContext({sampleRate})` 8000, 12000, 48000 | accepted in all three | yes (exact range per engine not verified at floor) |
| WebCodecs `AudioDecoder`, Opus | yes / yes / yes (`isConfigSupported` true, round trip OK) | **Chrome 94+ yes; Firefox 130+ (128–129 no); Safari 26+ (17.5–18.x no)**. Not exposed in `AudioWorkletGlobalScope`; exposed in dedicated workers |
| Opus packet-loss concealment through WebCodecs | not tested | no documented entry point (libopus does it by decoding a missing packet) |
| WebGL2 | SwiftShader / refused headless / yes | yes / yes / yes |
| WebGPU | no adapter / no `navigator.gpu` / no `navigator.gpu` | Chrome 113+; Firefox: not on by default (Windows only from 141); Safari 26+. **Not on the floor** |
| `OffscreenCanvas` transfer | yes in all three | yes (Safari 16.4+; WebGL in workers 17+) |
| `SharedArrayBuffer` | no (needs cross-origin isolation, COOP/COEP) | needs COOP/COEP |
| `'wasm-unsafe-eval'` | honoured in all three | Chrome 97+, Firefox 102+, Safari 16+ |

### Bundle size and build

esbuild v0.28.2 runs as a Go tool with no Node: declared in the spike's `go.mod` (`go get -tool github.com/evanw/esbuild/cmd/esbuild`), run with `go tool esbuild` in the dev image (~0.2 s per bundle, including tool start-up). The esbuild site documents `go build ./cmd/esbuild`; the CLI built this way has no JS plugins (the Go API takes Go plugins).

| Form | Raw | gzip -9 |
|---|---|---|
| Unbundled modules (sum of 14 files, incl. 6 KB spike-only probe) | 48.9 KB | 15.6 KB (per-file gzip) |
| esbuild bundle of `shell.js`, minified, target chrome123/firefox128/safari17.5 | 23.5 KB | 9.1 KB |
| Worklet, minified separately (cannot be bundled into the page module) | 2.1 KB | 0.9 KB |
| For scale: vendored htmx 4 | 36.7 KB | — |
| WASM libopus decoder | **not measured** | (libopus decoders compiled to WASM are typically several tens of KB; to measure if chosen) |

Notes: bundling changes module URLs, so `new URL("./rx-worklet.js", import.meta.url)` must point at a separately served worklet file. Unbundled, the receiver costs ~13 small requests (HTTP/2 via Caddy), cached as static assets.

## Options

### 1. Waterfall and spectrum renderer

| Option | How | Pros | Cons |
|---|---|---|---|
| **1A. Canvas 2D (prototyped)** | One `ImageData` row per line through a 256-entry level LUT into a ring canvas, two scaled `drawImage` blits per frame | Works everywhere, no context loss, simplest to test. Kept up with 8192 bins @ 30 fps in all engines | CPU cost grows with FFT width (8–34 ms per line at 32768 bins). A level or palette change has to re-colour the history from stored raw lines (prototyped: replay of up to 1024 lines). Downscaling is bilinear, so narrow carriers can fade when the FFT is wider than the canvas |
| **1B. WebGL2 (prototyped)** | Raw u8 lines in an R8 ring texture (`texSubImage2D` per line), levels and palette in the fragment shader, a full-screen triangle | Re-colours the whole history for free (RX-017 levels, RX-018 continuous auto-levels, UI-013 palettes). Per-line CPU cost is one texture row. Peak-preserving decimation can be done in the shader | Context loss to handle. Software GL on GPU-less machines costs more than 2D (measured). FFT width capped by `MAX_TEXTURE_SIZE` (8192 SwiftShader, 16384 WebKit): needs tiling or decimation for §6.9's 32768. Shader code to test. Needs a 2D fallback (headless Firefox refused WebGL2) |
| 1C. WebGPU | Compute + render pipeline | Future-proof | Not on the 2024 floor (Firefox, Safari < 26). Rejected for now |
| 1D. Rendering in a Worker via `OffscreenCanvas` | `transferControlToOffscreen`, WS + render in a worker | Main-thread jank does not freeze the waterfall | Worker needs a CSP exception (§3 below). Pointer events (RX-008 tuning) stay on the main thread. More plumbing. Not prototyped |

Either 1A or 1B sits behind the same small interface (`pushLine`, `setLevels`, `replay`, `draw`, `destroy`); the spectrum (RX-016) is cheap in 2D in both cases (max per pixel column, 150 ms).

### 2. Audio playback

| Option | How | Pros | Cons |
|---|---|---|---|
| **2A. AudioWorklet jitter buffer (prototyped)** | Ring buffer in the worklet, adaptive target 60–150 ms (+20 ms per underrun, −10 ms per 5 s without one, drop above target + 60 ms), silence on gaps, linear resampler to the context rate, stats over the port | Output on the audio thread, ≤ one render quantum (2.7 ms) of its own latency. Latency stays bounded | Needs a CSP exception (§3). Glitches when the feeding thread stalls (measured) |
| 2B. Scheduled `AudioBufferSourceNode`s on the main thread (prototyped fallback) | One buffer per frame, scheduled back to back | No CSP exception, no worklet file | Every main-thread stall turns into permanent latency (281–453 ms measured) unless the code drops audio itself; jitter control is coarse; one node per 20 ms |
| 2C. 2A fed by a dedicated Worker (WS + decode in the worker) | Worker owns the media WS and decoders, posts PCM to the worklet over a transferred `MessagePort` and FFT lines to the main thread | Main-thread jank no longer touches audio. WebCodecs is available in workers | Two CSP exceptions (worklet and worker). Token refresh and control messages cross a thread boundary. Not prototyped |
| 2D. 2C with a `SharedArrayBuffer` ring | Lock-free ring between worker and worklet | Lowest overhead, no message per frame | Needs cross-origin isolation (COOP `same-origin`, COEP `require-corp`/`credentialless`): affects map tiles and every cross-origin resource. Not prototyped |

**Sample rate.** Options: (a) context at the device default rate with a resampler in the worklet (prototyped, linear: fine for speech-band audio, a polyphase filter would be better for WFM/HD audio); (b) `new AudioContext({sampleRate: streamRate})` (all three engines accepted 8000, 12000 and 48000) and let the browser resample, recreating the long-lived context when the stream rate changes; (c) ask the node for 48 kHz only.

### 3. CSP for the worklet (and a worker)

ADR 0003 decided `script-src` nonce-only. Worklets and workers cannot carry a nonce, so the receiver needs one of:

| Option | Policy change | Pros | Cons |
|---|---|---|---|
| **3A. Exact file URLs (prototyped, works in all engines)** | `script-src 'nonce-…' https://<hub>/static/js/receiver/rx-worklet.js` (and the worker file for 2C); `worker-src` falls back to `script-src` | Only those files become loadable; uploads and every other same-origin URL stay blocked. No `blob:`/`'self'` | The policy needs absolute origins (`hub.url` and each `gateway.extra_origins`), so it becomes config-driven. The listed files must never be user-controlled. Path matching is relaxed after redirects: static files must not redirect |
| 3B. `blob:` in `script-src` | Load the worklet source as a Blob | No origin in the policy | Any script that can create a blob can then run code from it: weakens the nonce policy considerably |
| 3C. `'self'` | — | Simplest | Rejected by ADR 0003 (same-origin uploads would become executable) |
| 3D. No exception: 2B only | none | Policy unchanged | Audio quality depends on the main thread (measured) |
| 3E. Separate audio origin | Worklet served from another origin listed in the CSP | — | Contradicts the hub-only entry point; rejected |

A `report-to` endpoint (deferred to M5 by ADR 0003) would not catch the Chromium worklet case, which fires no violation event.

### 4. Codec decoding

| Option | How | Pros | Cons |
|---|---|---|---|
| **4A. IMA ADPCM in plain JS (prototyped)** | 40-line decoder, golden-tested against Go | MUST anyway; µs per frame; no dependency; can run in the worklet or a worker | 4:1 only (~48 kbit/s at 12 kHz) |
| **4B. Opus via WebCodecs `AudioDecoder` (probed)** | Advertise `"opus"` in `session.hello` only when `AudioDecoder.isConfigSupported({codec:"opus"})` is true; ADPCM otherwise | Native, no download, no CSP change; ~0.1–1 ms per packet | Missing on Firefox 128–129 (ESR) and Safari 17.5–18.x, which then get ADPCM. Not exposed in worklets (decode on main thread or worker). No documented PLC call for §6.7's "Opus PLC" |
| 4C. Opus via WASM libopus (vendored, self-hosted) | e.g. a pinned libopus WASM build | Same decoder everywhere on the floor; real PLC (`opus_decode` with no packet); can run inside the worklet | Needs `'wasm-unsafe-eval'` in `script-src`. A new vendored frontend library (licence BSD-3 for libopus; the wrapper's own licence to check) and its update policy. Size not measured |
| 4D. ADPCM only for M1 | Opus later | Smallest scope | §6.7 says Opus SHOULD be the default; bandwidth (RK-02) |

### 5. Build tooling

| Option | Pros | Cons |
|---|---|---|
| **5A. No build: plain ES modules + JSDoc types (prototyped; ADR 0003 5A says "no bundler")** | Nothing to install; served as-is from `embed`; the modules inherit the entry nonce; debugging maps 1:1 | ~13 requests; 15.6 KB gzip vs 9.1 KB bundled; no type checking unless a checker runs somewhere |
| 5B. esbuild as a Go tool (`go get -tool github.com/evanw/esbuild/cmd/esbuild`), run from `go generate` like Tailwind | Verified with no Node; minify + bundle + target down-levelling; can strip TypeScript syntax (no type check) | New tool dependency in the main `go.mod` (owner approval). Generated bundle: never committed (like `app.css`). The worklet stays a separate entry. Plugins only through the Go API |
| 5C. TypeScript with `tsc` | Real type checking | `tsc` needs Node: conflicts with "no Node in the app/dev image" unless it runs only in a CI container |
| 5D. 5A + `tsc --checkJs --noEmit` on JSDoc in a CI-only container (like `.infra/a11y`) | Type safety without changing the shipped files or the dev image | Another CI container with pinned Node packages |

### 6. Mounting the island

- **6A. Custom element + `templ.JSONScript` (prototyped, the ADR 0003 decision).** `<msdr-receiver>` contains its JSON config; `connectedCallback` builds the DOM with `createElement`/`textContent` (a hostile device name stays text), attaches a view to the engine and starts `requestAnimationFrame`; `disconnectedCallback` cancels and detaches. No `style=""`: sizes come from CSS classes, CSSOM writes are allowed.
- **Engine ownership.** The engine (WS, AudioContext, decoders, FFT history) is a module singleton imported by `shell.js` and never inside `#main`; a shell-level `<msdr-audio-dock>` gives start/stop on every page. Options for where the files live: `internal/web/static/js/receiver/` (shared presentation) or a receiver module's own static directory embedded and mounted by the shell. Options for the history: keep raw lines in the engine (prototyped: 1024 lines, 8 MiB at 8192 bins) or start blank after navigation.
- **Audio start (RX-004).** `AudioContext` is created and resumed inside the click handler; the island hides its "Start audio" button once running.

### 7. Accessibility (UI-009)

Prototyped: canvases with `role="img"`, an `aria-label` and `aria-describedby` pointing at a text list (frequency, mode, signal level at the tuned bin) refreshed once per second and **not** live; a separate `role="status"` region announcing connection states at most once every 2 s (last state wins); the dock text is not live either; buttons are native `<button>`s. Not prototyped: keyboard tuning (RX-008/RX-020 keys), the frequency input, focus handling for canvas interactions, a WCAG audit (needs the `.infra/a11y` job once the page exists).

## Spike recommendation (as submitted)

1. **Renderer: 1B WebGL2 with 1A Canvas 2D as the fallback**, behind one interface. Reason: RX-017/018 re-colour the history on every level change, which is free in a shader; 2D is kept for GPU-less or WebGL-refusing clients. Before committing, measure both on the reference clients (no GPU numbers exist yet), and add tiling or peak-preserving decimation for FFTs wider than `MAX_TEXTURE_SIZE`. If the owner prefers the smaller scope, 1A alone meets 8192 @ 30 fps in every engine measured.
2. **Audio: 2A AudioWorklet** with the prototyped jitter policy; 2B only as the degraded path when the worklet cannot load. Start with WS + decode on the main thread (measured fine when idle) and keep the engine API such that moving them into a worker (2C) is internal, if jank shows up on reference clients.
3. **CSP: 3A**, exact file URLs for the worklet (and the worker if 2C), built from `hub.url` and `gateway.extra_origins`.
4. **Codecs: 4A + 4B**: ADPCM in JS everywhere, Opus through WebCodecs where `isConfigSupported` says so, negotiated through `session.hello.capabilities.audio_codecs`. No WASM in M1 (keeps `'wasm-unsafe-eval'` out). Revisit 4C if Opus PLC turns out to matter.
5. **Build: 5A** (no build, JSDoc types), optionally 5D for type checking in CI. 5B stays available (verified) if bundle size or TS syntax becomes a goal.
6. **Mount: 6A** with the engine as a shell-level singleton and a shell audio dock; keep a bounded FFT history in the engine.
7. **Accessibility:** adopt the prototyped text equivalents and throttled status; add the receiver page to `.infra/a11y/urls.txt` and a real-browser receiver test (the a11y image can already drive Chromium, Firefox and WebKit).

## Questions put to the owner

All answered in the Decision section.

1. **CSP exception for the worklet.** Accept 3A (exact file URLs from `hub.url` + `gateway.extra_origins`, making the CSP config-driven), or 3B (`blob:`), or 3D (no worklet, main-thread audio)?
2. **Worker.** Should WS + decoding move to a dedicated Worker now (2C, a second exact-file exception), later if measured jank requires it, or never?
3. **Jitter policy under stalls.** Confirm the bounded-latency behaviour (drop audio above target + 60 ms, glitches under jank) over growing latency (no glitches, latency up to ~450 ms measured), as §2.3 implies.
4. **Opus.** WebCodecs only with ADPCM fallback (Firefox 128–129 and Safari 17.5–18.x get ADPCM), WASM libopus (needs `'wasm-unsafe-eval'` and a vendored library), or ADPCM only in M1?
5. **Opus PLC.** §6.7 requires Opus PLC on gaps. With WebCodecs there is no documented PLC call: accept silence (as for ADPCM) when WebCodecs decodes Opus?
6. **Renderer.** WebGL2 with a 2D fallback, or Canvas 2D only for M1?
7. **Wide FFTs.** For `fft.size` above `MAX_TEXTURE_SIZE` (§6.9 allows 32768): tile the texture, decimate (max per bin group) before upload, or cap `fft.size` in the UI/settings?
8. **Build tooling.** No build (5A), esbuild as a `go tool` in the main `go.mod` (5B), or 5A plus a CI-only `tsc --checkJs` container (5D)?
9. **Audio sample rate.** Resample in the worklet (prototyped), create the AudioContext at the stream rate (recreated on change), or always request 48 kHz from the node?
10. **Where the receiver JS lives:** `internal/web/static/js/receiver/` or static files owned by the receiver module?
11. **Waterfall history across navigation:** keep N raw lines in the engine (how many?) or redraw from empty?
12. **Target FPS and reference clients.** The spec gives a 10 fps capacity target (node side) and `fft.fps` as a setting, but no client frame-rate budget or reference client hardware ("budgets met on reference clients", M1 exit). Which FPS × FFT size and which client devices define "target FPS"?
13. **Cross-origin isolation.** Is a `SharedArrayBuffer` ring (2D) ever in scope? It needs COOP/COEP on every page.
14. **Spec divergences below:** record them only, or open spec issues?

## Divergences and gaps found in the specification

Recorded here only; the spec is not edited and no issue is opened.

1. **RX-015 vs §6.7.** RX-015 says FFT frames come "in float32 or ADPCM per `fft_compression`", and §8 (shared spectrum) encodes "once per codec variant (`adpcm`, `none`)"; §6.7 says "FFT data MUST NOT be ADPCM-compressed" and makes u8 dB the MUST codec. The prototype follows §6.7.
2. **ADM-013 vs §6.7.** ADM-013 makes `adpcm` the default audio codec; §6.7 says Opus SHOULD be the default.
3. **DEM-010 "12 kHz" vs §6.2/§6.7.** DEM-010 names a 12 kHz output path; the `session.hello` example advertises `audio_rates:[48000,44100]` and Opus decodes on a 48 kHz clock. The rate negotiation between the two is unspecified.
4. **§6.7 Opus PLC** assumes a decoder with PLC; WebCodecs exposes none.
5. **§2.3 audio output ≤ 20 ms.** Chromium reported 42 ms (`baseLatency` 10 ms + `outputLatency`) in the container; the output stage is device- and browser-dependent and not under the product's control.
6. **§6.9 `fft.size` ≤ 32768** exceeds common `MAX_TEXTURE_SIZE` values (8192–16384) for a GPU waterfall; no client-side constraint is specified.
7. **§8 Frontend libraries** lists no audio or WASM library; any Opus WASM decoder would be a new self-hosted, pinned entry there.
8. **No client frame-rate budget**: §2.3 budgets FFT frame age (≤ 200 ms p95) but not client FPS or CPU.

## Consequences


- The receiver engine is shell-level code with its own lifecycle (connect, back-off, token refresh, visibility pause) and must never be owned by `#main` content.
- The client mirrors `internal/protocol/rxv1` rules in JS (header, codec family, reserved flags dropped, `seq` gaps). A golden test between Go and JS (as prototyped for ADPCM) keeps them aligned; it needs a browser runtime, i.e. the CI-only Playwright image.
- The worklet changes the ADR 0003 policy (one exact file URL per origin, built from config); the `internal/http` CSP tests change with it. A future worker would add one more entry.
- Opus support differs per browser on the 2024 floor; the client must negotiate codecs from feature detection, and the node must keep ADPCM.
- Canvas 2D costs CPU in proportion to the drawn width; decimation to the canvas width bounds it. Measurements on reference clients (and on Firefox with an audio device) are still owed before M1 exit, to confirm the Canvas 2D budget and the jitter numbers.
- The spike code stays on branch `spike/spk-04-receiver-island` as a reference; it is throwaway.

## References

- Issues: SPK-04 #4, RX-002 #101, RX-015 #114, DEM-010 #163.
- `docs/spec/TECHNICAL_SPEC.md` §2.3 (latency budget, security baseline), §5.8, §5.16, §6.1–6.9, §8 (frontend libraries); `docs/spec/FEATURE_SPEC.md` RX-002–RX-020, RX-043, DEM-010, UI-007–UI-019, ADM-012, ADM-013.
- ADR 0003, ADR 0004, ADR 0007, ADR 0012 (`epic/grid-3`).
- Prototype: `spikes/spk-04-receiver-island/` (README, `bench/run.mjs`, `bench/bundle.sh`).
- CSP Level 3 (effective directive for worklet/worker requests, `script-src-elem`, `worker-src`, `'wasm-unsafe-eval'`): <https://www.w3.org/TR/CSP3/>.
- Web Audio API, AudioWorklet: <https://developer.mozilla.org/en-US/docs/Web/API/AudioWorklet>.
- WebCodecs `AudioDecoder`: <https://developer.mozilla.org/en-US/docs/Web/API/AudioDecoder>; support: <https://caniuse.com/mdn-api_audiodecoder> (Chrome 94, Firefox 130, Safari 26).
- WebGPU support: <https://caniuse.com/webgpu>.
- esbuild "Build from source" (`go build ./cmd/esbuild`, v0.28.2): <https://esbuild.github.io/getting-started/>.
- Opus (RFC 6716) and libopus PLC: <https://opus-codec.org/docs/>.
