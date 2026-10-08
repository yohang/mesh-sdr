// C ABI over the libcsdr++ image decoders (SSTV, FAX): float audio in, a
// BMP byte stream out (header, then one row per scanline). Same rules as
// shim.h: opaque handles, pointer + count buffers, no exception escapes.
#ifndef MESHSDR_CSDR_SHIM_IMAGE_H
#define MESHSDR_CSDR_SHIM_IMAGE_H

#include <stddef.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef struct msdr_image msdr_image;

// Options of msdr_fax_new (Csdr::FaxDecoder OPT_*).
enum { MSDR_FAX_AM = 0x1, MSDR_FAX_POST = 0x2, MSDR_FAX_COLOR = 0x4 };

// Constructors return NULL when the module cannot be built.
msdr_image* msdr_sstv_new(unsigned sample_rate);
msdr_image* msdr_fax_new(unsigned sample_rate, unsigned lpm, unsigned max_lines, unsigned options);

// msdr_image_process appends n_in samples to the decoder's carry, runs it
// while it can make progress and writes at most cap_out bytes of its BMP
// stream to out. It returns the number of bytes written, or -1 on failure.
long msdr_image_process(msdr_image* s, const float* in, size_t n_in, unsigned char* out, size_t cap_out);

// msdr_image_pending returns the number of samples kept in the carry.
size_t msdr_image_pending(msdr_image* s);

// msdr_sstv_canary_intact reports whether the second half of the slack of
// an SSTV decoder is untouched (tests).
int msdr_sstv_canary_intact(msdr_image* s);

void msdr_image_free(msdr_image* s);

#ifdef __cplusplus
}
#endif

#endif
