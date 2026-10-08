package decoder

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"time"

	"github.com/yohang/mesh-sdr/internal/radio/domain"
	"github.com/yohang/mesh-sdr/internal/shared/process"
)

// Slot recording (DEC-026, §8.4 "Batch slot decoders").
const (
	// slotRate is the rate of the slot WAV files (12 kHz mono, 16 bit).
	slotRate = domain.SlotRate
	// wavHeader is the size of the canonical WAV header.
	wavHeader = 44
	// SlotGuard: a slot with less audio than this is not decoded (the
	// first slot of a session usually starts late).
	SlotGuard = time.Second
)

// samplesIn returns the number of slot samples in d (rounded).
func samplesIn(d time.Duration) int64 {
	return int64(math.Round(d.Seconds() * slotRate))
}

// durationOf returns the duration of n slot samples.
func durationOf(n int) time.Duration {
	return time.Duration(int64(n) * int64(time.Second) / slotRate)
}

// slotStart returns the UTC multiple of period that contains t.
func slotStart(t time.Time, period time.Duration) time.Time {
	return time.Unix(0, t.UnixNano()-t.UnixNano()%int64(period)).UTC()
}

// slotFile is the WAV of one slot of one period, in the session workdir.
// Its jobs share it through a reference count and the last one removes it
// (no hard links, §8.4).
type slotFile struct {
	path   string
	start  time.Time
	period time.Duration
	f      *os.File
	// real counts the samples of audio written (samples written again
	// after the timestamps went back count once at most); end is the
	// sample after the last one written. Missing samples read as silence.
	real, end int64
	// dial is the dial frequency when the slot started (0: unknown).
	dial int64
	refs atomic.Int32
}

// slotFileName names a slot file so that jt9, js8 and wsprd read the
// slot's time of day from it: …_YYMMDD_HHMMSS.wav below one minute,
// …_YYMMDD_HHMM.wav from one minute.
func slotFileName(start time.Time, period time.Duration) string {
	layout := "060102_1504"
	if period < time.Minute {
		layout = "060102_150405"
	}

	return fmt.Sprintf("p%d_%s.wav", period.Milliseconds(), start.UTC().Format(layout))
}

// full returns the samples of a complete slot.
func (f *slotFile) full() int64 { return samplesIn(f.period) }

// partial reports whether the slot lost more than 10 % of its audio: it is
// still decoded, flagged (§8.4 batch rule 1).
func (f *slotFile) partial() bool { return f.full()-f.real > f.full()/10 }

// write writes s16le samples whose first one is at sample index idx of
// the slot.
func (f *slotFile) write(pcm []byte, idx int64) error {
	if _, err := f.f.WriteAt(pcm, wavHeader+2*idx); err != nil {
		return err
	}

	n := int64(len(pcm) / 2)
	f.end = max(f.end, idx+n)
	f.real = min(f.real+n, f.end)

	return nil
}

// finish writes the WAV header and closes the file.
func (f *slotFile) finish() error {
	data := uint32(2 * f.end)

	h := make([]byte, 0, wavHeader)
	h = append(h, "RIFF"...)
	h = binary.LittleEndian.AppendUint32(h, 36+data)
	h = append(h, "WAVEfmt "...)
	h = binary.LittleEndian.AppendUint32(h, 16)
	h = binary.LittleEndian.AppendUint16(h, 1) // PCM
	h = binary.LittleEndian.AppendUint16(h, 1) // mono
	h = binary.LittleEndian.AppendUint32(h, slotRate)
	h = binary.LittleEndian.AppendUint32(h, 2*slotRate)
	h = binary.LittleEndian.AppendUint16(h, 2)
	h = binary.LittleEndian.AppendUint16(h, 16)
	h = append(h, "data"...)
	h = binary.LittleEndian.AppendUint32(h, data)

	_, err := f.f.WriteAt(h, 0)

	return errors.Join(err, f.f.Close())
}

// release drops one reference; the last one removes the file.
func (f *slotFile) release(log *slog.Logger) {
	if f.refs.Add(-1) == 0 {
		f.remove(log)
	}
}

func (f *slotFile) remove(log *slog.Logger) {
	if err := os.Remove(f.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Warn("slot file not removed", slog.String("file", filepath.Base(f.path)), slog.Any("error", err))
	}
}

// recorder cuts the 12 kHz audio of a session into slots aligned to UTC
// multiples of each period, one file per period whatever the number of
// profiles that use it (DEC-026). Times come from the sample timestamps,
// not from timers (§8.4 batch rule 1).
type recorder struct {
	dir  string
	open map[time.Duration]*slotFile
	// failed is the slot of each period whose file was not created.
	failed map[time.Duration]time.Time
	// dial returns the dial frequency, read when a slot starts (0:
	// unknown).
	dial func() int64
	log  *slog.Logger
}

func newRecorder(dir string, dial func() int64, log *slog.Logger) *recorder {
	if dial == nil {
		dial = func() int64 { return 0 }
	}

	return &recorder{dir: dir, open: map[time.Duration]*slotFile{}, failed: map[time.Duration]time.Time{}, dial: dial, log: log}
}

// write records pcm (s16le at 12 kHz) whose first sample is at t in the
// current slot of each period, and returns the slots it completed. A
// period no longer in periods loses its slot in progress.
func (r *recorder) write(t time.Time, pcm []byte, periods []time.Duration) []*slotFile {
	var done []*slotFile

	for p, f := range r.open {
		if !slices.Contains(periods, p) {
			r.discard(f)
			delete(r.open, p)
		}
	}

	for _, p := range periods {
		done = append(done, r.writePeriod(p, t, pcm)...)
	}

	return done
}

func (r *recorder) writePeriod(p time.Duration, t time.Time, pcm []byte) []*slotFile {
	var done []*slotFile

	f := r.open[p]

	for len(pcm) >= 2 {
		if f == nil {
			start := slotStart(t, p)

			// A slot whose file cannot be created (it still exists after
			// the timestamps went back) is skipped until the next one.
			if r.failed[p].Equal(start) {
				return done
			}

			var err error
			if f, err = r.create(start, p); err != nil {
				r.log.Warn("slot file not created: the slot is not decoded", slog.Duration("period", p), slog.Time("slot", start), slog.Any("error", err))
				r.failed[p] = start

				return done
			}

			r.open[p] = f
		}

		end := f.start.Add(p)
		if !t.Before(end) {
			if err := f.finish(); err != nil {
				r.log.Warn("slot file not written: the slot is not decoded", slog.Any("error", err))
				f.remove(r.log)
			} else {
				done = append(done, f)
			}

			delete(r.open, p)
			f = nil

			continue
		}

		n := int64(len(pcm) / 2)
		k := min(n, max(1, int64(math.Ceil(end.Sub(t).Seconds()*slotRate))))

		if idx := samplesIn(t.Sub(f.start)); idx >= 0 {
			if err := f.write(pcm[:2*k], idx); err != nil {
				r.log.Warn("slot audio not written", slog.Any("error", err))
			}
		}

		t = t.Add(durationOf(int(k)))
		pcm = pcm[2*k:]
	}

	return done
}

func (r *recorder) create(start time.Time, p time.Duration) (*slotFile, error) {
	name := slotFileName(start, p)

	f, err := process.CreateFile(r.dir, name, 0o600)
	if err != nil {
		return nil, err
	}

	return &slotFile{path: filepath.Join(r.dir, name), start: start, period: p, f: f, dial: r.dial()}, nil
}

func (r *recorder) discard(f *slotFile) {
	_ = f.f.Close()
	f.remove(r.log)
}

// close discards the slots in progress.
func (r *recorder) close() {
	for p, f := range r.open {
		r.discard(f)
		delete(r.open, p)
	}
}
