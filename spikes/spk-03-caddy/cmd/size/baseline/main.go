// Baseline for binary size: net/http + crypto/tls + slog, no Caddy.
package main

import (
	"crypto/tls"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
)

func main() {
	u, _ := url.Parse("https://127.0.0.1:1")
	rp := httputil.NewSingleHostReverseProxy(u)
	rp.Transport = &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13}}
	srv := &http.Server{Addr: os.Getenv("ADDR"), Handler: rp}
	slog.Info("start")
	if os.Getenv("ADDR") != "" {
		_ = srv.ListenAndServeTLS("c", "k")
	}
}
