// Command wsbench runs the SPK-07 back-pressure scenarios against one server
// library and prints a markdown table row per scenario.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/yohang/mesh-sdr/spikes/spk-07-ws/session"
	"github.com/yohang/mesh-sdr/spikes/spk-07-ws/transport"
)

type scenario struct {
	name     string
	clients  int
	rate     int
	stallAt  time.Duration
	duration time.Duration
}

var scenarios = []scenario{
	{"fast", 1, 0, 0, 15 * time.Second},
	{"slow 200 kB/s", 1, 200_000, 0, 15 * time.Second},
	{"slow 64 kB/s", 1, 64_000, 0, 25 * time.Second},
	{"slow 16 kB/s", 1, 16_000, 0, 25 * time.Second},
	{"stall after 2 s", 1, 0, 2 * time.Second, 25 * time.Second},
	{"fan-out 100 × 64 kB/s", 100, 64_000, 0, 20 * time.Second},
}

var sockBuf int

func libByName(n string) transport.Lib {
	switch n {
	case "coder":
		return transport.Coder{}
	case "gorilla":
		return transport.Gorilla{}
	}
	fmt.Fprintln(os.Stderr, "unknown lib", n)
	os.Exit(2)
	return nil
}

func main() {
	serverLib := flag.String("server", "coder", "server library: coder|gorilla")
	clientLib := flag.String("client", "coder", "client library: coder|gorilla")
	only := flag.String("only", "", "run only scenarios whose name contains this")
	scale := flag.Float64("scale", 1, "duration multiplier")
	flag.IntVar(&sockBuf, "sockbuf", 16<<10, "server SO_SNDBUF and client SO_RCVBUF in bytes (0 = kernel default/autotuning)")
	footprint := flag.Int("footprint", 0, "if > 0: only measure the idle footprint of this many server connections")
	flag.Parse()
	if *footprint > 0 {
		measureFootprint(libByName(*serverLib), *footprint)
		return
	}

	srvLib, cliLib := libByName(*serverLib), libByName(*clientLib)
	fmt.Println("| server lib | scenario | audio recv/sent | audio lost (runs) | flagged runs | audio lat p50/p99/max ms | longest audio gap ms | FFT delivered | FFT dropped in queue | fps halvings → final fps | client close code | 4413 | peak queue KiB | peak heap MiB | alloc MiB | CPU s |")
	fmt.Println("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
	for _, sc := range scenarios {
		if *only != "" && !strings.Contains(sc.name, *only) {
			continue
		}
		sc.duration = time.Duration(float64(sc.duration) * *scale)
		run(srvLib, cliLib, sc)
	}
}

func cpu() time.Duration {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

func run(srvLib, cliLib transport.Lib, sc scenario) {
	runtime.GC()
	cfg := session.DefaultConfig()
	srv := session.NewServer(srvLib, cfg)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	hs := &http.Server{Handler: srv, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = hs.Serve(transport.SmallSendBufferListener{Listener: ln, SndBuf: sockBuf}) }()
	defer func() { _ = hs.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Run(ctx)

	var ms0 runtime.MemStats
	runtime.ReadMemStats(&ms0)
	cpu0 := cpu()
	sampDone := make(chan struct{})
	peakCh := make(chan uint64)
	go func() {
		var peakHeap uint64
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-sampDone:
				peakCh <- peakHeap
				return
			case <-t.C:
				var ms runtime.MemStats
				runtime.ReadMemStats(&ms)
				peakHeap = max(peakHeap, ms.HeapInuse)
			}
		}
	}()

	results := make([]session.ClientResult, sc.clients)
	var wg sync.WaitGroup
	for i := range sc.clients {
		wg.Go(func() {
			results[i] = session.RunClient(ctx, cliLib, session.ClientConfig{
				URL:       "ws://" + ln.Addr().String() + "/ws",
				RateBps:   sc.rate,
				StallAt:   sc.stallAt,
				Duration:  sc.duration,
				RcvBuf:    sockBuf,
				AudioStep: cfg.AudioFrame,
			})
		})
	}
	wg.Wait()
	cancel()
	time.Sleep(500 * time.Millisecond) // let sessions fold their stats
	close(sampDone)
	peakHeap := <-peakCh
	var ms1 runtime.MemStats
	runtime.ReadMemStats(&ms1)
	cpuUsed := cpu() - cpu0

	var aRecv, aMissing, aRuns, aFlagged, fRecv, halv, lastFPS int
	var lat []float64
	var longest float64
	codes := map[int]int{}
	for _, r := range results {
		aRecv += r.Audio.Received
		aMissing += r.Audio.Missing
		aRuns += r.Audio.GapEvents
		aFlagged += r.Audio.FlaggedGaps
		fRecv += r.FFT.Received
		lat = append(lat, r.Audio.LatenciesMS...)
		longest = max(longest, r.Audio.LongestGapMS)
		halv += r.StreamUpdates
		if r.LastFPS > 0 {
			lastFPS = r.LastFPS
		}
		if r.Err != nil {
			codes[r.CloseCode]++
		}
	}
	st := srv.Stats
	fftSent := st.FFTEnqueued.Load()
	fftPct := 0.0
	dropPct := 0.0
	if fftSent > 0 {
		fftPct = 100 * float64(fRecv) / float64(fftSent)
		dropPct = 100 * float64(st.FFTDropped.Load()) / float64(fftSent)
	}
	codeStr := "—"
	if len(codes) > 0 {
		var parts []string
		for c, n := range codes {
			if c == -1 {
				parts = append(parts, fmt.Sprintf("none/EOF ×%d", n))
			} else {
				parts = append(parts, fmt.Sprintf("%d ×%d", c, n))
			}
		}
		codeStr = strings.Join(parts, ", ")
	}
	fps := "—"
	if halv > 0 {
		fps = fmt.Sprintf("%d → %d", st.FPSHalvings.Load(), lastFPS)
	}
	fmt.Printf("| %s | %s | %d/%d | %d (%d) | %d/%d | %.0f/%.0f/%.0f | %.0f | %.1f %% | %.1f %% | %s | %s | %d | %d | %.1f | %.0f | %.1f |\n",
		srvLib.Name(), sc.name,
		aRecv, st.AudioEnqueued.Load(),
		aMissing, aRuns, aFlagged, aRuns,
		session.Percentile(lat, 50), session.Percentile(lat, 99), session.Percentile(lat, 100),
		longest,
		fftPct, dropPct, fps, codeStr, st.Closed4413.Load(),
		st.PeakQueueBytes.Load()/1024,
		float64(peakHeap)/(1<<20),
		float64(ms1.TotalAlloc-ms0.TotalAlloc)/(1<<20),
		cpuUsed.Seconds(),
	)
}

// measureFootprint opens n connections with a raw client and keeps one
// blocked Read per server connection (as a real session does), then reports
// heap and goroutines per connection attributable to the server side.
func measureFootprint(lib transport.Lib, n int) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	accepted := make(chan transport.Conn, n)
	hs := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := lib.Accept(w, r, 16<<10)
		if err != nil {
			return
		}
		accepted <- c
		_, _, _ = c.Read(context.Background()) // park like a real reader
	})}
	go func() { _ = hs.Serve(ln) }()
	defer func() { _ = hs.Close() }()

	clients := make([]net.Conn, 0, n)
	runtime.GC()
	var ms0 runtime.MemStats
	runtime.ReadMemStats(&ms0)
	g0 := runtime.NumGoroutine()
	for range n {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			panic(err)
		}
		_, _ = fmt.Fprintf(c, "GET /ws HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
			"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Protocol: rx.v1\r\n\r\n")
		buf := make([]byte, 512)
		_, _ = c.Read(buf) // 101 response
		clients = append(clients, c)
		<-accepted
	}
	time.Sleep(200 * time.Millisecond)
	runtime.GC()
	var ms1 runtime.MemStats
	runtime.ReadMemStats(&ms1)
	g1 := runtime.NumGoroutine()
	// The raw client side holds only a net.Conn and a 512-byte buffer per
	// connection, so the delta is dominated by the server.
	fmt.Printf("| %s | %d | %.1f KiB | %.2f |\n", lib.Name(), n,
		float64(int64(ms1.HeapInuse)-int64(ms0.HeapInuse))/float64(n)/1024,
		float64(g1-g0)/float64(n))
	for _, c := range clients {
		_ = c.Close()
	}
}
