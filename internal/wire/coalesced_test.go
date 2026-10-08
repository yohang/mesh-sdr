package wire

import (
	"sync/atomic"
	"testing"
	"time"
)

// Triggers during a run make one more run, not one each (re-probes of
// several decoders losing their tool).
func TestCoalesced(t *testing.T) {
	var runs atomic.Int32

	release := make(chan struct{})
	c := &coalesced{run: func() {
		runs.Add(1)
		<-release
	}}

	c.trigger()

	eventually(t, "first run", time.Second, func() bool { return runs.Load() == 1 })

	for range 5 {
		c.trigger()
	}

	release <- struct{}{}
	release <- struct{}{}

	eventually(t, "idle", time.Second, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()

		return !c.running
	})

	if n := runs.Load(); n != 2 {
		t.Errorf("runs = %d, want 2", n)
	}
}
