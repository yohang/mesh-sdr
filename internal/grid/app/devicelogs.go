package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// DeviceLogSize is the number of records kept per device, like the node
// (SRC-005).
const DeviceLogSize = 200

// LogRecord is one record of a device log: plain text, never HTML.
type LogRecord struct {
	Time time.Time
	// Source is "connector" (a stderr line of the connector) or "device"
	// (a lifecycle record).
	Source string
	// Class is the stderr class, or the state of a lifecycle record.
	Class string
	Text  string
}

// DeviceLogListener is told about the records of a device; reset is set
// when they replace the previous ones (a node backlog).
type DeviceLogListener func(ctx context.Context, device shared.DeviceID, reset bool, records []LogRecord)

// DeviceLogs holds in RAM the device logs the nodes push over their
// control channel (SRC-005, device.log): the last DeviceLogSize records of
// each device. They are ephemeral diagnostics: nothing is persisted, a
// node sends its backlog again when its channel opens.
type DeviceLogs struct {
	repo   domain.DeviceRepository
	logger *slog.Logger

	mu        sync.Mutex
	logs      map[shared.DeviceID][]LogRecord
	listeners []DeviceLogListener
}

// NewDeviceLogs returns the service.
func NewDeviceLogs(repo domain.DeviceRepository, logger *slog.Logger) *DeviceLogs {
	return &DeviceLogs{repo: repo, logger: logger, logs: map[shared.DeviceID][]LogRecord{}}
}

// OnRecords registers a listener (composition time only).
func (s *DeviceLogs) OnRecords(f DeviceLogListener) { s.listeners = append(s.listeners, f) }

// Receive records a device.log message of node. Records of a device the
// node does not own are dropped.
func (s *DeviceLogs) Receive(ctx context.Context, node domain.NodeID, m ctl.DeviceLog) {
	id, err := shared.NewDeviceID(m.DeviceID)
	if err != nil {
		s.logger.WarnContext(ctx, "device.log with an invalid device id dropped", slog.String("node_id", node.String()))

		return
	}

	d, err := s.repo.Get(ctx, id)

	switch {
	case errors.Is(err, domain.ErrDeviceNotFound):
		s.logger.DebugContext(ctx, "device.log of an unknown device dropped", slog.String("device_id", id.String()))

		return
	case err != nil:
		s.logger.ErrorContext(ctx, "read device for device.log", slog.String("device_id", id.String()), slog.Any("error", err))

		return
	case d.Node() != node:
		s.logger.WarnContext(ctx, "device.log for another node's device dropped", slog.String("node_id", node.String()),
			slog.String("device_id", id.String()))

		return
	}

	records := make([]LogRecord, 0, min(len(m.Records), DeviceLogSize))

	for _, r := range m.Records[max(0, len(m.Records)-DeviceLogSize):] {
		if r.Source != ctl.LogSourceConnector && r.Source != ctl.LogSourceDevice {
			continue
		}

		records = append(records, LogRecord{
			Time: time.UnixMilli(r.Time).UTC(), Source: r.Source, Class: plainText(r.Class, 32), Text: plainText(r.Text, ctl.MaxLogText),
		})
	}

	s.mu.Lock()
	all := records
	if !m.Reset {
		all = append(s.logs[id], records...)
	}

	s.logs[id] = append([]LogRecord(nil), all[max(0, len(all)-DeviceLogSize):]...)
	s.mu.Unlock()

	if len(records) == 0 && !m.Reset {
		return
	}

	for _, f := range s.listeners {
		f(ctx, id, m.Reset, records)
	}
}

// Records returns the records of a device, oldest first.
func (s *DeviceLogs) Records(id shared.DeviceID) []LogRecord {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]LogRecord(nil), s.logs[id]...)
}

// Forget drops the log of a forgotten device.
func (s *DeviceLogs) Forget(_ context.Context, d *domain.Device) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.logs, d.ID())
}

// plainText keeps reported text plain: valid UTF-8, no control character
// other than tab, at most maxBytes bytes.
func plainText(s string, maxBytes int) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.Map(func(r rune) rune {
		if r == '\t' || !unicode.IsControl(r) {
			return r
		}

		return -1
	}, s)

	if len(s) <= maxBytes {
		return s
	}

	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}

	return s[:cut]
}
