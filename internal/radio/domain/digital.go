package domain

import (
	"slices"
	"time"

	shared "github.com/yohang/mesh-sdr/internal/shared/domain"
)

// Decoder capabilities (TECHNICAL_SPEC §4.7, DEC-001): what the node probes
// before it offers the digital modes that need them.
const (
	CapWSJT       = "cap:wsjt"
	CapWSJT23     = "cap:wsjt-2.3"
	CapWSJT24     = "cap:wsjt-2.4"
	CapWSPRD      = "cap:wsprd"
	CapJS8        = "cap:js8"
	CapDirewolf   = "cap:direwolf"
	CapMultimonNG = "cap:multimon-ng"
	CapRTL433     = "cap:rtl_433"
	CapSkimmer    = "cap:skimmer"
	CapNativeDSP  = "cap:native-dsp"
)

// DecoderInput is what a decoder reads (§8.3 "Secondary decoder").
type DecoderInput string

// Decoder inputs.
const (
	// InputAudio is the underlying demodulator's audio, resampled to the
	// mode's input rate.
	InputAudio DecoderInput = "audio"
	// InputNarrowIQ is the IQ of the decoder's own narrow secondary
	// selector, at the mode's input rate.
	InputNarrowIQ DecoderInput = "narrow_iq"
	// InputWideIQ is the channel IQ of the demodulator at the mode's input
	// rate.
	InputWideIQ DecoderInput = "wide_iq"
)

// DigitalMode is one entry of the digital mode catalogue (DEC-002): a
// decoder chain run on top of an underlying (analog) demodulator.
type DigitalMode struct {
	// Name is the mode id (decoder.set decoder, decoded_messages.mode).
	Name  string
	Label string
	// Cap is the capability the decoder needs (DEC-001).
	Cap string
	// Family is the decoder family (§9.4: paging, wsjt, textmodes…).
	Family string
	// Underlying are the allowed underlying modes, the default first
	// (DEC-003).
	Underlying []string
	Input      DecoderInput
	// InputRate is the sample rate of the input in Hz.
	InputRate int
	// SecondaryFFT: the mode shows a secondary FFT in the Decoders tab
	// (DEC-004).
	SecondaryFFT bool
	// Slot is the slot interval of batch (slot) decoders; 0 for streaming
	// decoders.
	Slot time.Duration
	// ServiceOnly modes run only as background services: a listener may
	// not start them (§8.3 rule 5).
	ServiceOnly bool
}

// Allows reports whether underlying is an allowed underlying mode.
func (m DigitalMode) Allows(underlying string) bool { return slices.Contains(m.Underlying, underlying) }

// DefaultUnderlying is the underlying mode a decoder switches its
// demodulator to when the current one is not allowed.
func (m DigitalMode) DefaultUnderlying() string { return m.Underlying[0] }

// digitalModes is the catalogue. Later decoders add their entries here
// (and their adapter on the node).
var digitalModes = []DigitalMode{
	// DEC-034: multimon-ng DTMF, EEA, EIA and CCIR at 22 050 Hz.
	{Name: "selcall", Label: "SelCall", Cap: CapMultimonNG, Family: "paging", Underlying: []string{"nfm"}, Input: InputAudio, InputRate: 22050},
	// DEC-035: multimon-ng ZVEI1/2/3, DZVEI and PZVEI.
	{Name: "zvei", Label: "ZVEI", Cap: CapMultimonNG, Family: "paging", Underlying: []string{"nfm"}, Input: InputAudio, InputRate: 22050},
}

// DigitalModes returns the catalogue, in display order.
func DigitalModes() []DigitalMode {
	out := make([]DigitalMode, len(digitalModes))
	for i, m := range digitalModes {
		m.Underlying = slices.Clone(m.Underlying)
		out[i] = m
	}

	return out
}

// Digital mode errors.
var (
	ErrUnknownDecoder     = shared.NewError(shared.KindNotFound, "unknown_decoder", "no such digital mode")
	ErrDecoderServiceOnly = shared.NewError(shared.KindForbidden, "decoder_service_only", "this digital mode runs only as a background service")
	ErrDecoderUnavailable = shared.NewError(shared.KindUnavailable, "decoder_unavailable", "this digital mode is not available on this receiver")
)

// DigitalModeOf returns the catalogue entry of a digital mode a listener
// may start (DEC-002): an unknown mode is ErrUnknownDecoder, a service-only
// one ErrDecoderServiceOnly. Whether its capability is present is checked
// by the caller.
func DigitalModeOf(name string) (DigitalMode, error) {
	i := slices.IndexFunc(digitalModes, func(m DigitalMode) bool { return m.Name == name })
	if i < 0 {
		return DigitalMode{}, ErrUnknownDecoder
	}

	m := digitalModes[i]
	if m.ServiceOnly {
		return DigitalMode{}, ErrDecoderServiceOnly
	}

	m.Underlying = slices.Clone(m.Underlying)

	return m, nil
}
