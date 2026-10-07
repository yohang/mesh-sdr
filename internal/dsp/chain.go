package dsp

import (
	"errors"
	"fmt"
	"math"
	"slices"

	"github.com/yohang/mesh-sdr/internal/dsp/csdr"
)

// OutputRates are the client audio rates (§8.3 execution model rule 4).
var OutputRates = []int{8000, 11025, 12000, 16000, 22050, 24000, 44100, 48000}

// HDOutputRates are the audio rates of the HD path (§8.3 rule 4
// hd_output_rate), required by wide demodulators.
var HDOutputRates = []int{44100, 48000}

// DefaultOutputRate is the audio rate of DEM-010 (12 kHz).
const DefaultOutputRate = 12000

// Channel rates.
const (
	// NarrowChannelRate is the minimum channel rate of the narrow chains
	// (AM, SAM, NFM, SSB, CW) and the rate the channel plan is sized for.
	NarrowChannelRate = 24000
	// WideChannelRate is the minimum channel rate of broadcast FM (200 kHz
	// IF, DEM-015).
	WideChannelRate = 200000
)

// Broadcast FM de-emphasis time constants in µs (DEM-016, hub setting
// wfm_deemphasis).
const (
	WFMDeemphasis50 = 50
	WFMDeemphasis75 = 75
)

// NR threshold range (§8.3 rule 4 nr_threshold).
const (
	NRThresholdMin = -20
	NRThresholdMax = 20
)

// Squelch range (§8.3 rule 4).
const (
	SquelchMin = -150
	SquelchMax = 0
)

// AGCProfile is an AGC preset (settings dsp.*_agc_profile: Fast | Slow).
type AGCProfile string

// AGC profiles.
const (
	AGCFast AGCProfile = "fast"
	AGCSlow AGCProfile = "slow"
	// AGCOff is a chain without AGC (broadcast FM).
	AGCOff AGCProfile = "off"
)

// agcMaxGain bounds the AGC gain (+60 dB): AM and SSB audio follows the
// signal level.
const agcMaxGain = 1000

// agcParams returns the libcsdr++ AGC parameters of a profile at rate.
func agcParams(p AGCProfile, rate float64) (csdr.AGC, error) {
	switch p {
	case AGCFast:
		return csdr.AGC{Reference: 0.8, Attack: 0.1, Decay: 0.001, MaxGain: agcMaxGain, Hang: int(0.02 * rate)}, nil
	case AGCSlow, "":
		return csdr.AGC{Reference: 0.8, Attack: 0.05, Decay: 0.0001, MaxGain: agcMaxGain, Hang: int(0.2 * rate)}, nil
	default:
		return csdr.AGC{}, fmt.Errorf("%w: agc profile %q", ErrChain, p)
	}
}

// ErrChain reports invalid chain parameters.
var ErrChain = errors.New("dsp: invalid demodulator parameters")

// Demodulator is the signal processing of a chain; the engine maps the
// modes of its catalogue onto it.
type Demodulator string

// Demodulators.
const (
	// DemodAM: envelope, DC block (DEM-002).
	DemodAM Demodulator = "am"
	// DemodSAM: carrier PLL, in-phase part, DC block (DEM-003).
	DemodSAM Demodulator = "sam"
	// DemodNFM: FM demodulator, limiter, NFM de-emphasis (DEM-001).
	DemodNFM Demodulator = "nfm"
	// DemodSSB: band-pass, real part (USB, LSB and CW: the pass band picks
	// the sideband and the CW tone, DEM-004, DEM-005).
	DemodSSB Demodulator = "ssb"
	// DemodWFM: FM demodulator, de-emphasis at the output rate (DEM-015).
	DemodWFM Demodulator = "wfm"
)

// ssbTransition is the band-pass transition width of SSB and CW (Hz).
const ssbTransition = 300.0

// NR configures the noise filter (DEM-011).
type NR struct {
	Enabled bool
	// ThresholdDB is the gate above the average power (−20..20 dB).
	ThresholdDB float64
}

func (n NR) validate() error {
	if n.ThresholdDB < NRThresholdMin || n.ThresholdDB > NRThresholdMax || math.IsNaN(n.ThresholdDB) {
		return fmt.Errorf("%w: nr threshold %g dB", ErrChain, n.ThresholdDB)
	}

	return nil
}

func validSquelch(db *float64) error {
	if db != nil && (*db < SquelchMin || *db > SquelchMax || math.IsNaN(*db)) {
		return fmt.Errorf("%w: squelch %g dB", ErrChain, *db)
	}

	return nil
}

// ChainConfig configures the chain of one listener.
type ChainConfig struct {
	Demod Demodulator
	// ChannelRate is the rate of the channel IQ fed to Process.
	ChannelRate float64
	// OutputRate is the client audio rate (one of OutputRates).
	OutputRate int
	// ResidualHz is the part of the offset the channelizer left (see
	// Channel.Residual).
	ResidualHz float64
	// LowHz and HighHz are the pass band, filtered again by DemodSSB.
	LowHz, HighHz float64
	// Squelch is the opening level in dBFS; nil disables the squelch.
	Squelch *float64
	// AGC is the AGC profile of the mode family.
	AGC AGCProfile
	NR  NR
	// DeemphasisUS is the WFM de-emphasis in µs (50 or 75; 0: 50).
	DeemphasisUS int
}

func validDeemphasis(us int) error {
	if us != 0 && us != WFMDeemphasis50 && us != WFMDeemphasis75 {
		return fmt.Errorf("%w: wfm de-emphasis %d µs", ErrChain, us)
	}

	return nil
}

// Validate checks the ranges of §8.3 rule 4.
func (c ChainConfig) Validate() error {
	switch {
	case !slices.Contains([]Demodulator{DemodAM, DemodSAM, DemodNFM, DemodSSB, DemodWFM}, c.Demod):
		return fmt.Errorf("%w: demodulator %q", ErrChain, c.Demod)
	case c.ChannelRate < 8000:
		return fmt.Errorf("%w: channel rate %g", ErrChain, c.ChannelRate)
	case !slices.Contains(OutputRates, c.OutputRate):
		return fmt.Errorf("%w: output rate %d", ErrChain, c.OutputRate)
	case c.Demod == DemodWFM && !slices.Contains(HDOutputRates, c.OutputRate):
		return fmt.Errorf("%w: wfm needs an HD output rate, not %d", ErrChain, c.OutputRate)
	case c.Demod == DemodWFM && c.ChannelRate < WideChannelRate:
		return fmt.Errorf("%w: wfm channel rate %g", ErrChain, c.ChannelRate)
	case c.Demod == DemodSSB && (c.LowHz >= c.HighHz || c.LowHz < -c.ChannelRate/2 || c.HighHz > c.ChannelRate/2):
		return fmt.Errorf("%w: band [%g, %g]", ErrChain, c.LowHz, c.HighHz)
	}

	if err := validSquelch(c.Squelch); err != nil {
		return err
	}

	if err := validDeemphasis(c.DeemphasisUS); err != nil {
		return err
	}

	return c.NR.validate()
}

// stage is a block of the chain: a libcsdr++ stage or a Go one.
type stage[T, U csdr.Sample] interface {
	Process(in []T, out []U) (int, error)
	Pending() int
	Close()
}

// step runs a float stage into its own buffer. ratio is its output rate
// over its input rate.
type step struct {
	s     stage[float32, float32]
	ratio float64
	buf   []float32
}

// stepMargin covers what a stage releases from its carry (AGC look-ahead,
// filter blocks).
const stepMargin = 2048

func (st *step) run(in []float32) ([]float32, error) {
	st.buf = grow(st.buf, int(float64(len(in)+st.s.Pending())*st.ratio)+stepMargin)

	n, err := st.s.Process(in, st.buf)
	if err != nil {
		return nil, err
	}

	return st.buf[:n], nil
}

// Chain is the demodulator chain of one listener (§8.3 per-listener chain):
// selector (residual shift, SSB band-pass, power meter and squelch),
// demodulator, audio chain (AGC, resampling to the client rate, optional
// NR) on libcsdr++ kernels.
type Chain struct {
	cfg ChainConfig

	shift *csdr.Shift
	bp    *csdr.Stage[complex64, complex64]
	demod stage[complex64, float32]
	// pre run at the channel rate, rs resamples, post run at the output
	// rate.
	pre    []*step
	rs     *step
	deemph *deemphasis
	post   []*step
	nr     *csdr.NoiseFilter
	nrSt   *step
	// steps is the float part of the chain in order.
	steps []*step

	shifted, filtered []complex64
	demodOut          []float32

	level float64
	open  bool
}

// NewChain builds the chain. Close releases it.
func NewChain(cfg ChainConfig) (*Chain, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	c := &Chain{cfg: cfg, level: -200, open: cfg.Squelch == nil}

	if err := c.build(); err != nil {
		c.Close()

		return nil, err
	}

	return c, nil
}

func (c *Chain) addPre(s *csdr.Stage[float32, float32], err error) error {
	if err != nil {
		return err
	}

	c.pre = append(c.pre, &step{s: s, ratio: 1})

	return nil
}

func (c *Chain) build() error {
	cfg := c.cfg
	rate := cfg.ChannelRate

	var err error
	if c.shift, err = csdr.NewShift(c.shiftRate()); err != nil {
		return err
	}

	switch cfg.Demod {
	case DemodNFM:
		if c.demod, err = csdr.NewFMDemod(); err != nil {
			return err
		}

		if err := c.addPre(csdr.NewLimit(1)); err != nil {
			return err
		}

		if err := c.addPre(csdr.NewNFMDeemphasis(int(math.Round(rate)))); err != nil {
			return err
		}
	case DemodWFM:
		if c.demod, err = csdr.NewFMDemod(); err != nil {
			return err
		}
	case DemodAM, DemodSAM:
		if cfg.Demod == DemodAM {
			c.demod, err = csdr.NewAMDemod()
		} else {
			c.demod = newPLL(rate)
		}

		if err != nil {
			return err
		}

		if err := c.addPre(csdr.NewDCBlock()); err != nil {
			return err
		}
	case DemodSSB:
		if c.bp, err = csdr.NewBandPass(float32(cfg.LowHz/rate), float32(cfg.HighHz/rate), float32(ssbTransition/rate)); err != nil {
			return err
		}

		if c.demod, err = csdr.NewRealPart(); err != nil {
			return err
		}
	}

	if cfg.AGC != AGCOff {
		p, err := agcParams(cfg.AGC, rate)
		if err != nil {
			return err
		}

		if err := c.addPre(csdr.NewAGC(p)); err != nil {
			return err
		}
	}

	rs, err := csdr.NewResampler(rate, cfg.OutputRate)
	if err != nil {
		return err
	}

	c.rs = &step{s: rs, ratio: float64(cfg.OutputRate) / rate}

	if cfg.Demod == DemodWFM {
		c.deemph = newDeemphasis(cfg.OutputRate, cfg.DeemphasisUS)
		c.post = append(c.post, &step{s: c.deemph, ratio: 1})
	}

	return c.SetNR(cfg.NR)
}

// SetDeemphasis changes the WFM de-emphasis (µs) without rebuilding the
// chain; other chains ignore it.
func (c *Chain) SetDeemphasis(us int) error {
	if err := validDeemphasis(us); err != nil {
		return err
	}

	c.cfg.DeemphasisUS = us

	if c.deemph != nil {
		c.deemph.set(us)
	}

	return nil
}

func (c *Chain) shiftRate() float32 { return float32(-c.cfg.ResidualHz / c.cfg.ChannelRate) }

// Config returns the configuration.
func (c *Chain) Config() ChainConfig { return c.cfg }

// SetResidual changes the residual shift (a retune within the same
// channelizer bin keeps the chain).
func (c *Chain) SetResidual(hz float64) error {
	c.cfg.ResidualHz = hz

	return c.shift.SetRate(c.shiftRate())
}

// SetSquelch changes the squelch level (nil: open).
func (c *Chain) SetSquelch(db *float64) error {
	if err := validSquelch(db); err != nil {
		return err
	}

	c.cfg.Squelch = db

	return nil
}

// noiseFilterSize returns a power-of-two noise filter block of about 25 ms
// at rate.
func noiseFilterSize(rate int) int {
	n := 32
	for n*2 <= rate/40 {
		n *= 2
	}

	return n
}

// SetNR switches the noise filter on or off, or changes its threshold,
// without rebuilding the chain.
func (c *Chain) SetNR(nr NR) error {
	if err := nr.validate(); err != nil {
		return err
	}

	switch {
	case nr.Enabled && c.nr == nil:
		f, err := csdr.NewNoiseFilter(noiseFilterSize(c.cfg.OutputRate), float32(nr.ThresholdDB))
		if err != nil {
			return err
		}

		c.nr, c.nrSt = f, &step{s: f, ratio: 1}
	case nr.Enabled:
		if err := c.nr.SetThreshold(float32(nr.ThresholdDB)); err != nil {
			return err
		}
	case c.nr != nil:
		c.nr.Close()
		c.nr, c.nrSt = nil, nil
	}

	c.cfg.NR = nr
	c.steps = slices.Concat(c.pre, []*step{c.rs}, c.post)

	if c.nrSt != nil {
		c.steps = append(c.steps, c.nrSt)
	}

	return nil
}

// Result is the output of one Process call, valid until the next one.
type Result struct {
	Audio []float32
	// LevelDB is the channel power in dBFS, after the selector.
	LevelDB float64
	// Open reports whether the squelch was open (always true without
	// squelch). Squelched audio is silence.
	Open bool
}

// Process runs a block of channel IQ through the chain.
func (c *Chain) Process(iq []complex64) (Result, error) {
	c.shifted = grow(c.shifted, len(iq)+64)

	n, err := c.shift.Process(iq, c.shifted)
	if err != nil {
		return Result{}, err
	}

	sel := c.shifted[:n]

	if c.bp != nil {
		c.filtered = grow(c.filtered, n+c.bp.Pending()+stepMargin)

		m, err := c.bp.Process(sel, c.filtered)
		if err != nil {
			return Result{}, err
		}

		sel = c.filtered[:m]
	}

	// A block-based selector may release nothing: keep the last reading.
	if len(sel) > 0 {
		c.level = powerDB(sel)
		c.open = c.cfg.Squelch == nil || c.level >= *c.cfg.Squelch
	}

	c.demodOut = grow(c.demodOut, len(sel)+c.demod.Pending()+stepMargin)

	m, err := c.demod.Process(sel, c.demodOut)
	if err != nil {
		return Result{}, err
	}

	audio := c.demodOut[:m]

	for _, st := range c.steps {
		if audio, err = st.run(audio); err != nil {
			return Result{}, err
		}
	}

	if !c.open {
		clear(audio)
	}

	return Result{Audio: audio, LevelDB: c.level, Open: c.open}, nil
}

// Close releases the stages.
func (c *Chain) Close() {
	if c.shift != nil {
		c.shift.Close()
	}

	if c.bp != nil {
		c.bp.Close()
	}

	if c.demod != nil {
		c.demod.Close()
	}

	for _, st := range slices.Concat(c.pre, c.post) {
		st.s.Close()
	}

	if c.rs != nil {
		c.rs.s.Close()
	}

	if c.nr != nil {
		c.nr.Close()
	}
}

// powerDB returns the mean power of iq in dBFS.
func powerDB(iq []complex64) float64 {
	if len(iq) == 0 {
		return -200
	}

	var sum float64
	for _, v := range iq {
		sum += float64(real(v))*float64(real(v)) + float64(imag(v))*float64(imag(v))
	}

	p := sum / float64(len(iq))
	if p <= 1e-20 {
		return -200
	}

	return 10 * math.Log10(p)
}
