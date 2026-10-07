// Package app runs the devices of a node (TECHNICAL_SPEC §8.2): the device
// manager starts and stops each device's source on demand, applies the
// retry schedule outcome, auto-recovers failed devices, retunes, and
// reports every state; it hands media sessions a lease on a device's DSP
// engine.
package app

import (
	"context"
	"errors"
	"time"

	"github.com/yohang/mesh-sdr/internal/radio/domain"
)

// Errors returned by Source.Run.
var (
	// ErrSourceFailed: the start attempts are exhausted (§8.2: failed).
	ErrSourceFailed = errors.New("source failed")
	// ErrSourceUnavailable: the tool is missing or refuses its
	// configuration; no retry until the configuration changes.
	ErrSourceUnavailable = errors.New("source unavailable")
)

// IQSink receives the samples of a running source.
type IQSink interface {
	// Samples delivers a block of CF32 IQ; index counts the samples since
	// the source started and t is the time of the first one. iq is valid
	// during the call only.
	Samples(index uint64, t time.Time, iq []complex64)
}

// SourceEvent is a lifecycle transition of a source.
type SourceEvent struct {
	State   domain.State
	Reason  string
	Attempt int
}

// Source runs the connector of one device.
type Source interface {
	// Run supervises the connector at tuning t until ctx ends (nil) or it
	// fails for good (ErrSourceFailed, ErrSourceUnavailable). report is
	// called on every transition and must not block.
	Run(ctx context.Context, t domain.Tuning, sink IQSink, report func(SourceEvent)) error
	// SetCenter retunes the running connector live (control socket).
	SetCenter(hz int64) error
}

// Sources builds the source of a device.
type Sources interface {
	// Probe checks that the tool of the device type can run (§8.4
	// capability probing). An error makes the device unavailable.
	Probe(ctx context.Context, p domain.DeviceParams) error
	New(p domain.DeviceParams) (Source, error)
}

// SpectrumInfo describes the shared spectrum of a device.
type SpectrumInfo struct {
	Size    int
	FPS     int
	StartHz int64
	SpanHz  int
}

// SpectrumFrame is one encoded spectrum line (FFT u8 dB, §6.7). Payload is
// shared by every subscriber and must not be modified.
type SpectrumFrame struct {
	Payload     []byte
	TimestampUS uint64
}

// AudioCodec is an audio encoding of the node.
type AudioCodec string

// Audio codecs (§6.4 audio.configure names). Opus is not provided.
const (
	CodecPCM   AudioCodec = "pcm-s16le"
	CodecADPCM AudioCodec = "adpcm-ima"
)

// DemodParams configure a demodulator (§6.4 demod.create / demod.set).
type DemodParams struct {
	Mode     string
	OffsetHz int64
	// LowHz and HighHz are the pass band relative to the offset.
	LowHz, HighHz float64
	// SquelchDB is the squelch level; nil: open.
	SquelchDB  *float64
	OutputRate int
	Codec      AudioCodec
}

// AudioOut is one encoded audio frame of a demodulator.
type AudioOut struct {
	Payload     []byte
	Samples     int
	Duration    time.Duration
	TimestampUS uint64
	Squelched   bool
	// Reset: first frame of a codec configuration.
	Reset bool
	// Discontinuity: input samples were lost before this frame.
	Discontinuity bool
}

// Meter is a demodulator level reading (demod.meter), at most 10 Hz.
type Meter struct {
	LevelDB float64
	Open    bool
}

// Demod is a running demodulator.
type Demod interface {
	// Set changes the parameters; the chain is rebuilt off the hot path
	// when needed and the previous one keeps running on failure (§8.3
	// rule 3).
	Set(p DemodParams) error
	// Params returns the applied parameters.
	Params() DemodParams
	Close()
}

// Engine is the DSP of one device (§8.3): it receives the samples of the
// device source and serves the shared spectrum and the demodulators.
type Engine interface {
	IQSink
	// Start begins a new run of the source at tuning t (rings reset).
	Start(t domain.Tuning)
	// Retuned records a live change of the centre frequency.
	Retuned(t domain.Tuning)
	// Stop ends the current run.
	Stop()
	// Spectrum returns the spectrum geometry at the current tuning.
	Spectrum() SpectrumInfo
	// SubscribeSpectrum delivers every spectrum frame to deliver, which
	// must not block, until cancel.
	SubscribeSpectrum(deliver func(SpectrumFrame)) (cancel func())
	// NewDemod starts a demodulator; audio and meter must not block.
	NewDemod(p DemodParams, audio func(AudioOut), meter func(Meter)) (Demod, error)
	// Close stops every goroutine.
	Close()
}

// Engines builds the engine of a device.
type Engines interface {
	New(id domain.DeviceID) Engine
}

// Reporter publishes device states (device.state over the control
// channel). It must not block.
type Reporter interface {
	DeviceState(s domain.Snapshot)
}
