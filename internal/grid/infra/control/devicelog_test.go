package control_test

import (
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/infra/control"
	"github.com/yohang/mesh-sdr/internal/grid/infra/pki"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	"github.com/yohang/mesh-sdr/internal/radio/infra/devlog"
	"github.com/yohang/mesh-sdr/internal/radio/infra/process"
)

// nextDeviceLog reads until a device.log message.
func nextDeviceLog(t *testing.T, read func() rxv1.Envelope) ctl.DeviceLog {
	t.Helper()

	for range 10 {
		env := read()
		if env.Type() != rxv1.TypeDeviceLog {
			continue
		}

		var l ctl.DeviceLog
		if err := env.DecodePayload(&l, true); err != nil {
			t.Fatal(err)
		}

		return l
	}

	t.Fatal("no device.log")

	return ctl.DeviceLog{}
}

// TestDeviceLogPush: the node sends its device log backlog when the
// channel opens, then pushes each new record (SRC-005).
func TestDeviceLogPush(t *testing.T) {
	logs := devlog.New([]string{"hf"}, 0, time.Now)
	logs.Connector("hf", process.Line{Time: time.Now(), Class: process.ClassInfo, Text: "before the channel"})

	n := startNodeWith(t, time.Second, 0, nil, func(o *control.NodeOptions) { o.Logs = logs })

	c, err := n.dial(t, pki.KindHub, "hub.example.org")
	if err != nil {
		t.Fatal(err)
	}

	sendEnv(t, c, rxv1.TypeCtlHello, "", ctl.Hello{HubID: "hub.example.org", HubVersion: "dev", Protocols: []string{"rx-ctl.v1"}, ServerTime: time.Now().UnixMilli()})

	readEnv := func() rxv1.Envelope { return read(t, c) }

	backlog := nextDeviceLog(t, readEnv)
	if backlog.DeviceID != "hf" || !backlog.Reset || len(backlog.Records) != 1 || backlog.Records[0].Text != "before the channel" {
		t.Fatalf("backlog = %+v", backlog)
	}

	tests := []struct{ device, text string }{
		{"hf", "live one"},
		{"hf", "live two"},
	}

	for _, tt := range tests {
		logs.State(tt.device, "running", tt.text)

		live := nextDeviceLog(t, readEnv)
		if live.Reset || len(live.Records) != 1 || live.Records[0].Text != "state running ("+tt.text+")" ||
			live.Records[0].Source != ctl.LogSourceDevice {
			t.Errorf("live = %+v", live)
		}
	}
}
