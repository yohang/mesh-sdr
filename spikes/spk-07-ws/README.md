# SPK-07: WebSocket library and rx.v1 back-pressure prototype

Throwaway prototype for issue #7. It backs ADR
[`docs/adr/0004-websocket-library-and-rxv1-framing.md`](../../docs/adr/0004-websocket-library-and-rxv1-framing.md).
**Do not import from production code.** The part meant to stay is the codec at
`internal/protocol/rxv1/`. This module uses it through a `replace` directive.

It is a separate Go module, so `go test ./...`, `golangci-lint` and the
Docker build of the main module ignore it.

## Layout

| Path | What |
|---|---|
| `transport/` | `Conn`/`Lib` interface with adapters for `coder/websocket` v1.8.15 and `gorilla/websocket` v1.5.3, the 426 subprotocol pre-check, and small-socket-buffer helpers |
| `sendq/` | Per-connection send queue implementing the §6.8 policy without depending on a WebSocket library (JSON never dropped, meter coalescing, FFT latest-wins ×2, audio 500 ms drop-oldest + discontinuity flag, 1 MiB/4 MiB caps) |
| `session/` | Fake node (one DSP producer fanned out to N sessions: handshake, 4408, 1003, 1009, fps halving, 4413 watcher) and a measuring client |
| `cmd/wsbench/` | Runs the scenarios and prints the tables below |

## How to run

```sh
docker compose run --rm --no-deps app bash -c 'export GOPATH=/cache/gopath; cd spikes/spk-07-ws && go test -race ./...'
docker compose run --rm --no-deps app bash -c 'export GOPATH=/cache/gopath; cd spikes/spk-07-ws && go run ./cmd/wsbench -server coder'
#   flags: -server coder|gorilla  -client coder|gorilla  -only <substring>  -sockbuf <bytes|0>  -footprint <n>  -scale <x>
docker compose run --rm --no-deps app bash -c 'export GOPATH=/cache/gopath; cd spikes/spk-07-ws && go test -run ^$ -bench . -benchmem ./transport/'
```

`GOPATH=/cache/gopath` is needed only because the dev image's `/go/pkg` is not
writable by the `app` user when new modules are downloaded (sumdb cache).

## What the tests prove (both libraries, all 4 server×client pairs, `-race`)

- No `rx.v1` offered → **HTTP 426** (pre-check). Neither library rejects a
  missing subprotocol on its own: both complete the handshake with `""`.
- `rx.v2, rx.v1` offered → `rx.v1` is echoed, and no `Sec-WebSocket-Extensions`
  is sent (compression is off).
- `session.welcome` and both `stream.open` messages reach the client before the first binary frame.
- A client binary frame closes the connection with **1003**. A text message over 16 KiB closes it with **1009** (the library read limit).
- No `session.hello` within 5 s → **4408**.
- A 16 kB/s consumer gets **4413**, and every audio seq gap it sees has the `discontinuity` flag set.

## Setup of the measurements

- Fake DSP: FFT with 4096 u8 bins at 25 fps (4128 B frames, ~103 kB/s), and IMA
  ADPCM 48 kHz mono in 20 ms frames (508 B, ~25 kB/s). Each frame is encoded
  once and shared by every queue.
- Client: always `coder/websocket`, so that the server library is the only variable.
  Reads are throttled to a byte rate, or stop entirely (stall).
- Socket buffers: server `SO_SNDBUF` and client `SO_RCVBUF` are 16 KiB (Linux
  doubles that), unless stated otherwise.
- Machine: i7-1355U (12 threads), Linux 6.12, Go 1.26.8, inside the dev
  container, over loopback. Each server library runs in its own process.
- Column meanings: "audio recv/sent" counts frames received by the client
  against frames enqueued by the server. "lost (runs)" is the number of
  missing seq numbers, with the count of gaps in brackets. "flagged runs" is
  how many gaps had the `discontinuity` flag. Latency is client receive time
  minus `timestamp_us`.

## Results: back-pressure scenarios

| server lib | scenario | audio recv/sent | audio lost (runs) | flagged runs | audio lat p50/p99/max ms | longest audio gap ms | FFT delivered | FFT dropped in queue | fps halvings → final fps | client close code | 4413 | peak queue KiB | peak heap MiB | alloc MiB | CPU s |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| coder/websocket | fast | 749/750 | 0 (0) | 0/0 | 1/1/1 | 0 | 99.7 % | 0.0 % | — | — | 0 | 4 | 4.2 | 12 | 0.3 |
| coder/websocket | slow 200 kB/s | 749/750 | 0 (0) | 0/0 | 1/1/2 | 0 | 99.7 % | 0.0 % | — | — | 0 | 4 | 4.2 | 12 | 0.3 |
| coder/websocket | slow 64 kB/s | 1249/1250 | 0 (0) | 0/0 | 246/895/920 | 0 | 71.2 % | 28.8 % | 2 → 6 | — | 0 | 17 | 4.3 | 12 | 0.4 |
| coder/websocket | slow 16 kB/s | 172/550 | 284 (5) | 5/5 | 2521/3717/3741 | 1460 | 14.9 % | 84.0 % | 2 → 12 | 4413 ×1 | 1 | 20 | 4.4 | 5 | 0.1 |
| coder/websocket | stall after 2 s | 100/648 | 0 (0) | 0/0 | 1/1/1 | 0 | 23.8 % | 69.4 % | — | — | 1 | 20 | 4.5 | 7 | 0.2 |
| coder/websocket | fan-out 100 × 64 kB/s | 99892/99946 | 36 (11) | 11/11 | 676/924/1061 | 100 | 67.4 % | 32.5 % | 200 → 6 | — | 0 | 20 | 11.6 | 712 | 5.9 |
| gorilla/websocket | fast | 749/750 | 0 (0) | 0/0 | 1/1/2 | 0 | 99.7 % | 0.0 % | — | — | 0 | 4 | 4.0 | 12 | 0.2 |
| gorilla/websocket | slow 200 kB/s | 750/750 | 0 (0) | 0/0 | 1/1/1 | 0 | 99.7 % | 0.0 % | — | — | 0 | 4 | 4.2 | 12 | 0.2 |
| gorilla/websocket | slow 64 kB/s | 1249/1250 | 0 (0) | 0/0 | 247/873/901 | 0 | 70.9 % | 29.1 % | 2 → 6 | — | 0 | 16 | 4.2 | 11 | 0.4 |
| gorilla/websocket | slow 16 kB/s | 182/552 | 299 (5) | 5/5 | 2569/3778/3895 | 1420 | 15.4 % | 83.6 % | 2 → 12 | 4413 ×1 | 1 | 20 | 4.4 | 5 | 0.1 |
| gorilla/websocket | stall after 2 s | 101/651 | 0 (0) | 0/0 | 1/1/1 | 0 | 24.2 % | 69.1 % | — | — | 1 | 20 | 4.4 | 7 | 0.2 |
| gorilla/websocket | fan-out 100 × 64 kB/s | 99939/99984 | 6 (3) | 3/3 | 669/909/1021 | 40 | 67.4 % | 32.4 % | 200 → 6 | — | 0 | 20 | 13.0 | 663 | 6.2 |

Reading:

- **The two libraries behave the same.** The policy lives in the queue, not in the library.
- **64 kB/s: the policy works as intended.** Audio arrives complete (1249/1250;
  the last frame was still in flight at shutdown). FFT is dropped (≈ 29 %),
  and the fps is halved twice, 25 → 12 → 6, with `stream.update` sent.
- **16 kB/s** is slower than the audio bitrate. Audio is dropped oldest-first,
  every gap is flagged, and after about 10 s of sustained overflow the server
  closes with 4413, which the client receives.
- **Stall.** The server detects the slow consumer and closes (4413 counted on
  the server side), but the client never sees the 4413 frame: it is stuck
  behind full TCP buffers. Clients must treat 1006/EOF like 4413.
- **Fan-out**, 100 clients: a few isolated audio losses (≤ 0.04 %) with both
  libraries. These come from CPU contention between 100 throttled clients and
  the server in one process. All gaps were flagged.
- **Audio latency is driven by the kernel buffers, not by the 500 ms queue
  cap.** At 64 kB/s, p99 is about 0.9 s with 16 KiB buffers. See below.

## Results: socket buffer size vs latency (scenario "slow 64 kB/s")

| server lib | `-sockbuf` | audio recv/sent | audio lat p50/p99/max ms | FFT dropped in queue | fps halvings |
|---|---|---|---|---|---|
| coder/websocket | 4 KiB | 1249/1250 | 1/291/306 | 31.6 % | 2 → 6 |
| coder/websocket | 16 KiB (default above) | 1249/1250 | 246/895/920 | 28.8 % | 2 → 6 |
| coder/websocket | kernel default (autotune) | **622/1250** | **6233/12370/12503** | **0.0 %** | none |
| gorilla/websocket | 4 KiB | 1249/1250 | 1/282/305 | 31.6 % | 2 → 6 |
| gorilla/websocket | 16 KiB | 1249/1250 | 247/873/901 | 29.1 % | 2 → 6 |
| gorilla/websocket | kernel default (autotune) | **622/1250** | **6234/12372/12505** | **0.0 %** | none |

With default (autotuned) buffers, the application queue **never sees
back-pressure**. Megabytes pile up in the kernel, audio latency grows without
bound (12.5 s after 25 s), and neither the FFT drop policy nor the fps
adaptation ever triggers. The §6.8 policy only works if the node bounds the
send-side socket buffering, for example with `SO_SNDBUF` or `TCP_NOTSENT_LOWAT`,
and if the gateway does not add its own unbounded buffering. This holds for
both libraries.

## Results: idle connection footprint (server side, 1000 connections, one parked `Read` each)

| server lib | connections | heap / conn | goroutines / conn |
|---|---|---|---|
| coder/websocket | 1000 | 14.6–14.8 KiB | 1.00 (the handler goroutine itself) |
| gorilla/websocket | 1000 | 17.0 KiB | 1.00 |

Gorilla was configured with 4 KiB read/write buffers and a `sync.Pool` write buffer pool.

## Results: server write path microbenchmark (`transport/bench_test.go`)

The raw TCP client discards everything, so only the server write path is
measured. Values are medians of 4 runs. Noise on this laptop is about ±40 %, so
read them as orders of magnitude.

| lib | frame | per-write `context.WithTimeout` | ns/op | B/op | allocs/op |
|---|---|---|---|---|---|
| coder | 508 B | yes | ~5 800 | 792 | 11 |
| coder | 508 B | no | ~4 100 | **0** | **0** |
| coder | 4128 B | yes | ~14 800 | 792 | 11 |
| coder | 4128 B | no | ~8 500 | 0 | 0 |
| coder | 32 KiB | yes | ~15 000 | 792 | 11 |
| coder | 32 KiB | no | ~14 000 | 0 | 0 |
| gorilla | 508 B | yes | ~7 400 | 296 | 5 |
| gorilla | 508 B | no | ~5 400 | 24 | 1 |
| gorilla | 4128 B | yes | ~8 400 | 368 | 7 |
| gorilla | 4128 B | no | ~8 300 | 96 | 3 |
| gorilla | 32 KiB | yes | ~27 000 | 368 | 7 |
| gorilla | 32 KiB | no | ~14 700 | 96 | 3 |

- `context.WithTimeout` itself costs about 4 allocs and 272 B (visible in the gorilla delta).
- On top of that, coder's context watching adds about 7 allocs and 520 B per write.
- Without a per-write context, coder writes allocate nothing.
- At realistic rates both are negligible. A node with 100 listeners at ~75 messages/s each does ~7 500 writes/s, which costs ≲ 0.1 core with either library.

## Library behaviour discovered on the way

- **coder/websocket**
  - A `Read` whose context expires *closes the connection*, so no close code can follow. The 4408 handshake timeout therefore uses a timer plus `Close(4408)`, not a read deadline.
  - Expiring a `Write` context also closes the connection.
  - The client-side default read limit is 32 KiB. It must be raised to 64 KiB to receive full binary frames (§6.9).
  - `Close` runs the whole close handshake (5 s + 5 s bounds), and can be called while another goroutine is blocked in `Write`.
- **gorilla/websocket**
  - Has no context support: the adapter emulates it with deadlines and `context.AfterFunc`.
  - After a read timeout the connection is unusable. A write timeout is also fatal.
  - The close handshake is left to the application (`WriteControl` + wait + `Close`).
  - Concurrent writers panic, so one writer goroutine is mandatory. `WriteControl` and `Close` are the only concurrency-safe write calls.
- **Both**
  - Complete the handshake without a subprotocol, hence the 426 pre-check.
  - Send close codes 4xxx, and return the peer's code to the reader.
  - Enforce the read limit with 1009.
  - Have zero transitive dependencies (see `go.sum`).

## Caveats

- Everything runs over loopback in one process. Real networks, the gateway and browser buffering add latency on top of these numbers.
- The client is a Go program, not a browser.
- The fake DSP uses Go tickers. The fps adaptation acts by decimating per connection, and FFT `seq` is renumbered per connection, which costs one copy of each delivered FFT frame.
- "Audio backlog above cap for > 10 s" is implemented as "overflow drops with no drop-free gap ≥ 1 s for > 10 s", because the literal reading cannot happen with drop-oldest (see the ADR, spec issues).
