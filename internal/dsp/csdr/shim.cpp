// C ABI shim over libcsdr++ (ADR 0014, ADR 0019).
//
// The libcsdr framework (AsyncRunner threads, mmap ring buffers) is not used:
// each stage owns a linear carry buffer as its Reader and writes into the
// caller's buffer through a span Writer, and the module's canProcess() /
// process() pair is driven synchronously by the calling goroutine.

#include "shim.h"

#include "shim_stage.hpp"

#include <csdr/agc.hpp>
#include <csdr/amdemod.hpp>
#include <csdr/audioresampler.hpp>
#include <csdr/complex.hpp>
#include <csdr/dcblock.hpp>
#include <csdr/deemphasis.hpp>
#include <csdr/fft.hpp>
#include <csdr/fftfilter.hpp>
#include <csdr/filter.hpp>
#include <csdr/fmdemod.hpp>
#include <csdr/limit.hpp>
#include <csdr/logaveragepower.hpp>
#include <csdr/module.hpp>
#include <csdr/noisefilter.hpp>
#include <csdr/reader.hpp>
#include <csdr/realpart.hpp>
#include <csdr/shift.hpp>
#include <csdr/window.hpp>
#include <csdr/writer.hpp>

#include <cstring>
#include <mutex>
#include <new>
#include <vector>

// The FFTW planner lock (shim_stage.hpp; shim_image.cpp takes it too).
std::mutex fftwPlanner;

namespace {

using msdr::build;
using msdr::Stage;

using cf = Csdr::complex<float>;

// NoiseStage keeps the filter of its module to change the threshold.
class NoiseStage : public Stage<float, float> {
  public:
    explicit NoiseStage(Csdr::NoiseFilter<float>* f) : Stage<float, float>(new Csdr::FilterModule<float>(f), true), filter(f) {}

    Csdr::NoiseFilter<float>* filter;
};

}  // namespace

extern "C" {

msdr_stage* msdr_fft_new(unsigned size, unsigned every, int window) {
    return build([&]() -> msdr_stage* {
        Csdr::BlackmanWindow blackman;
        Csdr::HammingWindow hamming;
        Csdr::BoxcarWindow boxcar;
        Csdr::Window* w = &blackman;
        if (window == MSDR_WINDOW_HAMMING) w = &hamming;
        if (window == MSDR_WINDOW_BOXCAR) w = &boxcar;
        std::lock_guard<std::mutex> lock(fftwPlanner);
        return new Stage<cf, cf>(new Csdr::Fft(size, every, w), true);
    });
}

msdr_stage* msdr_logavgpower_new(unsigned size, unsigned avg, float add_db) {
    return build([&]() -> msdr_stage* { return new Stage<cf, float>(new Csdr::LogAveragePower(size, avg, add_db)); });
}

msdr_stage* msdr_shift_new(float rate) {
    return build([&]() -> msdr_stage* { return new Stage<cf, cf>(new Csdr::ShiftMath(rate)); });
}

int msdr_shift_set_rate(msdr_stage* s, float rate) {
    auto* st = dynamic_cast<Stage<cf, cf>*>(s);
    if (st == nullptr) return -1;
    auto* shift = dynamic_cast<Csdr::ShiftMath*>(st->module);
    if (shift == nullptr) return -1;
    shift->setRate(rate);
    return 0;
}

msdr_stage* msdr_fmdemod_new(void) {
    return build([&]() -> msdr_stage* { return new Stage<cf, float>(new Csdr::FmDemod()); });
}

msdr_stage* msdr_limit_new(float max_amplitude) {
    return build([&]() -> msdr_stage* { return new Stage<float, float>(new Csdr::Limit(max_amplitude)); });
}

msdr_stage* msdr_nfm_deemphasis_new(unsigned sample_rate) {
    return build([&]() -> msdr_stage* { return new Stage<float, float>(new Csdr::NfmDeephasis(sample_rate)); });
}

msdr_stage* msdr_agc_new(float reference, float attack, float decay, float max_gain, unsigned hang) {
    return build([&]() -> msdr_stage* {
        auto* agc = new Csdr::Agc<float>();
        agc->setReference(reference);
        agc->setAttack(attack);
        agc->setDecay(decay);
        agc->setMaxGain(max_gain);
        agc->setHangTime(hang);
        return new Stage<float, float>(agc);
    });
}

msdr_stage* msdr_resampler_new(double ratio) {
    return build([&]() -> msdr_stage* { return new Stage<float, float>(new Csdr::AudioResampler(ratio)); });
}

msdr_stage* msdr_amdemod_new(void) {
    return build([&]() -> msdr_stage* { return new Stage<cf, float>(new Csdr::AmDemod()); });
}

msdr_stage* msdr_dcblock_new(void) {
    return build([&]() -> msdr_stage* { return new Stage<float, float>(new Csdr::DcBlock()); });
}

msdr_stage* msdr_realpart_new(void) {
    return build([&]() -> msdr_stage* { return new Stage<cf, float>(new Csdr::Realpart()); });
}

msdr_stage* msdr_bandpass_new(float low, float high, float transition) {
    return build([&]() -> msdr_stage* {
        Csdr::HammingWindow window;
        std::lock_guard<std::mutex> lock(fftwPlanner);
        return new Stage<cf, cf>(new Csdr::FilterModule<cf>(new Csdr::FftBandPassFilter(low, high, transition, &window)), true);
    });
}

msdr_stage* msdr_noisefilter_new(unsigned fft_size, float threshold_db) {
    return build([&]() -> msdr_stage* {
        std::lock_guard<std::mutex> lock(fftwPlanner);
        auto* f = new Csdr::NoiseFilter<float>(fft_size);
        f->setThreshold(threshold_db);
        return new NoiseStage(f);
    });
}

int msdr_noisefilter_set_threshold(msdr_stage* s, float threshold_db) {
    auto* st = dynamic_cast<NoiseStage*>(s);
    if (st == nullptr) return -1;
    st->filter->setThreshold(threshold_db);
    return 0;
}

long msdr_stage_process(msdr_stage* s, const void* in, size_t n_in, void* out, size_t cap_out) {
    try {
        return s->process(in, n_in, out, cap_out);
    } catch (...) {
        return -1;
    }
}

size_t msdr_stage_pending(msdr_stage* s) { return s->pending(); }

void msdr_stage_free(msdr_stage* s) {
    try {
        delete s;
    } catch (...) {
    }
}

}  // extern "C"
