package dsp

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/yohang/mesh-sdr/internal/dsp/csdr"
	"github.com/yohang/mesh-sdr/internal/protocol/rxv1"
)

// SpectrumConfig is the shared spectrum of one device (§8.3): fft.size,
// fft.fps and fft.voverlap_factor (FEATURE_SPEC defaults 4096, 9, 0.3).
type SpectrumConfig struct {
	SampleRate int
	Size       int
	FPS        int
	VOverlap   float64
}

// DefaultSpectrum returns the FEATURE_SPEC defaults for sampleRate.
func DefaultSpectrum(sampleRate int) SpectrumConfig {
	return SpectrumConfig{SampleRate: sampleRate, Size: 4096, FPS: 9, VOverlap: 0.3}
}

// ErrSpectrumConfig reports invalid spectrum parameters.
var ErrSpectrumConfig = errors.New("dsp: invalid spectrum configuration")

// MaxFFTSize is the largest fft.size (§6.9).
const MaxFFTSize = 32768

// Plan returns how many FFTs are averaged per line and the number of
// samples between two FFTs, as OpenWebRX does: the overlap factor raises
// the number of averaged FFTs at a given frame rate.
func (c SpectrumConfig) Plan() (avg, every int, err error) {
	if c.SampleRate <= 0 || c.Size < 16 || c.Size > MaxFFTSize || c.FPS < 1 || c.VOverlap < 0 || c.VOverlap >= 1 {
		return 0, 0, fmt.Errorf("%w: %+v", ErrSpectrumConfig, c)
	}

	avg = max(1, int(math.Round(float64(c.SampleRate)/float64(c.Size)/float64(c.FPS)/(1-c.VOverlap))))
	every = max(1, c.SampleRate/c.FPS/avg)

	return avg, every, nil
}

// blackmanCoherentGain is the sum of a Blackman window over its length.
const blackmanCoherentGain = 0.42

// Spectrum computes the shared spectrum lines of a device: windowed FFT,
// log power averaged over avg FFTs, bins reordered lowest frequency first
// (libcsdr++ Fft and LogAveragePower; the half swap is done here). Levels
// are dBFS: a full-scale complex tone on a bin reads 0 dB.
type Spectrum struct {
	cfg   SpectrumConfig
	avg   int
	every int

	fft *csdr.Stage[complex64, complex64]
	pow *csdr.Stage[complex64, float32]

	spec  []complex64
	db    []float32
	lines []float32

	consumed uint64
}

// Line is one spectrum line, valid until the next Push.
type Line struct {
	// Index and Time locate the first IQ sample that contributed.
	Index uint64
	Time  time.Time
	DB    []float32
}

// NewSpectrum builds the stages for cfg. Close releases them.
func NewSpectrum(cfg SpectrumConfig) (*Spectrum, error) {
	avg, every, err := cfg.Plan()
	if err != nil {
		return nil, err
	}

	s := &Spectrum{cfg: cfg, avg: avg, every: every}

	addDB := float32(-20 * math.Log10(float64(cfg.Size)*blackmanCoherentGain))

	if s.fft, err = csdr.NewFFT(cfg.Size, every, csdr.Blackman); err != nil {
		return nil, err
	}

	if s.pow, err = csdr.NewLogAveragePower(cfg.Size, avg, addDB); err != nil {
		s.Close()

		return nil, err
	}

	return s, nil
}

// Config returns the configuration.
func (s *Spectrum) Config() SpectrumConfig { return s.cfg }

// Push feeds a block of IQ starting at sample index with its time, and
// calls emit for every line completed.
func (s *Spectrum) Push(index uint64, t time.Time, iq []complex64, emit func(Line)) error {
	size := s.cfg.Size
	// Every FFT of the block fits, plus the one the carry may complete.
	ffts := (len(iq)+s.fft.Pending())/s.every + 2
	s.spec = Grow(s.spec, (ffts+1)*size+1)
	s.db = Grow(s.db, (ffts/s.avg+2)*size+1)

	n, err := s.fft.Process(iq, s.spec)
	if err != nil {
		return err
	}

	m, err := s.pow.Process(s.spec[:n], s.db)
	if err != nil {
		return err
	}

	k := m - m%size
	s.lines = Grow(s.lines, k)
	half := size / 2

	for off := 0; off < k; off += size {
		copy(s.lines[off:off+half], s.db[off+half:off+size])
		copy(s.lines[off+half:off+size], s.db[off:off+half])
	}

	s.consumed += uint64(len(iq))

	// The last line ends where the FFT stage stopped reading; earlier lines
	// one averaging period before.
	end := s.consumed - uint64(s.fft.Pending())
	period := uint64(s.avg * s.every)
	lines := k / size

	for i := range lines {
		last := end - uint64(lines-1-i)*period
		first := last - min(last, period+uint64(size))
		at := t.Add(time.Duration((float64(first) - float64(index)) / float64(s.cfg.SampleRate) * float64(time.Second)))
		emit(Line{Index: first, Time: at, DB: s.lines[i*size : (i+1)*size]})
	}

	return nil
}

// Close releases the stages.
func (s *Spectrum) Close() {
	s.fft.Close()
	s.pow.Close()
}

// Grow returns b resized to n items, reallocated when its capacity is short.
func Grow[T any](b []T, n int) []T {
	if cap(b) < n {
		return make([]T, n)
	}

	return b[:n]
}

// EncodeFFTU8 appends the rx.v1 FFT u8 dB payload of db (§6.7, codec 0x10).
func EncodeFFTU8(dst []byte, scale rxv1.FFTU8Scale, db []float32) []byte {
	dst = rxv1.AppendFFTU8(dst, scale, nil)
	for _, v := range db {
		dst = append(dst, scale.Quantise(v))
	}

	return dst
}
