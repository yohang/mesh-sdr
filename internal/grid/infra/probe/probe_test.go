package probe

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProberReadsProc(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"proc/meminfo":                         "MemTotal:        4000 kB\nMemAvailable:    1000 kB\n",
		"proc/loadavg":                         "0.50 0.40 0.30 1/100 12345\n",
		"proc/stat":                            "cpu  100 0 100 800 0 0 0 0 0 0\n",
		"proc/cpuinfo":                         "processor : 0\nmodel name : Test CPU\n",
		"sys/class/thermal/thermal_zone0/temp": "45500\n",
	}

	for name, content := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	p := New("1.2.3", nil, []string{"am", "nfm"}, time.Now().Add(-time.Minute))
	p.root = root + "/"

	caps := p.Capabilities(context.Background())
	if caps.ProductVersion != "1.2.3" || caps.Platform.RAMBytes != 4000*1024 || caps.Platform.CPUModel != "Test CPU" || caps.Devices == nil {
		t.Errorf("capabilities = %+v", caps)
	}

	if len(caps.Decoders) != 1 || caps.Decoders[0].Cap != AnalogCap || len(caps.Decoders[0].Modes) != 2 {
		t.Errorf("analog modes = %+v", caps.Decoders)
	}

	hb := p.Heartbeat(context.Background())
	if hb.Load[0] != 0.5 || hb.Mem.AvailableBytes != 1000*1024 || hb.TempC == nil || *hb.TempC != 45.5 || hb.UptimeS < 59 {
		t.Errorf("heartbeat = %+v", hb)
	}

	// The CPU ratio is computed between two samples.
	if err := os.WriteFile(filepath.Join(root, "proc/stat"), []byte("cpu  150 0 150 900 0 0 0 0 0 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := p.Heartbeat(context.Background()).CPU; got != 0.5 {
		t.Errorf("cpu = %v, want 0.5", got)
	}
}
