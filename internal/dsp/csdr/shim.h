// C ABI over the libcsdr++ modules used by the node (ADR 0014, ADR 0019).
// No C++ type crosses this boundary: a stage is an opaque handle, buffers
// are pointer + item count, and C++ exceptions never escape.
#ifndef MESHSDR_CSDR_SHIM_H
#define MESHSDR_CSDR_SHIM_H

#include <stddef.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef struct msdr_stage msdr_stage;

// Window functions of msdr_fft_new.
enum { MSDR_WINDOW_BLACKMAN = 0, MSDR_WINDOW_HAMMING = 1, MSDR_WINDOW_BOXCAR = 2 };

// Constructors return NULL when the module cannot be built.
msdr_stage* msdr_fft_new(unsigned size, unsigned every, int window);
msdr_stage* msdr_logavgpower_new(unsigned size, unsigned avg, float add_db);
msdr_stage* msdr_shift_new(float rate);
msdr_stage* msdr_fmdemod_new(void);
msdr_stage* msdr_limit_new(float max_amplitude);
msdr_stage* msdr_nfm_deemphasis_new(unsigned sample_rate);
msdr_stage* msdr_agc_new(float reference, float attack, float decay, float max_gain, unsigned hang);
// ratio is output rate / input rate.
msdr_stage* msdr_resampler_new(double ratio);

// msdr_shift_set_rate changes the rate (cycles per sample) of a shift stage.
int msdr_shift_set_rate(msdr_stage* s, float rate);

// msdr_stage_process appends n_in input items to the stage's carry, runs
// the module while it can make progress and writes at most cap_out output
// items to out. It returns the number of output items, or -1 on failure.
// Input the module could not consume yet stays in the carry.
long msdr_stage_process(msdr_stage* s, const void* in, size_t n_in, void* out, size_t cap_out);

// msdr_stage_pending returns the number of input items kept in the carry.
size_t msdr_stage_pending(msdr_stage* s);

void msdr_stage_free(msdr_stage* s);

#ifdef __cplusplus
}
#endif

#endif
