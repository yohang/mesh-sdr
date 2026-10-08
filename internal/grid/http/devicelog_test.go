package http

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/app"
	"github.com/yohang/mesh-sdr/internal/grid/domain"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// TestDeviceLogRender: the records are rendered as escaped text, oldest
// first, with their time and origin.
func TestDeviceLogRender(t *testing.T) {
	t0 := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	d, err := domain.NewReportedDevice(domain.MustNodeID("attic"), domain.DeviceSpec{
		ID: shared.MustDeviceID("hf"), Name: "HF", Type: "rtl_sdr", Enabled: true, FreqMin: 1, FreqMax: 2, SampleRates: []int64{1},
	}, 0, t0)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		records []app.LogRecord
		want    []string
		not     []string
	}{
		{
			name: "empty",
			want: []string{"No record yet", `data-msdr-topics="admin.device_log:device=hf"`, `hx-get="/admin/devices/hf/log"`},
		},
		{
			name: "records",
			records: []app.LogRecord{
				{Time: t0, Source: "device", Class: "starting", Text: "state starting"},
				{Time: t0.Add(time.Second), Source: "connector", Class: "device_lost", Text: `<script>alert("x")</script>`},
			},
			want: []string{
				`<time datetime="2026-10-08T12:00:00.000Z">2026-10-08 12:00:00 UTC</time>`, "[device]</span> state starting",
				"[connector device_lost]", "&lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt;", "data-device-log-empty hidden",
			},
			not: []string{"<script>alert"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b strings.Builder
			if err := deviceLogPage(deviceLogView{Device: d, Records: tt.records}).Render(context.Background(), &b); err != nil {
				t.Fatal(err)
			}

			html := b.String()

			for _, w := range tt.want {
				if !strings.Contains(html, w) {
					t.Errorf("page lacks %s:\n%s", w, html)
				}
			}

			for _, n := range tt.not {
				if strings.Contains(html, n) {
					t.Errorf("page has %s", n)
				}
			}

			if tt.records != nil && strings.Index(html, "state starting") > strings.Index(html, "device_lost") {
				t.Error("records not oldest first")
			}
		})
	}
}
