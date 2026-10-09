// C ABI over the libcsdr++ modules of the native text decoders (DEC-006 to
// DEC-012, MAR-002, MAR-003): PSK, RTTY, SITOR-B, NAVTEX, DSC and CW. The
// stages are driven by msdr_stage_process and released by msdr_stage_free
// (shim.h). Decoders output bytes (bits, codes or characters).
#ifndef MESHSDR_CSDR_SHIM_TEXT_H
#define MESHSDR_CSDR_SHIM_TEXT_H

#include "shim.h"

#ifdef __cplusplus
extern "C" {
#endif

// Csdr::Agc<complex<float>> with the libcsdr++ defaults (reference 0.8,
// attack 0.1, decay 0.001, maximum gain 65535, hang 200 samples).
msdr_stage* msdr_agc_complex_new(void);
// Csdr::GardnerTimingRecovery on complex or float samples: one output
// sample per symbol of decimation input samples.
msdr_stage* msdr_timing_recovery_complex_new(unsigned decimation, float loop_gain, float max_error);
msdr_stage* msdr_timing_recovery_float_new(unsigned decimation, float loop_gain, float max_error);
// Csdr::LowPassFilter<float> (FIR, Hamming window); cutoff and transition
// are relative to the sample rate.
msdr_stage* msdr_lowpass_float_new(float cutoff, float transition);
// Csdr::DBPskDecoder: complex symbols to bits.
msdr_stage* msdr_dbpsk_new(void);
// Csdr::VaricodeDecoder: bits to characters.
msdr_stage* msdr_varicode_new(void);
// Csdr::RttyDecoder: float symbols to 5-bit Baudot codes.
msdr_stage* msdr_rtty_new(int invert);
// Csdr::BaudotDecoder: Baudot codes to characters.
msdr_stage* msdr_baudot_new(void);
// Csdr::SitorBDecoder: float symbols to CCIR 476 codes (FEC applied).
msdr_stage* msdr_sitorb_new(unsigned errors_allowed, int invert);
// Csdr::Ccir476Decoder: CCIR 476 codes to characters.
msdr_stage* msdr_ccir476_new(void);
// Csdr::NavtexDecoder: characters to the NAVTEX messages only (ZCZC
// header line to NNNN), copied unchanged.
msdr_stage* msdr_navtex_new(void);
// Csdr::Ccir493Decoder: float symbols to CCIR 493 (DSC) symbols, the
// time diversity (DX/RX) applied; errors_allowed invalid symbols in a row
// resync it.
msdr_stage* msdr_ccir493_new(unsigned errors_allowed, int invert);
// Csdr::DscDecoder: CCIR 493 symbols to one JSON line per DSC call. It
// writes more than it reads (a call of about 20 symbols is up to 400
// bytes) and checks the room it has: it needs at least 256 bytes free.
msdr_stage* msdr_dsc_new(void);
// Csdr::CwDecoder<complex<float>> at sample_rate; show_cw also prints the
// dots and dashes.
msdr_stage* msdr_cw_new(unsigned sample_rate, int show_cw);

// msdr_cw_reset forgets the timing a CW decoder learnt (dial change).
int msdr_cw_reset(msdr_stage* s);

#ifdef __cplusplus
}
#endif

#endif
