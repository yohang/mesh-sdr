# ADR 0004: WebSocket library and rx.v1 framing

- **Status:** Proposed
- **Date:** 2026-10-06
- **Deciders:** project owner
- **Spike:** SPK-07 (#7). Unblocks GRID-008 (#16, control channel) and RX-002 (#101, receiver WS connect).

## Context

Protocol v1 (TECHNICAL_SPEC §6) runs three WebSocket channels: the hub events
WS (`/api/ws`, `rx.v1`), the node media WS (`/nodes/{nodeId}/ws` → node `/ws`,
`rx.v1`) and the hub-dialled control channel (`/control`, `rx-ctl.v1`,
§4.4). They share:

- the JSON envelope `{v, type, id, ts, payload}`, ack/correlation and the error frame (§6.2);
- application close codes 4400–4503 (§6.2);
- the 5 s `session.hello` handshake (§6.2);
- the 24-byte little-endian binary media header, FFT and audio codecs (§6.7);
- a per-connection bounded send queue with a drop policy that drops FFT and protects audio (§6.8);
- size and rate limits (§6.9).

The project needs a Go WebSocket library that works with `net/http` and chi.
On the server side it has to support the hub, the node `/ws` and the node
`/control`. On the client side it has to support the hub dialling
`/control` over mTLS.

Constraints already decided:

- hub ↔ node links use mTLS;
- EdDSA JWT access tokens are verified when the WS opens;
- tests use stdlib `testing` only;
- domain code is log-free and logging uses an injected `*slog.Logger`;
- the layout is light DDD.

This spike delivers:

1. a library comparison, backed by the docs and a prototype for each candidate;
2. a codec package for the protocol that does not depend on any WebSocket
   library. It lives at `internal/protocol/rxv1/` and is meant to be kept;
3. a throwaway prototype in a separate module, `spikes/spk-07-ws/`. It
   implements subprotocol negotiation and the §6.8 send queue, and was
   measured against a deliberately slow client. The full numbers are in
   `spikes/spk-07-ws/README.md`.

## Options

### Option A: `github.com/coder/websocket` (v1.8.15)

This is the former `nhooyr.io/websocket`, taken over by Coder in 2024. Its API
is context-first and minimal, and it has zero dependencies. It is already in
our module graph, as an indirect dependency of a dev tool.

### Option B: `github.com/gorilla/websocket` (v1.5.3)

The historical default. The project was archived in 2022, unarchived in 2023
under new maintainers, and its last release dates from June 2024. Its API is
deadline-based, it allows one writer goroutine, and it has zero dependencies.

### Other candidates (not prototyped)

- `github.com/lxzan/gws` v1.10.2 (Sep 2026): event/callback model, active, fast, but a smaller community, and its callback style does not fit our goroutine-per-connection, `context` design.
- `github.com/gobwas/ws` v1.4.0 (May 2024): low-level, zero-copy, and a lot of hand-written framing and close handling.
- `golang.org/x/net/websocket`: its own documentation points users to other libraries. It has no close-code or ping/pong control. Excluded.

### Comparison

| Criterion | coder/websocket v1.8.15 | gorilla/websocket v1.5.3 |
|---|---|---|
| Maintenance | Active: last release Jun 2026, backed by Coder | Low activity: last release Jun 2024; archived 2022–2023 |
| Dependencies | none | none |
| `context` support | Native on `Read`, `Write`, `Dial`, `Ping`. **Caveat:** an expired context closes the connection, for reads and for writes | None. Must be emulated with `Set{Read,Write}Deadline` and `context.AfterFunc` (done in the prototype) |
| Subprotocol negotiation | `AcceptOptions.Subprotocols`, `Conn.Subprotocol()`. Completes the handshake with `""` when there is no match | `Upgrader.Subprotocols`, `Conn.Subprotocol()`, `websocket.Subprotocols(r)`. Same: completes with `""` |
| §6.1 "426 if absent" | Needs a pre-check before `Accept` (≈ 10 lines, prototyped) | Same pre-check |
| Origin check | `OriginPatterns` (host globs) and same-host by default | `CheckOrigin` func and same-host by default |
| Compression (§6.1: MAY on hub WS, MUST NOT on media WS) | Off by default. Supports context takeover and no-context-takeover | Off by default (`EnableCompression`). No-context-takeover only, documented as experimental |
| Write deadlines / back-pressure | `Write(ctx)`. Expiry is fatal to the connection. `Close` can be called while a `Write` is blocked (it bounds its own close-frame write to 5 s) | `SetWriteDeadline`. Timeout is fatal. `WriteControl` (close, ping) can be called concurrently with a blocked write |
| Concurrency model | `Write` is safe from several goroutines. One reader. `Close`/`CloseNow` safe from any goroutine | One writer, one reader: concurrent writes **panic**. `Close`/`WriteControl` concurrency-safe |
| Close handshake | Built in: `Close(code, reason)` sends, waits for the peer and tears down | Manual: `WriteControl(Close)`, wait for the reader, `Close()` |
| Custom close codes 4xxx | `StatusCode(4413)`; `CloseStatus(err)` reads it | `FormatCloseMessage(4413, …)`; `*CloseError.Code` |
| Read limit | `SetReadLimit`, default **32 KiB**, closes with 1009. Must be raised to 64 KiB on clients for binary frames | `SetReadLimit`, default unlimited, closes with 1009 |
| Ping/pong | `Ping(ctx)` waits for the pong (needs a concurrent reader); `OnPingReceived`/`OnPongReceived` hooks | `WriteControl(Ping)` and `SetPongHandler`. RTT and timeout are handled by hand |
| net/http + chi | `Accept(w, r, …)` in any handler. Needs `http.Hijacker`: avoid `middleware.Compress`/`Timeout` on WS routes (`middleware.WrapResponseWriter` does hijack) | Same (`Upgrader.Upgrade(w, r, nil)`), same hijack constraints |
| mTLS client (hub → node `/control`) | `DialOptions.HTTPClient` with a custom `tls.Config` | `Dialer.TLSClientConfig` / `NetDialTLSContext` |
| Slow-consumer behaviour (measured) | Identical: the policy lives in the queue | Identical |
| Write path, 508 B–4 KiB frames (measured, noisy) | ~4–9 µs; **0 allocs** without a per-write context; +11 allocs / 792 B with `context.WithTimeout` per write | ~5–8 µs; 1–3 allocs / 24–96 B; 5–7 allocs / ~300 B with a per-write deadline context |
| Idle server footprint (measured) | ~14.7 KiB/conn, no extra goroutine | ~17.0 KiB/conn (4 KiB buffers plus a write pool), no extra goroutine |
| Ergonomics with our conventions (`ctx` everywhere, graceful shutdown, tests) | Good: contexts map directly onto request and shutdown contexts | Every `ctx` → deadline mapping is hand-written and has to be tested |

### Measurements (prototype, both libraries, see the spike README)

Setup: fake node with a 4096-bin u8 FFT at 25 fps (~103 kB/s) and IMA ADPCM
48 kHz in 20 ms frames (~25 kB/s). Clients are throttled. Socket buffers are
16 KiB on both sides. All runs over loopback.

| Scenario | Audio received / lost | Audio latency p99 | FFT dropped | fps | Close seen by client |
|---|---|---|---|---|---|
| fast / 200 kB/s | 100 % / 0 | ~1 ms | 0 % | 25 | — |
| 64 kB/s | 1249/1250 / 0 | ~0.9 s | ≈ 29 % | 25 → 12 → 6 | — |
| 16 kB/s (< audio bitrate) | drops oldest, **every gap flagged** `discontinuity` | ~3.7 s | ≈ 84 % | halved | **4413** after ~10 s of sustained overflow |
| stall after 2 s | — | — | ≈ 69 % | — | **none**: the server closed with 4413, but the frame is stuck behind full TCP buffers |
| 100 clients × 64 kB/s | ≥ 99.96 % (rare isolated losses, all flagged) | ~0.9 s | ≈ 32 % | → 6 | — |
| 64 kB/s, **kernel default socket buffers** | only 50 % delivered within the run | **12.4 s and growing** | **0 %** (policy never triggers) | 25 | — |

Main findings:

1. **The library does not matter for back-pressure.** Both behave the same
   under the same queue. The §6.8 policy has to be built as our own
   component; no library provides it.
2. **Kernel and proxy buffering defeat §6.8.** With autotuned TCP buffers,
   megabytes pile up below the application queue. Latency then becomes
   unbounded and the drop and fps rules never fire. The node must bound
   send-side buffering on `/ws` sockets (for example `SO_SNDBUF` ≈ 16–32 KiB
   or `TCP_NOTSENT_LOWAT`). The gateway path must not add buffering either:
   `flush_interval: -1` disables HTTP-level buffering only, not the socket
   buffers.
3. **4413 is best effort.** A consumer that has stopped reading never receives
   the close frame. Clients must treat 1006 or EOF like 4413.
4. **Library pitfalls found:**
   - with coder, an expired read context closes the connection, so the 4408
     handshake timeout must be a timer that calls `Close(4408)`, not a read
     context;
   - coder's client default read limit (32 KiB) is below the 64 KiB frame
     limit;
   - with gorilla, read and write timeouts are fatal and concurrent writes
     panic.

### Codec (kept): `internal/protocol/rxv1`

The package is pure: no I/O, no goroutines, no logging, stdlib only.

- `DecodeEnvelope`:
  - checks UTF-8 and JSON validity (`invalid_json`);
  - requires a single object with **exact-case, non-duplicated** keys (`invalid_envelope` with `details.path`);
  - reads an integer `v`, where any integer other than 1 → `unsupported_version`;
  - checks `type` against the §6.2 grammar (hand-written, no regexp);
  - requires an integer `ts` and an object `payload`;
  - keeps the request `id` on every error, so the error frame can carry `re`.
- `Envelope.DecodePayload(dst, strict)` rejects unknown fields in the strict direction (client → server).
- Typed `MessageType` constants plus four per-channel `Catalogue`s (§6.3) for `unsupported_type`.
- `ErrorCode` (§6.2 table). `Error` matches with `errors.Is` on the code. `ErrorPayloadFrom` maps foreign errors to `internal` with an `incident_id`. `NewAckEnvelope` and `NewErrorEnvelope` (`re: null` for fire-and-forget; `rate_limited` requires `retry_after_ms`). Each error code maps to an immediate or an escalation close code.
- `CloseCode` constants with names and `ShouldReconnect` (§6.2 client behaviour).
- Binary header: `AppendFrame` (validates, ≤ 64 KiB, 0 allocs, ~140 ns). `ParseFrame` (structural checks only, zero-copy, 0 allocs, ~35 ns). `Validate` (type/codec family, reserved flags). `SetFlags` (in place). `SeqGap` (wrap-aware).
- FFT u8 dB prefix (`{db_min, db_step}`) and IMA ADPCM state prefix helpers.
- Tests: table tests, golden bytes for the LE header, and two fuzz targets
  (`FuzzParseFrame`: never panics, rejects anything structurally invalid,
  round-trips every valid header byte for byte; `FuzzDecodeEnvelope`:
  only `*Error` with known codes, encode/decode round trip). Statement
  coverage is 96 %. Both fuzzers ran 30 s clean (3.7 M and 0.25 M execs).
- `DecodeEnvelope` costs ~25 µs and 72 allocs per message, because of
  `encoding/json` v1 token walking. That is irrelevant at 20 msg/s per
  connection. It can be revisited when `encoding/json/v2` leaves
  `GOEXPERIMENT` (it is case-sensitive and rejects duplicate keys natively).

## Recommendation (for the owner to decide)

**Adopt `github.com/coder/websocket` for all three channels**, together with
the following design:

1. **Keep `internal/protocol/rxv1`** as the single codec for `rx.v1` and `rx-ctl.v1`. Wire it to the library only in `infra`/`http` adapters.
2. **Build the §6.8 send queue as our own component**, independent of the library, on the model of `spikes/spk-07-ws/sendq`:
   - producers never block;
   - one writer goroutine per connection;
   - a watcher closes the connection with 4413 even while a write is blocked;
   - audio frames are shared across connections and copied only when the discontinuity flag has to be set.
3. **Bound socket buffering on node `/ws`** (`SO_SNDBUF` or `TCP_NOTSENT_LOWAT`, value to be tuned on real hardware). Without this, §6.8 has no effect.
4. **coder-specific rules:**
   - writer goroutine: use the session context rather than a per-write `WithTimeout` (0 allocs); a slow-consumer watcher enforces the time bounds;
   - handshake 4408: use a timer plus `Close`, never a read context;
   - set `SetReadLimit` to 16 KiB (`rx.v1` inbound), 64 KiB (`rx-ctl.v1`, and binary on clients);
   - check for the subprotocol before `Accept` and answer 426;
   - set `CompressionMode` explicitly: disabled on media and control, open question for the hub WS.

Why coder over gorilla:

- active maintenance;
- a native `context` API that fits graceful shutdown and our conventions;
- writes and `Close` are concurrency-safe, which removes a class of panics;
- a built-in close handshake;
- safe defaults: read limit on, compression off;
- equal or better allocation profile and idle footprint.

The measured throughput difference is within noise at our rates. Gorilla
remains a viable fallback: the adapter in the spike is about 120 lines.

## Open questions for the owner

1. **Library:** confirm coder/websocket, or prefer gorilla for its longer track record?
2. **"Audio backlog above cap for > 10 s"** (§6.8) cannot happen literally: drop-oldest keeps the backlog at or below the cap. The spike implements "overflow drops with no drop-free gap of at least 1 s for longer than 10 s". Accept this interpretation, or specify another one (for example drop ratio over 10 s)?
3. **Socket and gateway buffering:** should the spec require bounded send buffering on node `/ws`, and say what the gateway must do? This would need a separate spec issue (finding 2).
4. **Dequeue priority between JSON control and audio:** §6.8 says audio is "highest priority", but `stream.open` must precede the first binary frame (§6.5). The spike orders JSON > audio > meter > FFT. Is that acceptable?
5. **FFT `seq` under fps halving:** renumber per connection (one copy per delivered frame, as in the spike), or keep the producer's `seq` and let decimation show up as gaps? Should FFT drops set `discontinuity`? §6.7 defines it generically, but §6.8 only mandates it for audio.
6. **Envelope details the codec had to decide:**
   - `id`: the codec requires it to be non-empty, counts its length in characters (code points), and accepts any characters. Should the §6.1 identifier charset apply?
   - unknown top-level keys: ignored;
   - duplicate keys: rejected;
   - key case: exact match required;
   - a non-integer `v`: `invalid_envelope`, not `unsupported_version`.

   Confirm or amend.
7. **Receiver handling of reserved frame types, codecs and flag bits** (§6.7 only covers unknown versions). The codec parses them structurally and leaves the policy to the caller (`Validate`). What should clients do: drop the frame, or report it?
8. **"≤ 64 KiB per binary frame":** does that include the 24-byte header? (The codec assumes it does.)
9. **Placement:** `internal/protocol/rxv1` (outside any bounded context, like `internal/shared`), and where the send queue should live: `internal/protocol/rxv1/sendq`, or a module's `infra`?
10. **Spec issues below:** the issue rules say spec changes go into separate issues. Should they be opened (this spike does not touch GitHub)?

## Spec issues

Found while implementing. The spec is not edited here.

1. **§6.8 audio rule:** see open question 2. "Backlog above cap" cannot occur under drop-oldest.
2. **§6.8 vs transport buffering:** the queue caps (500 ms of audio, 2 FFT frames) do not bound latency while kernel or proxy buffers are unbounded. Measured: 12.4 s of audio latency with zero drops.
3. **§6.2 4413 client behaviour** assumes the code is delivered. For a stalled consumer it cannot be (measured), so clients will see 1006.
4. **§6.7 `stream_error`:** clients must "report `stream_error` once". There is no `stream_error` code in the §6.2 catalogue, and no client → node message carries errors (`error` is server → client only, §6.3).
5. **§6.9 frame size vs §6.7 FFT f32:** `fft.size ≤ 32768` with codec `0x11` (f32) gives 131 KiB, which is over the 64 KiB frame limit. u8 fits (32 800 B). "Larger FFTs MUST be split" is undefined: the header has no fragment index or offset.
6. **§4.4 `file.chunk` vs §6.9:** chunks of up to 256 KiB, base64-encoded in JSON, come to about 342 KiB. That exceeds the 64 KiB `rx-ctl.v1` inbound limit.
7. **§4.5 vs §6.9 ping timing:** the control channel pings every 15 s with a 30 s pong timeout, while "Idle: ping every 20 s, pong timeout 30 s" applies to "All".
8. **§6.1 vs §6.2 close 1008:** §6.1 rejects a missing subprotocol with HTTP 426 before the upgrade, yet 1008 mentions "subprotocol policy violation", which can no longer happen after a 101.
9. **"Repeated" thresholds:** given for `invalid_*` (10 in 60 s → 4400) and for `forbidden` in §5.9 (> 10/min → 4403), but not for `rate_limited` → 4429.
10. **§6.2 `id` grammar** (empty, unit of length, charset) and envelope-level unknown or duplicate keys are not specified (open question 6).
11. **Hub events WS and media WS** share the subprotocol name `rx.v1` with different catalogues. It works, but a client cannot tell from the handshake which catalogue applies. Minor.

## Consequences

Positive:

- A single tested codec for envelope, errors, close codes and binary frames,
  shared by hub, node and the Go side of the control channel. The frontend
  (JS) must mirror the same rules, and the golden-byte tests document them.
- The back-pressure policy is independent of the library, so it can be
  unit-tested with an injected clock (as in the spike) and swapped between
  libraries.
- coder's context API fits graceful shutdown (SIGTERM → cancel → `Close(1001)`)
  without any deadline bookkeeping.

Negative / costs:

- A new runtime dependency (`coder/websocket`, no transitive dependencies).
- The send queue, the slow-consumer watcher and the socket-buffer tuning are
  our own code to maintain and to test under load on target hardware
  (Raspberry Pi class nodes have not been measured).
- coder's "expired context closes the connection" rule must be known by
  everyone writing WS code. It should be documented next to the adapter.
- Envelope decoding via `encoding/json` v1 tokens is slower than necessary.
  This is acceptable for control traffic, and can be revisited with
  `encoding/json/v2`.

Follow-ups if accepted:

- Move `sendq` into production code with its tests.
- Write the WS adapter for chi routes (`/api/ws`, node `/ws`, `/control`) with the 426 pre-check and the Origin check.
- Expose `queue_depth`, drops and 4413 counts in `node.heartbeat` metrics.
- Open the spec issues listed above.

## References

- TECHNICAL_SPEC §4.3–4.5 (mTLS, control channel, heartbeat), §5.8 (access tokens), §5.16 (media WS connect), §6.1–6.9 (Protocol v1).
- Issues #7 (SPK-07), #16 (GRID-008), #101 (RX-002).
- Spike code and full measurements: `spikes/spk-07-ws/README.md`.
- Codec: `internal/protocol/rxv1/`.
- coder/websocket docs (`AcceptOptions`, `CompressionMode`, `Conn.Write`/`Read`/`Close`/`CloseRead`, `SetReadLimit`): https://github.com/coder/websocket, https://pkg.go.dev/github.com/coder/websocket
- gorilla/websocket docs (`Upgrader`, concurrency contract, `WriteControl`, `SetReadLimit`, `FormatCloseMessage`): https://github.com/gorilla/websocket, https://pkg.go.dev/github.com/gorilla/websocket
- RFC 6455 (WebSocket), RFC 7692 (permessage-deflate), RFC 8037 (EdDSA in JOSE).
- Linux `tcp(7)`: `SO_SNDBUF`, `TCP_NOTSENT_LOWAT`.
