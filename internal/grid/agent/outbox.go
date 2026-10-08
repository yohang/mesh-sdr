package agent

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1/ctl"
	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// File kinds a node sends to the hub (FIL-005).
const (
	FileKindSSTV    = "sstv"
	FileKindFAX     = "fax"
	FileKindTextLog = "text_log"
)

// Size caps of a file (FIL-005): 8 MiB, 16 MiB for FAX. MaxOutboxBytes
// bounds the files waiting in the outbox for the hub's acknowledgement.
const (
	MaxFileBytes   = 8 << 20
	MaxFAXBytes    = 16 << 20
	MaxOutboxBytes = 48 << 20
)

// fileMIME is the media type of each kind: PNG images and plain UTF-8
// text.
var fileMIME = map[string]string{FileKindSSTV: "image/png", FileKindFAX: "image/png", FileKindTextLog: "text/plain"}

// Errors of SendFile.
var (
	ErrInvalidFile = errors.New("invalid file")
	ErrFileTooBig  = errors.New("file too large")
	ErrOutboxFull  = errors.New("file outbox full: the hub has not acknowledged the previous files yet")
)

var modePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,23}$`)

// FileMeta describes a file a decoder produced (FIL-008): the values are
// stamped from the node clock when the reception started.
type FileMeta struct {
	// Kind is FileKindSSTV, FileKindFAX (PNG images) or FileKindTextLog
	// (plain UTF-8 text).
	Kind     string
	DeviceID string
	// PresetID and DecoderSessionID are UUIDs, when known.
	PresetID         string
	DecoderSessionID string
	// Mode is the decoder mode (sstv, fax, cw…).
	Mode string
	// FrequencyHz is the dial frequency the decoder was tuned to.
	FrequencyHz   int64
	ReceivedStart time.Time
	// ReceivedEnd is zero when the reception was cut short.
	ReceivedEnd time.Time
	// Metadata are the per-kind details (SSTV mode, VIS code, FAX LPM…).
	Metadata map[string]any
}

// MaxSize returns the size cap of the kind.
func (m FileMeta) MaxSize() int64 {
	if m.Kind == FileKindFAX {
		return MaxFAXBytes
	}

	return MaxFileBytes
}

func (m FileMeta) validate() error {
	_, kind := fileMIME[m.Kind]

	switch {
	case !kind:
		return fmt.Errorf("%w: unsupported kind %q", ErrInvalidFile, m.Kind)
	case !modePattern.MatchString(m.Mode):
		return fmt.Errorf("%w: invalid mode %q", ErrInvalidFile, m.Mode)
	case m.FrequencyHz <= 0, m.ReceivedStart.IsZero():
		return fmt.Errorf("%w: the reception frequency and start are required", ErrInvalidFile)
	case !m.ReceivedEnd.IsZero() && m.ReceivedEnd.Before(m.ReceivedStart):
		return fmt.Errorf("%w: the reception ends before it starts", ErrInvalidFile)
	}

	if _, err := shared.NewDeviceID(m.DeviceID); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidFile, err)
	}

	for _, id := range []string{m.PresetID, m.DecoderSessionID} {
		if _, err := shared.ParseUUID(id); id != "" && err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidFile, err)
		}
	}

	return nil
}

// Outbox sends the files the decoders produce to the hub over the control
// channel (FIL-005): file.begin, file.chunk and file.end events, numbered
// and buffered like every node event. The content stays in the outbox
// directory (under node.runtime_dir) until the hub acknowledges the
// file.end event; then the node deletes its copy. A chunk is read from the
// file each time it is sent, so the event buffer holds no content.
type Outbox struct {
	dir    string
	agent  *Agent
	now    func() time.Time
	logger *slog.Logger

	prepare func() error

	mu    sync.Mutex
	files map[int64]outboxFile // by seq of file.end
	bytes int64
}

type outboxFile struct {
	path string
	size int64
}

// NewOutbox returns the outbox of dir. The directory is emptied and
// created (0700) at the first file: the files of a previous process cannot
// be resent.
func NewOutbox(dir string, a *Agent, now func() time.Time, logger *slog.Logger) *Outbox {
	o := &Outbox{dir: dir, agent: a, now: now, logger: logger, files: map[int64]outboxFile{}}
	o.prepare = sync.OnceValue(func() error {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("empty the file outbox: %w", err)
		}

		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create the file outbox: %w", err)
		}

		return nil
	})
	a.Buffer().OnAck(o.acked)

	return o
}

// SendFile moves the file at path (a regular file a decoder wrote under
// node.runtime_dir) into the outbox and queues it for the hub. The caller
// no longer owns the file once SendFile returns, whatever the result: a
// refused file is deleted.
func (o *Outbox) SendFile(ctx context.Context, meta FileMeta, path string) error {
	err := ctx.Err()
	if err == nil {
		err = o.send(meta, path)
	}

	if err != nil {
		_ = os.Remove(path)
	}

	return err
}

func (o *Outbox) send(meta FileMeta, path string) error {
	if err := meta.validate(); err != nil {
		return err
	}

	extra := []byte("{}")

	if meta.Metadata != nil {
		b, err := json.Marshal(meta.Metadata)
		if err != nil {
			return fmt.Errorf("%w: metadata: %w", ErrInvalidFile, err)
		}

		extra = b
	}

	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat produced file: %w", err)
	}

	switch {
	case !fi.Mode().IsRegular() || fi.Size() == 0:
		return fmt.Errorf("%w: not a non-empty regular file", ErrInvalidFile)
	case fi.Size() > meta.MaxSize():
		return fmt.Errorf("%w: %d bytes, at most %d for %s", ErrFileTooBig, fi.Size(), meta.MaxSize(), meta.Kind)
	}

	if err := o.prepare(); err != nil {
		return err
	}

	if !o.reserve(fi.Size()) {
		return ErrOutboxFull
	}

	id, err := shared.NewUUIDv7(o.now())
	if err != nil {
		o.release(fi.Size())

		return fmt.Errorf("file id: %w", err)
	}

	dst := filepath.Join(o.dir, id.String())

	sum, size, err := moveAndHash(path, dst)
	if err == nil && size != fi.Size() {
		err = fmt.Errorf("%w: the file changed while it was moved", ErrInvalidFile)
	}

	if err != nil {
		_ = os.Remove(dst)
		o.release(fi.Size())

		return fmt.Errorf("move produced file to the outbox: %w", err)
	}

	o.queue(id.String(), dst, size, sum, meta, extra)

	return nil
}

func (o *Outbox) reserve(n int64) bool {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.bytes+n > MaxOutboxBytes {
		return false
	}

	o.bytes += n

	return true
}

func (o *Outbox) release(n int64) {
	o.mu.Lock()
	o.bytes -= n
	o.mu.Unlock()
}

// queue buffers the events of a file. The lock is held until the file is
// recorded under its file.end seq, so that an acknowledgement never misses
// it.
func (o *Outbox) queue(id, path string, size int64, sum []byte, meta FileMeta, extra []byte) {
	begin := ctl.FileBegin{
		FileID: id, Kind: meta.Kind, MIME: fileMIME[meta.Kind], Size: size, SHA256: hex.EncodeToString(sum),
		DeviceID: meta.DeviceID, PresetID: meta.PresetID, DecoderSessionID: meta.DecoderSessionID, Mode: meta.Mode,
		FrequencyHz: meta.FrequencyHz, ReceivedStartUTC: utc(meta.ReceivedStart), Metadata: extra,
	}
	if !meta.ReceivedEnd.IsZero() {
		begin.ReceivedEndUTC = utc(meta.ReceivedEnd)
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	o.agent.emitRef(rxv1.TypeFileBegin, ClassDecode, 512+len(extra), func(seq int64) any {
		begin.Seq = seq

		return begin
	})

	for off := int64(0); off < size; off += ctl.FileChunkBytes {
		n := int(min(size-off, ctl.FileChunkBytes))
		o.agent.emitRef(rxv1.TypeFileChunk, ClassDecode, 128, func(seq int64) any {
			return chunkRef{seq: seq, id: id, path: path, off: off, n: n}
		})
	}

	end := o.agent.emitRef(rxv1.TypeFileEnd, ClassDecode, 128, func(seq int64) any {
		return ctl.FileEnd{Seq: seq, FileID: id}
	})
	o.files[end] = outboxFile{path: path, size: size}

	o.logger.Debug("file queued for the hub", slog.String("file_id", id), slog.String("kind", meta.Kind),
		slog.Int64("size", size), slog.Int64("end_seq", end))
}

// acked deletes the files whose file.end the hub acknowledged. A file whose
// events were dropped while the hub was unreachable goes too: the hub
// refuses it as incomplete.
func (o *Outbox) acked(seq int64) {
	o.mu.Lock()
	defer o.mu.Unlock()

	for end, f := range o.files {
		if end > seq {
			continue
		}

		if err := os.Remove(f.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			o.logger.Warn("delete a file the hub acknowledged", slog.String("path", f.path), slog.Any("error", err))
		}

		o.bytes -= f.size
		delete(o.files, end)
	}
}

// Pending returns the files and bytes waiting for the hub.
func (o *Outbox) Pending() (files int, bytes int64) {
	o.mu.Lock()
	defer o.mu.Unlock()

	return len(o.files), o.bytes
}

func utc(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// moveAndHash moves src to dst (a rename, or a copy across file systems)
// and returns the SHA-256 and size of dst.
func moveAndHash(src, dst string) ([]byte, int64, error) {
	if err := os.Rename(src, dst); err != nil {
		if err := copyFile(src, dst); err != nil {
			return nil, 0, err
		}

		_ = os.Remove(src)
	}

	f, err := os.Open(dst)
	if err != nil {
		return nil, 0, err
	}

	defer func() { _ = f.Close() }()

	h := sha256.New()

	n, err := io.Copy(h, f)
	if err != nil {
		return nil, 0, err
	}

	return h.Sum(nil), n, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}

	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()

		return err
	}

	return out.Close()
}

// chunkRef is the payload of a file.chunk: the content is read from the
// outbox when the event is sent.
type chunkRef struct {
	seq  int64
	id   string
	path string
	off  int64
	n    int
}

// MarshalJSON implements json.Marshaler.
func (c chunkRef) MarshalJSON() ([]byte, error) {
	f, err := os.Open(c.path)
	if err != nil {
		return nil, fmt.Errorf("read file %s: %w", c.id, err)
	}

	defer func() { _ = f.Close() }()

	buf := make([]byte, c.n)
	if _, err := f.ReadAt(buf, c.off); err != nil {
		return nil, fmt.Errorf("read file %s at %d: %w", c.id, c.off, err)
	}

	return json.Marshal(ctl.FileChunk{Seq: c.seq, FileID: c.id, Offset: c.off, DataB64: base64.StdEncoding.EncodeToString(buf)})
}
