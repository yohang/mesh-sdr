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

// DefaultOutputRate is the audio rate of DEM-010 (12 kHz).
const DefaultOutputRate = 12000

// NFMChannelRate is the minimum channel rate of the NFM chain.
const NFMChannelRate = 24000

// AGCProfile is an AGC preset (settings dsp.agc_profile.*: Fast | Slow).
type AGCProfile string

// AGC profiles.
const (
	AGCFast AGCProfile = "fast"
	AGCSlow AGCProfile = "slow"
)

// agcParams returns the libcsdr++ AGC parameters of a profile at rate.
func agcParams(p AGCProfile, rate float64) (csdr.AGC, error) {
	switch p {
	case AGCFast:
		return csdr.AGC{Reference: 0.8, Attack: 0.1, Decay: 0.001, MaxGain: 10, Hang: int(0.02 * rate)}, nil
	case AGCSlow, "":
		return csdr.AGC{Reference: 0.8, Attack: 0.05, Decay: 0.0001, MaxGain: 10, Hang: int(0.2 * rate)}, nil
	default:
		return csdr.AGC{}, fmt.Errorf("%w: agc profile %q", ErrChain, p)
	}
}

// ErrChain reports invalid chain parameters.
var ErrChain = errors.New("dsp: invalid demodulator parameters")

// NFMConfig configures an NFM chain (DEM-001).
type NFMConfig struct {
	// ChannelRate is the rate of the channel IQ fed to Process.
	ChannelRate float64
	// OutputRate is the client audio rate (one of OutputRates).
	OutputRate int
	// ResidualHz is the part of the offset the channelizer left (see
	// Channel.Residual).
	ResidualHz float64
	// Squelch is the opening level in dBFS; nil disables the squelch.
	Squelch *float64
	// AGC is the AGC profile (dsp.agc_profile.nfm, default slow).
	AGC AGCProfile
}

// Validate checks the ranges of §8.3 rule 4.
func (c NFMConfig) Validate() error {
	switch {
	case c.ChannelRate < 8000:
		return fmt.Errorf("%w: channel rate %g", ErrChain, c.ChannelRate)
	case !slices.Contains(OutputRates, c.OutputRate):
		return fmt.Errorf("%w: output rate %d", ErrChain, c.OutputRate)
	case c.Squelch != nil && (*c.Squelch < -150 || *c.Squelch > 0):
		return fmt.Errorf("%w: squelch %g dB", ErrChain, *c.Squelch)
	}

	return nil
}

// NFM is the NFM chain of one listener (DEM-001): residual shift, power
// meter and squelch, FM demodulator, limiter, NFM de-emphasis, AGC and
// resampling to the client rate (libcsdr++ kernels).
type NFM struct {
	cfg NFMConfig

	shift  *csdr.Shift
	demod  *csdr.Stage[complex64, float32]
	limit  *csdr.Stage[float32, float32]
	deemph *csdr.Stage[float32, float32]
	agc    *csdr.Stage[float32, float32]
	rs     *csdr.Stage[float32, float32]

	shifted []complex64
	a, b    []float32
	out     []float32
}

// NewNFM builds the chain. Close releases it.
func NewNFM(cfg NFMConfig) (*NFM, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	agc, err := agcParams(cfg.AGC, cfg.ChannelRate)
	if err != nil {
		return nil, err
	}

	c := &NFM{cfg: cfg}

	build := []func() error{
		func() (err error) { c.shift, err = csdr.NewShift(c.shiftRate()); return err },
		func() (err error) { c.demod, err = csdr.NewFMDemod(); return err },
		func() (err error) { c.limit, err = csdr.NewLimit(1); return err },
		func() (err error) {
			c.deemph, err = csdr.NewNFMDeemphasis(int(math.Round(cfg.ChannelRate)))
			return err
		},
		func() (err error) { c.agc, err = csdr.NewAGC(agc); return err },
		func() (err error) { c.rs, err = csdr.NewResampler(cfg.ChannelRate, cfg.OutputRate); return err },
	}

	for _, f := range build {
		if err := f(); err != nil {
			c.Close()

			return nil, err
		}
	}

	return c, nil
}

func (c *NFM) shiftRate() float32 { return float32(-c.cfg.ResidualHz / c.cfg.ChannelRate) }

// Config returns the configuration.
func (c *NFM) Config() NFMConfig { return c.cfg }

// SetResidual changes the residual shift (a retune within the same
// channelizer bin keeps the chain).
func (c *NFM) SetResidual(hz float64) error {
	c.cfg.ResidualHz = hz

	return c.shift.SetRate(c.shiftRate())
}

// SetSquelch changes the squelch level (nil: open).
func (c *NFM) SetSquelch(db *float64) error {
	if db != nil && (*db < -150 || *db > 0) {
		return fmt.Errorf("%w: squelch %g dB", ErrChain, *db)
	}

	c.cfg.Squelch = db

	return nil
}

// Result is the output of one Process call, valid until the next one.
type Result struct {
	Audio []float32
	// LevelDB is the channel power in dBFS.
	LevelDB float64
	// Open reports whether the squelch was open (always true without
	// squelch). Squelched audio is silence.
	Open bool
}

// Process runs a block of channel IQ through the chain.
func (c *NFM) Process(iq []complex64) (Result, error) {
	c.shifted = grow(c.shifted, len(iq)+64)

	n, err := c.shift.Process(iq, c.shifted)
	if err != nil {
		return Result{}, err
	}

	level := powerDB(c.shifted[:n])
	open := c.cfg.Squelch == nil || level >= *c.cfg.Squelch

	c.a = grow(c.a, n+256)

	m, err := c.demod.Process(c.shifted[:n], c.a)
	if err != nil {
		return Result{}, err
	}

	c.b = grow(c.b, m+256)

	if m, err = c.limit.Process(c.a[:m], c.b); err != nil {
		return Result{}, err
	}

	if m, err = c.deemph.Process(c.b[:m], c.a); err != nil {
		return Result{}, err
	}

	if m, err = c.agc.Process(c.a[:m], c.b); err != nil {
		return Result{}, err
	}

	c.out = grow(c.out, int(float64(m+c.rs.Pending())*float64(c.cfg.OutputRate)/c.cfg.ChannelRate)+64)

	k, err := c.rs.Process(c.b[:m], c.out)
	if err != nil {
		return Result{}, err
	}

	audio := c.out[:k]
	if !open {
		clear(audio)
	}

	return Result{Audio: audio, LevelDB: level, Open: open}, nil
}

// Close releases the stages.
func (c *NFM) Close() {
	if c.shift != nil {
		c.shift.Close()
	}

	c.demod.Close()
	c.limit.Close()
	c.deemph.Close()
	c.agc.Close()
	c.rs.Close()
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
