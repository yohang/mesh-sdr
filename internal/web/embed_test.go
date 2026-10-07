package web_test

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/yohang/mesh-sdr/internal/web"
)

// TestReceiverWorklet checks that the worklet file the CSP names exists
// among the static assets, and that it imports nothing: a worklet module
// carries no nonce, so an import would be blocked.
func TestReceiverWorklet(t *testing.T) {
	name := strings.TrimPrefix(web.ReceiverWorkletPath, "/static/")

	b, err := fs.ReadFile(web.Static(), name)
	if err != nil {
		t.Fatalf("worklet %s: %v", web.ReceiverWorkletPath, err)
	}

	for _, banned := range []string{"import ", "import(", "importScripts"} {
		if strings.Contains(string(b), banned) {
			t.Errorf("worklet contains %q", banned)
		}
	}
}
