package domain

import (
	"math"
	"slices"
	"strconv"
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
	// CapRTTYSkimmer is the RTTY skimmer (csdr-rttyskimmer); cap:skimmer
	// is the CW one.
	CapRTTYSkimmer = "cap:skimmer-rtty"
	CapNativeDSP   = "cap:native-dsp"
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
	// InputWideIQ is the IQ of a second channel at the demodulator's
	// offset, at the mode's input rate (the listener keeps hearing the
	// narrow demodulator).
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
	// Slot is the slot interval of batch (slot) decoders, the shortest one
	// of a mode with several (FST4, Q65, JS8); 0 for streaming decoders.
	Slot time.Duration
	// LowHz and HighHz are the pass band the decoder sets on its
	// demodulator when it starts (WSJT: 0 to 3000 Hz, WSPR: 1350 to 1650
	// Hz); both zero keep the demodulator's.
	LowHz, HighHz float64
	// ServiceOnly modes run only as background services: a listener may
	// not start them (§8.3 rule 5).
	ServiceOnly bool
	// Variants are the decoder variants a listener chooses from (the
	// default first); none for a mode without variants.
	Variants []string
	// DedupStep is the frequency rounding (Hz) of the hub's duplicate
	// key: two listeners a few hertz apart decode the same message (ADR
	// 0028).
	DedupStep int64
	// BandwidthHz is the half width of the secondary selector of a text
	// decoder (DEC-005): it keeps ±BandwidthHz around the secondary
	// offset. 0 for modes without a secondary selector.
	BandwidthHz float64
	// BandLow and BandHigh are the pass band (Hz from the dial) of a wide
	// IQ input; it is cut to the input rate.
	BandLow, BandHigh float64
}

// TextRate is the input rate of the native text decoders and of their
// secondary FFT (the selector rate of OpenWebRX+'s digital chains).
const TextRate = 12000

// CheckOffset checks the secondary offset of a text decoder: the
// selector band (±BandwidthHz around it) stays within the input band.
func (m DigitalMode) CheckOffset(hz float64) error {
	if m.BandwidthHz <= 0 {
		return nil
	}

	if math.IsNaN(hz) || math.Abs(hz)+m.BandwidthHz > float64(m.InputRate)/2 {
		return ErrOutOfRange.WithDetail("offset_hz: the decoder band must stay within ±" + strconv.Itoa(m.InputRate/2) + " Hz")
	}

	return nil
}

// DedupWindow is the time bucket of the duplicate key of streaming modes;
// slot modes use their slot.
const DedupWindow = 10 * time.Second

// DedupBucket returns the time bucket of the duplicate key of the mode.
func (m DigitalMode) DedupBucket() time.Duration {
	if m.Slot > 0 {
		return m.Slot
	}

	return DedupWindow
}

// Variant returns the variant a listener asks for ("" for the default):
// ok is false for a variant the mode does not have.
func (m DigitalMode) Variant(v string) (string, bool) {
	if len(m.Variants) == 0 {
		return "", v == ""
	}

	if v == "" {
		return m.Variants[0], true
	}

	return v, slices.Contains(m.Variants, v)
}

// Allows reports whether underlying is an allowed underlying mode.
func (m DigitalMode) Allows(underlying string) bool { return slices.Contains(m.Underlying, underlying) }

// DefaultUnderlying is the underlying mode a decoder switches its
// demodulator to when the current one is not allowed.
func (m DigitalMode) DefaultUnderlying() string { return m.Underlying[0] }

// digitalModes is the catalogue. Later decoders add their entries here
// (and their adapter on the node).
var digitalModes = []DigitalMode{
	// DEC-034: multimon-ng with one of DTMF, EEA, EIA and CCIR at 22 050 Hz.
	{
		Name: "selcall", Label: "SelCall", Cap: CapMultimonNG, Family: "paging", Underlying: []string{"nfm"}, Input: InputAudio, InputRate: 22050,
		Variants: []string{"DTMF", "EEA", "EIA", "CCIR"}, DedupStep: 1000,
	},
	// DEC-035: multimon-ng with one of ZVEI1/2/3, DZVEI and PZVEI.
	{
		Name: "zvei", Label: "ZVEI", Cap: CapMultimonNG, Family: "paging", Underlying: []string{"nfm"}, Input: InputAudio, InputRate: 22050,
		Variants: []string{"ZVEI1", "ZVEI2", "ZVEI3", "DZVEI", "PZVEI"}, DedupStep: 1000,
	},
	// DEC-037: the libcsdr++ SSTV decoder on the audio at 24 kHz
	// (OpenWebRX+ chain).
	{Name: "sstv", Label: "SSTV", Cap: CapNativeDSP, Family: "image", Underlying: []string{"usb", "lsb", "nfm"}, Input: InputAudio, InputRate: 24000, DedupStep: 1000},
	// DEC-038: the libcsdr++ HF FAX decoder on the USB audio at 12 kHz.
	{Name: "fax", Label: "FAX", Cap: CapNativeDSP, Family: "image", Underlying: []string{"usb"}, Input: InputAudio, InputRate: 12000, DedupStep: 1000},
	// DEC-006, DEC-007: native BPSK with Varicode, ±baud selector.
	{
		Name: "bpsk31", Label: "BPSK31", Cap: CapNativeDSP, Family: textmodes, Underlying: []string{"usb"}, Input: InputNarrowIQ, InputRate: TextRate,
		SecondaryFFT: true, BandwidthHz: 31.25, DedupStep: textDedupStep,
	},
	{
		Name: "bpsk63", Label: "BPSK63", Cap: CapNativeDSP, Family: textmodes, Underlying: []string{"usb"}, Input: InputNarrowIQ, InputRate: TextRate,
		SecondaryFFT: true, BandwidthHz: 62.5, DedupStep: textDedupStep,
	},
	// DEC-008 to DEC-010: native RTTY with Baudot, ±shift selector.
	{
		Name: "rtty170", Label: "RTTY-170 (45)", Cap: CapNativeDSP, Family: textmodes, Underlying: []string{"usb", "lsb"}, Input: InputNarrowIQ,
		InputRate: TextRate, SecondaryFFT: true, BandwidthHz: 170, DedupStep: textDedupStep,
	},
	{
		Name: "rtty450", Label: "RTTY-450 (50N)", Cap: CapNativeDSP, Family: textmodes, Underlying: []string{"usb", "lsb"}, Input: InputNarrowIQ,
		InputRate: TextRate, SecondaryFFT: true, BandwidthHz: 450, DedupStep: textDedupStep,
	},
	{
		Name: "rtty85", Label: "RTTY-85 (50N)", Cap: CapNativeDSP, Family: textmodes, Underlying: []string{"usb", "lsb"}, Input: InputNarrowIQ,
		InputRate: TextRate, SecondaryFFT: true, BandwidthHz: 85, DedupStep: textDedupStep,
	},
	// DEC-011: native SITOR-B with CCIR 476, 100 Bd, 170 Hz shift.
	{
		Name: "sitorb", Label: "SITOR-B", Cap: CapNativeDSP, Family: textmodes, Underlying: []string{"usb"}, Input: InputNarrowIQ, InputRate: TextRate,
		SecondaryFFT: true, BandwidthHz: 210, DedupStep: textDedupStep,
	},
	// DEC-012: native CW decoder, 75 Hz selector.
	{
		Name: "cwdecoder", Label: "CW Decoder", Cap: CapNativeDSP, Family: textmodes, Underlying: []string{"usb", "lsb"}, Input: InputNarrowIQ,
		InputRate: TextRate, SecondaryFFT: true, BandwidthHz: 75, DedupStep: textDedupStep,
	},
	// DEC-016…023: the WSJT-X family, 12 kHz WAV slots decoded by jt9 or
	// wsprd (DEC-025, DEC-026) on USB audio.
	wsjtMode("ft8", "FT8", CapWSJT, 15*time.Second, 0, 3000),
	wsjtMode("ft4", "FT4", CapWSJT, 7500*time.Millisecond, 0, 3000),
	wsjtMode("jt65", "JT65", CapWSJT, time.Minute, 0, 3000),
	wsjtMode("jt9", "JT9", CapWSJT, time.Minute, 0, 3000),
	wsjtMode("wspr", "WSPR", CapWSPRD, 2*time.Minute, 1350, 1650),
	wsjtMode("fst4", "FST4", CapWSJT23, 15*time.Second, 0, 3000),
	wsjtMode("fst4w", "FST4W", CapWSJT23, 2*time.Minute, 1350, 1650),
	wsjtMode("q65", "Q65", CapWSJT24, 15*time.Second, 0, 3000),
	// DEC-029: JS8Call, slots of 6 to 30 s decoded by js8.
	{
		Name: "js8", Label: "JS8Call", Cap: CapJS8, Family: "js8", Underlying: []string{"usb", "usbd"}, Input: InputAudio, InputRate: SlotRate,
		Slot: 6 * time.Second, HighHz: 3000,
	},
	// DEC-031, DEC-032: direwolf (AX.25 1200 Bd) on FM audio at 48 kHz,
	// APRS parsed on the node.
	{Name: "packet", Label: "Packet", Cap: CapDirewolf, Family: "packet", Underlying: []string{"nfm"}, Input: InputAudio, InputRate: 48000, DedupStep: 1000},
	// DEC-033: multimon-ng with FLEX and POCSAG 512/1200/2400 at 22 050 Hz.
	{Name: "page", Label: "Page", Cap: CapMultimonNG, Family: "paging", Underlying: []string{"nfm"}, Input: InputAudio, InputRate: 22050, DedupStep: 1000},
	// DEC-036: multimon-ng EAS (SAME headers) at 22 050 Hz.
	{Name: "eas", Label: "EAS", Cap: CapMultimonNG, Family: "paging", Underlying: []string{"nfm"}, Input: InputAudio, InputRate: 22050, DedupStep: 1000},
	// DEC-013, DEC-014: the skimmers decode every signal of the 48 kHz of
	// band above the dial: the real part of a 96 kHz wide IQ tap
	// (OpenWebRX+ chain). A signal's frequency is the dial plus its offset,
	// rounded to the skimmer's bins.
	{
		Name: "cwskimmer", Label: "CW Skimmer", Cap: CapSkimmer, Family: "skimmer", Underlying: []string{"usb", "cw", "lsb"}, Input: InputWideIQ,
		InputRate: 96000, BandHigh: 48000, DedupStep: 100,
	},
	{
		Name: "rttyskimmer", Label: "RTTY Skimmer", Cap: CapRTTYSkimmer, Family: "skimmer", Underlying: []string{"usb", "lsb"}, Input: InputWideIQ,
		InputRate: 96000, BandHigh: 48000, DedupStep: 100,
	},
	// DEC-039: rtl_433 on 250 kHz of IQ around the dial.
	{
		Name: "ism", Label: "ISM", Cap: CapRTL433, Family: "ism", Underlying: []string{"am", "nfm"}, Input: InputWideIQ, InputRate: 250_000,
		BandLow: -125_000, BandHigh: 125_000, DedupStep: 1000,
	},
	// DEC-040: rtl_433 with its Wireless M-Bus decoders on 1.2 MS/s of IQ
	// (their rate), 250 kHz of band around the dial.
	{
		Name: "wmbus", Label: "WMBus", Cap: CapRTL433, Family: "ism", Underlying: []string{"nfm", "am"}, Input: InputWideIQ, InputRate: 1_200_000,
		BandLow: -125_000, BandHigh: 125_000, DedupStep: 1000,
	},
}

// SlotRate is the input rate of the slot decoders: 12 kHz mono WAV
// (DEC-026).
const SlotRate = 12000

func wsjtMode(name, label, capability string, slot time.Duration, low, high float64) DigitalMode {
	return DigitalMode{
		Name: name, Label: label, Cap: capability, Family: "wsjt", Underlying: []string{"usb", "usbd"}, Input: InputAudio, InputRate: SlotRate,
		Slot: slot, LowHz: low, HighHz: high,
	}
}

// textmodes is the family of the native text decoders (§9.4).
const textmodes = "textmodes"

// textDedupStep rounds the frequency of text decoders in the duplicate
// key: listeners click a few tens of hertz apart on the same signal.
const textDedupStep = 100

// DigitalModes returns the catalogue, in display order.
func DigitalModes() []DigitalMode {
	out := make([]DigitalMode, len(digitalModes))
	for i, m := range digitalModes {
		m.Underlying, m.Variants = slices.Clone(m.Underlying), slices.Clone(m.Variants)
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

	m.Underlying, m.Variants = slices.Clone(m.Underlying), slices.Clone(m.Variants)

	return m, nil
}
