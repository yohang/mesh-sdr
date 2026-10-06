// Command spike runs the SPK-03 scenarios: embedded Caddy gateway in front of a
// fake hub and fake mTLS nodes, and prints measurements as Markdown.
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/coder/websocket"
	"github.com/golang-jwt/jwt/v5"

	"github.com/yohang/mesh-sdr/spikes/spk-03-caddy/fakehub"
	"github.com/yohang/mesh-sdr/spikes/spk-03-caddy/gateway"
)

var drivers = []gateway.Driver{gateway.DriverLoad, gateway.DriverAdminAPI, gateway.DriverDynamic}

func main() {
	level := slog.LevelWarn
	if os.Getenv("SPIKE_DEBUG") != "" {
		level = slog.LevelDebug
	}
	counts := map[string]int{}
	mu := &sync.Mutex{}
	sample := &atomic.Value{}
	h := countingHandler{slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}), level, mu, counts, sample}
	log := slog.New(h)

	base := os.Getenv("SPIKE_TMP")
	if base == "" {
		base = os.TempDir()
	}
	e, err := newEnv(base, log, counts, mu, sample)
	if err != nil {
		fail(err)
	}
	defer e.close()

	var ms runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&ms)
	heap0, gor0 := ms.HeapAlloc, runtime.NumGoroutine()

	fmt.Println("# SPK-03 measurements")
	fmt.Printf("\nCaddy %s, Go %s, GOMAXPROCS=%d\n", caddyVersion(), runtime.Version(), runtime.GOMAXPROCS(0))

	functional(e)

	// memory footprint of an idle gateway (files TLS, 2 node routes)
	must(e.startGateway(gateway.DriverLoad, gateway.TLSFiles, 0, "roof", "shack"))
	time.Sleep(300 * time.Millisecond)
	runtime.GC()
	runtime.ReadMemStats(&ms)
	fmt.Printf("\n## Idle footprint\n\nHeap delta after caddy.Load: %.1f MiB, goroutines +%d\n",
		float64(ms.HeapAlloc-heap0)/(1<<20), runtime.NumGoroutine()-gor0)
	e.stopGateway()

	survival(e)
	storm(e)
	latency(e)
	tlsModes(e)
	logging(e)
	dumpExamples()
}

// dumpExamples writes reference configs with production-like paths to $SPIKE_OUT.
func dumpExamples() {
	out := os.Getenv("SPIKE_OUT")
	if out == "" {
		return
	}
	base := gateway.Options{
		Listen: ":443", HTTPSPort: 443, Domain: "sdr.example.org",
		ACMEEmail: "ops@example.org", ACMECA: "https://acme-v02.api.letsencrypt.org/directory",
		OperatorCert: "/etc/meshsdr/tls/public.crt", OperatorKey: "/etc/meshsdr/tls/public.key",
		StorageDir: "/data/caddy", HubHandlerName: "hub", HubAuthzDial: "unix//run/meshsdr/hub-authz.sock",
		NodeLookupName: "nodes", LoggerName: "log",
		CAFile: "/etc/meshsdr/tls/hub-ca.pem", GatewayCert: "/etc/meshsdr/tls/gateway.crt", GatewayKey: "/etc/meshsdr/tls/gateway.key",
		StreamCloseDelay: 2 * time.Hour, StreamTimeout: 24 * time.Hour,
	}
	nodes := []gateway.Node{{ID: "n-roof", Addr: "10.8.0.12:7443"}, {ID: "n-shack", Addr: "10.8.0.13:7443"}}
	per := base
	per.Routing, per.TLS, per.AdminSocket = gateway.RoutingPerNode, gateway.TLSFiles, "/run/meshsdr/caddy-admin.sock"
	dyn := base
	dyn.Routing, dyn.TLS = gateway.RoutingDynamic, gateway.TLSACME
	for name, o := range map[string]gateway.Options{"caddy-per-node-files.json": per, "caddy-dynamic-acme.json": dyn} {
		b, err := o.Config(nodes)
		must(err)
		must(os.WriteFile(out+"/"+name, append(b, '\n'), 0o644))
	}
}

func caddyVersion() string {
	_, full := caddy.Version()
	return full
}

func must(err error) {
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "FATAL:", err)
	os.Exit(1)
}

func ok(b bool) string {
	if b {
		return "PASS"
	}
	return "**FAIL**"
}

// ---------------------------------------------------------------------------
// 1, 2, 4: dynamic routes, WS + mTLS, forward auth + token injection

func functional(e *env) {
	fmt.Println("\n## Functional checks (TLS=operator files)")
	fmt.Println("\n| Check | " + strings.Join(names(drivers), " | ") + " |")
	fmt.Println("|---|" + strings.Repeat("---|", len(drivers)))
	type row struct {
		name string
		res  []string
	}
	var rows []*row
	get := func(name string) *row {
		for _, r := range rows {
			if r.name == name {
				return r
			}
		}
		r := &row{name: name}
		rows = append(rows, r)
		return r
	}
	ctx := context.Background()
	for _, d := range drivers {
		must(e.startGateway(d, gateway.TLSFiles, 0, "roof"))

		c, hello, st, err := e.br.dial(ctx, "roof", dialOpts{forged: true})
		good := err == nil && echo(ctx, c, "hi") == nil
		get("WS upgrade + echo through gateway, mTLS to node").res = append(get("WS upgrade + echo through gateway, mTLS to node").res, ok(good))
		get("Node saw gateway URI SAN client cert").res = append(get("Node saw gateway URI SAN client cert").res, ok(hello.PeerURISAN == "urn:rx:gateway:hub1"))
		get("Hub-minted token injected, verified offline by node (EdDSA)").res = append(get("Hub-minted token injected, verified offline by node (EdDSA)").res, ok(good && hello.Sub == "u-alice" && hello.Cid != ""))
		get("Client-forged X-Rx-Access-Token / X-Rx-Node-Id stripped").res = append(get("Client-forged X-Rx-Access-Token / X-Rx-Node-Id stripped").res, ok(good && !hello.ForgedTokenIn && hello.HeaderNodeID == "roof"))
		if c != nil {
			_ = c.CloseNow()
		}

		_, _, st, _ = e.br.dial(ctx, "roof", dialOpts{noCookie: true})
		get("No session -> authz 401 passed through, no upgrade").res = append(get("No session -> authz 401 passed through, no upgrade").res, fmt.Sprintf("%s (%d)", ok(st == 401), st))

		_, _, st, _ = e.br.dial(ctx, "shack", dialOpts{})
		get("Not-yet-enrolled node shack -> 404").res = append(get("Not-yet-enrolled node shack -> 404").res, fmt.Sprintf("%s (%d)", ok(st == 404), st))
		_, _, st, _ = e.br.dial(ctx, "zz-unknown", dialOpts{})
		get("Unknown node id -> 404").res = append(get("Unknown node id -> 404").res, fmt.Sprintf("%s (%d)", ok(st == 404), st))

		addLat, err := e.addNode("shack")
		must(err)
		t0 := time.Now()
		var ready time.Duration = -1
		for time.Since(t0) < 3*time.Second {
			if c, _, _, err := e.br.dial(ctx, "shack", dialOpts{}); err == nil {
				ready = time.Since(t0)
				_ = c.CloseNow()
				break
			}
		}
		get("Add node shack at runtime: apply call duration").res = append(get("Add node shack at runtime: apply call duration").res, fmtDur(addLat))
		get("node shack reachable after apply returned").res = append(get("node shack reachable after apply returned").res, fmt.Sprintf("%s (+%s)", ok(ready >= 0), fmtDur(ready)))

		// node offline: route kept, authz answers 503 (§4.6 rule 2)
		e.reg.Put(fakehub.NodeEntry{ID: "shack", Addr: e.nodes["shack"].Addr, Online: false, Enabled: true})
		_, _, st, _ = e.br.dial(ctx, "shack", dialOpts{})
		get("node shack offline (route kept) -> 503 from authz").res = append(get("node shack offline (route kept) -> 503 from authz").res, fmt.Sprintf("%s (%d)", ok(st == 503), st))

		rmLat, err := e.removeNode("shack")
		must(err)
		_, _, st, _ = e.br.dial(ctx, "shack", dialOpts{})
		get("Remove node shack at runtime: apply call duration").res = append(get("Remove node shack at runtime: apply call duration").res, fmtDur(rmLat))
		get("Removed node shack -> 404").res = append(get("Removed node shack -> 404").res, fmt.Sprintf("%s (%d)", ok(st == 404), st))

		resp, err := e.br.httpClient().Get("https://" + domain + "/api/v1/ping")
		body := ""
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body = string(b)
		}
		get("Hub API served in-process by meshsdr_hub handler").res = append(get("Hub API served in-process by meshsdr_hub handler").res, ok(body == "pong"))
		resp, err = e.br.httpClient().Get("https://" + domain + "/nope")
		st = 0
		if err == nil {
			st = resp.StatusCode
			resp.Body.Close()
		}
		get("Other path -> 404").res = append(get("Other path -> 404").res, fmt.Sprintf("%s (%d)", ok(st == 404), st))
		e.stopGateway()
	}
	for _, r := range rows {
		fmt.Printf("| %s | %s |\n", r.name, strings.Join(r.res, " | "))
	}
}

// ---------------------------------------------------------------------------
// 3: effect of a route change on already-open WebSockets

func survival(e *env) {
	fmt.Println("\n## Open WebSockets across route changes")
	fmt.Println("\nTwo WS (to roof and shack) open and echoing every 10 ms. Event 1: add unrelated node attic. Event 2: remove node shack. Observation window: close delay + 2 s.")
	fmt.Println("\n| Driver | stream_close_delay | WS roof after add attic | WS shack after add attic | WS roof after remove shack | WS shack after remove shack |")
	fmt.Println("|---|---|---|---|---|---|")
	ctx := context.Background()
	type cse struct {
		d     gateway.Driver
		delay time.Duration
	}
	cases := []cse{
		{gateway.DriverLoad, 0}, {gateway.DriverLoad, 3 * time.Second},
		{gateway.DriverAdminAPI, 0}, {gateway.DriverAdminAPI, 3 * time.Second},
		{gateway.DriverDynamic, 0},
	}
	for _, cs := range cases {
		must(e.startGateway(cs.d, gateway.TLSFiles, cs.delay, "roof", "shack"))
		window := cs.delay + 2*time.Second
		open := func() (*pinger, *pinger) {
			ca, _, _, err := e.br.dial(ctx, "roof", dialOpts{})
			must(err)
			cb, _, _, err := e.br.dial(ctx, "shack", dialOpts{})
			must(err)
			return startPinger(ca), startPinger(cb)
		}
		pa, pb := open()
		time.Sleep(200 * time.Millisecond)
		t0 := time.Now()
		_, err := e.addNode("attic")
		must(err)
		time.Sleep(window)
		r1a, r1b := fate(pa, t0), fate(pb, t0)

		// fresh sockets for event 2
		pa, pb = open()
		time.Sleep(200 * time.Millisecond)
		t0 = time.Now()
		_, err = e.removeNode("shack")
		must(err)
		time.Sleep(window)
		r2a, r2b := fate(pa, t0), fate(pb, t0)
		fmt.Printf("| %s | %s | %s | %s | %s | %s |\n", cs.d, fmtDur(cs.delay), r1a, r1b, r2a, r2b)
		e.stopGateway()
	}
}

func fate(p *pinger, t0 time.Time) string {
	if p.alive() {
		return "survived"
	}
	return "closed after " + fmtDur(p.diedAfter(t0).Round(10*time.Millisecond))
}

// ---------------------------------------------------------------------------
// reload storm: are new requests/WS refused while Caddy reloads?

func storm(e *env) {
	fmt.Println("\n## Reload storm (40 route changes, one every 25 ms, traffic in parallel)")
	fmt.Println("\nControl row: same traffic for the same duration with no route change.")
	fmt.Println("\n| Driver | apply p50 | apply max | HTTP GETs ok/total | WS dials ok/total | errors seen |")
	fmt.Println("|---|---|---|---|---|---|")
	ctx := context.Background()
	for _, d := range []gateway.Driver{"control", gateway.DriverLoad, gateway.DriverAdminAPI} {
		drv := d
		if d == "control" {
			drv = gateway.DriverLoad
		}
		must(e.startGateway(drv, gateway.TLSFiles, time.Hour, "roof"))
		stop := make(chan struct{})
		var httpOK, httpAll, wsOK, wsAll atomic.Int64
		var errMu sync.Mutex
		errs := map[string]int{}
		record := func(err error, st int) {
			errMu.Lock()
			defer errMu.Unlock()
			k := fmt.Sprintf("status %d", st)
			if err != nil {
				k = err.Error()
				if i := strings.LastIndex(k, ": "); i > 0 {
					k = k[i+2:]
				}
			}
			errs[k]++
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			hc := e.br.httpClient()
			for {
				select {
				case <-stop:
					return
				default:
				}
				httpAll.Add(1)
				resp, err := hc.Get("https://" + domain + "/api/v1/ping")
				if err != nil {
					record(err, 0)
					continue
				}
				if resp.StatusCode == 200 {
					httpOK.Add(1)
				} else {
					record(nil, resp.StatusCode)
				}
				resp.Body.Close()
			}
		}()
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				wsAll.Add(1)
				c, _, st, err := e.br.dial(ctx, "roof", dialOpts{})
				if err != nil {
					record(err, st)
					continue
				}
				wsOK.Add(1)
				_ = c.CloseNow()
			}
		}()
		var lats []time.Duration
		for i := 0; i < 20; i++ {
			for _, add := range []bool{true, false} {
				time.Sleep(25 * time.Millisecond)
				if d == "control" {
					continue
				}
				var l time.Duration
				var err error
				if add {
					l, err = e.addNode("attic")
				} else {
					l, err = e.removeNode("attic")
				}
				must(err)
				lats = append(lats, l)
			}
		}
		close(stop)
		wg.Wait()
		p50, mx := "-", "-"
		if len(lats) > 0 {
			a, _, b := pct(lats)
			p50, mx = fmtDur(a), fmtDur(b)
		}
		var es []string
		for k, v := range errs {
			es = append(es, fmt.Sprintf("%dx `%s`", v, k))
		}
		sort.Strings(es)
		fmt.Printf("| %s | %s | %s | %d/%d | %d/%d | %s |\n", d, p50, mx, httpOK.Load(), httpAll.Load(), wsOK.Load(), wsAll.Load(), strings.Join(es, "; "))
		e.stopGateway()
	}
}

// ---------------------------------------------------------------------------
// latency: WS connect and echo RTT, via gateway (forward auth + mTLS) vs direct

func latency(e *env) {
	fmt.Println("\n## Latency (loopback, N=200 connects, N=1000 echoes)")
	fmt.Println("\n| Path | connect p50 | connect p95 | echo RTT p50 | echo RTT p95 |")
	fmt.Println("|---|---|---|---|---|")
	ctx := context.Background()
	measure := func(label string, dial func() (*websocket.Conn, error)) {
		var conns []time.Duration
		for i := 0; i < 200; i++ {
			t0 := time.Now()
			c, err := dial()
			must(err)
			conns = append(conns, time.Since(t0))
			_ = c.CloseNow()
		}
		c, err := dial()
		must(err)
		var rtts []time.Duration
		for i := 0; i < 1000; i++ {
			t0 := time.Now()
			must(echo(ctx, c, "x"))
			rtts = append(rtts, time.Since(t0))
		}
		_ = c.CloseNow()
		cp50, cp95, _ := pct(conns)
		ep50, ep95, _ := pct(rtts)
		fmt.Printf("| %s | %s | %s | %s | %s |\n", label, fmtDur(cp50), fmtDur(cp95), fmtDur(ep50), fmtDur(ep95))
	}
	for _, d := range []gateway.Driver{gateway.DriverLoad, gateway.DriverDynamic} {
		must(e.startGateway(d, gateway.TLSFiles, 0, "roof"))
		measure("browser -> gateway ("+string(d)+") -> node roof", func() (*websocket.Conn, error) {
			c, _, _, err := e.br.dial(ctx, "roof", dialOpts{})
			return c, err
		})
		e.stopGateway()
	}
	// direct: same mTLS client cert, token minted through the hub authz endpoint
	e.reg.Put(fakehub.NodeEntry{ID: "roof", Addr: e.nodes["roof"].Addr, Online: true, Enabled: true})
	hc := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: e.ca.Pool, Certificates: []tls.Certificate{e.gwLeaf.TLS}, ServerName: "roof.nodes.rx.internal", NextProtos: []string{"http/1.1"},
	}, DisableKeepAlives: true}}
	tok := mintDirect(e)
	measure("direct mTLS -> node roof (no gateway, no authz)", func() (*websocket.Conn, error) {
		h := http.Header{"X-Rx-Access-Token": {tok}}
		c, _, _, err := wsDial(ctx, "wss://"+e.nodes["roof"].Addr+"/ws", hc, h)
		return c, err
	})
	e.reg.Delete("roof")
}

func mintDirect(e *env) string {
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (netConn, error) {
		return dialUnix(ctx, e.hubSock)
	}}}
	req, _ := http.NewRequest("GET", "http://hub/internal/gateway/authz?node=roof", nil)
	req.Header.Set("Cookie", "session=valid")
	resp, err := hc.Do(req)
	must(err)
	resp.Body.Close()
	t := resp.Header.Get("X-Rx-Access-Token")
	if _, _, err := jwt.NewParser().ParseUnverified(t, jwt.MapClaims{}); err != nil {
		fail(err)
	}
	return t
}

// ---------------------------------------------------------------------------
// 5: TLS modes

func tlsModes(e *env) {
	fmt.Println("\n## TLS modes (browser-facing listener)")
	fmt.Println("\n| Mode | Result |")
	fmt.Println("|---|---|")
	ctx := context.Background()

	// operator-provided files: covered by every scenario above
	fmt.Println("| operator cert/key files (`tls.certificates.load_files`) | PASS (used by all scenarios above) |")

	// internal issuer: same automation path as ACME (certmagic), local CA instead of an ACME CA
	t0 := time.Now()
	must(e.startGateway(gateway.DriverLoad, gateway.TLSInternal, 0, "roof"))
	var res string
	for time.Since(t0) < 15*time.Second {
		c, _, _, err := e.br.dial(ctx, "roof", dialOpts{})
		if err == nil {
			_ = c.CloseNow()
			res = fmt.Sprintf("PASS, first successful TLS WS %s after Load", fmtDur(time.Since(t0)))
			break
		}
		res = "**FAIL** " + err.Error()
		time.Sleep(10 * time.Millisecond)
	}
	fmt.Printf("| managed cert, internal issuer (`automate` + `issuers:[internal]`) | %s |\n", res)
	e.stopGateway()

	// ACME: config validated only (no network)
	o := e.opts
	o.TLS, o.Domain, o.ACMEEmail, o.ACMECA = gateway.TLSACME, "sdr.example.org", "ops@example.org", "https://acme-staging-v02.api.letsencrypt.org/directory"
	cfg, err := o.Config([]gateway.Node{{ID: "roof", Addr: e.nodes["roof"].Addr}})
	must(err)
	// "@id" keys are only understood by the admin/Load path (they are indexed then
	// stripped); caddy.Validate needs them removed first.
	var c caddy.Config
	must(jsonUnmarshal(stripIDs(cfg), &c))
	err = caddy.Validate(&c)
	fmt.Printf("| ACME (`issuers:[acme]`, staging CA) | %s |\n", map[bool]string{true: "config validates (caddy.Validate), not issued: no public DNS in spike", false: "**FAIL** " + fmt.Sprint(err)}[err == nil])
}

// ---------------------------------------------------------------------------
// 6 (logging part)

func logging(e *env) {
	fmt.Println("\n## Logging bridge (zap -> slog)")
	e.countMu.Lock()
	defer e.countMu.Unlock()
	var keys []string
	for k := range e.counts {
		if strings.HasPrefix(k, "gateway.caddy") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	fmt.Println("\n| slog component | records |")
	fmt.Println("|---|---|")
	for _, k := range keys {
		fmt.Printf("| %s | %d |\n", k, e.counts[k])
	}
	if s := e.sample.Load(); s != nil {
		fmt.Printf("\nSample: `%s`\n", s)
	}
}

// ---------------------------------------------------------------------------

func names(ds []gateway.Driver) []string {
	var s []string
	for _, d := range ds {
		s = append(s, string(d))
	}
	return s
}

func pct(ds []time.Duration) (p50, p95, mx time.Duration) {
	s := slices.Clone(ds)
	slices.Sort(s)
	return s[len(s)/2], s[len(s)*95/100], s[len(s)-1]
}

func fmtDur(d time.Duration) string {
	switch {
	case d < 0:
		return "n/a"
	case d == 0:
		return "0"
	case d < time.Millisecond:
		return fmt.Sprintf("%dµs", d.Microseconds())
	case d < time.Second:
		return fmt.Sprintf("%.1fms", float64(d.Microseconds())/1000)
	default:
		return d.Round(10 * time.Millisecond).String()
	}
}

