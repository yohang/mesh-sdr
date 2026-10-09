// C ABI shim over the libcsdr++ text decoder modules (ADR 0014): PSK,
// RTTY, SITOR-B, NAVTEX, DSC and CW, as OpenWebRX+ chains them
// (csdr/chain/digimodes.py).

#include "shim_text.h"

#include "shim_stage.hpp"

#include <csdr/agc.hpp>
#include <csdr/baudot.hpp>
#include <csdr/ccir476.hpp>
#include <csdr/ccir493.hpp>
#include <csdr/complex.hpp>
#include <csdr/cw.hpp>
#include <csdr/dbpsk.hpp>
#include <csdr/dsc.hpp>
#include <csdr/filter.hpp>
#include <csdr/fir.hpp>
#include <csdr/navtex.hpp>
#include <csdr/rtty.hpp>
#include <csdr/sitorb.hpp>
#include <csdr/timingrecovery.hpp>
#include <csdr/varicode.hpp>
#include <csdr/window.hpp>

namespace {

using cf = Csdr::complex<float>;
using byte = unsigned char;

// CwStage keeps its decoder to reset it.
class CwStage : public msdr::Stage<cf, byte> {
  public:
    explicit CwStage(Csdr::CwDecoder<cf>* d) : msdr::Stage<cf, byte>(d), decoder(d) {}

    Csdr::CwDecoder<cf>* decoder;
};

}  // namespace

extern "C" {

msdr_stage* msdr_agc_complex_new(void) {
    return msdr::build([&]() -> msdr_stage* { return new msdr::Stage<cf, cf>(new Csdr::Agc<cf>()); });
}

msdr_stage* msdr_timing_recovery_complex_new(unsigned decimation, float loop_gain, float max_error) {
    return msdr::build([&]() -> msdr_stage* {
        return new msdr::Stage<cf, cf>(new Csdr::GardnerTimingRecovery<cf>(decimation, loop_gain, max_error));
    });
}

msdr_stage* msdr_timing_recovery_float_new(unsigned decimation, float loop_gain, float max_error) {
    return msdr::build([&]() -> msdr_stage* {
        return new msdr::Stage<float, float>(new Csdr::GardnerTimingRecovery<float>(decimation, loop_gain, max_error));
    });
}

msdr_stage* msdr_lowpass_float_new(float cutoff, float transition) {
    return msdr::build([&]() -> msdr_stage* {
        Csdr::HammingWindow window;
        return new msdr::Stage<float, float>(new Csdr::FilterModule<float>(new Csdr::LowPassFilter<float>(cutoff, transition, &window)));
    });
}

msdr_stage* msdr_dbpsk_new(void) {
    return msdr::build([&]() -> msdr_stage* { return new msdr::Stage<cf, byte>(new Csdr::DBPskDecoder()); });
}

msdr_stage* msdr_varicode_new(void) {
    return msdr::build([&]() -> msdr_stage* { return new msdr::Stage<byte, byte>(new Csdr::VaricodeDecoder()); });
}

msdr_stage* msdr_rtty_new(int invert) {
    return msdr::build([&]() -> msdr_stage* { return new msdr::Stage<float, byte>(new Csdr::RttyDecoder(invert != 0)); });
}

msdr_stage* msdr_baudot_new(void) {
    return msdr::build([&]() -> msdr_stage* { return new msdr::Stage<byte, byte>(new Csdr::BaudotDecoder()); });
}

msdr_stage* msdr_sitorb_new(unsigned errors_allowed, int invert) {
    return msdr::build([&]() -> msdr_stage* {
        return new msdr::Stage<float, byte>(new Csdr::SitorBDecoder(errors_allowed, invert != 0));
    });
}

msdr_stage* msdr_ccir476_new(void) {
    return msdr::build([&]() -> msdr_stage* { return new msdr::Stage<byte, byte>(new Csdr::Ccir476Decoder()); });
}

msdr_stage* msdr_navtex_new(void) {
    return msdr::build([&]() -> msdr_stage* { return new msdr::Stage<byte, byte>(new Csdr::NavtexDecoder()); });
}

msdr_stage* msdr_ccir493_new(unsigned errors_allowed, int invert) {
    return msdr::build([&]() -> msdr_stage* {
        return new msdr::Stage<float, byte>(new Csdr::Ccir493Decoder(errors_allowed, invert != 0));
    });
}

msdr_stage* msdr_dsc_new(void) {
    return msdr::build([&]() -> msdr_stage* { return new msdr::Stage<byte, byte>(new Csdr::DscDecoder()); });
}

msdr_stage* msdr_cw_new(unsigned sample_rate, int show_cw) {
    return msdr::build([&]() -> msdr_stage* { return new CwStage(new Csdr::CwDecoder<cf>(sample_rate, show_cw != 0)); });
}

int msdr_cw_reset(msdr_stage* s) {
    auto* st = dynamic_cast<CwStage*>(s);
    if (st == nullptr) return -1;
    try {
        st->decoder->reset();
    } catch (...) {
        return -1;
    }
    return 0;
}

}  // extern "C"
