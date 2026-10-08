// C ABI shim over libcsdr++ (ADR 0014, ADR 0019).
//
// The libcsdr framework (AsyncRunner threads, mmap ring buffers) is not used:
// each stage owns a linear carry buffer as its Reader and writes into the
// caller's buffer through a span Writer, and the module's canProcess() /
// process() pair is driven synchronously by the calling goroutine.

#include "shim.h"

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

// The FFTW planner (plan creation and destruction) is not thread-safe;
// fftwf_execute is. Every Fft construction and deletion takes this lock
// (shim_image.cpp too).
std::mutex fftwPlanner;

namespace {

template <typename T>
class CarryReader : public Csdr::Reader<T> {
  public:
    size_t available() override { return buf.size() - pos; }
    T* getReadPointer() override { return buf.data() + pos; }
    void advance(size_t n) override { pos += n; }
    void wait() override {}
    void unblock() override {}

    void append(const T* in, size_t n) {
        if (pos > 0) {
            buf.erase(buf.begin(), buf.begin() + static_cast<std::ptrdiff_t>(pos));
            pos = 0;
        }
        buf.insert(buf.end(), in, in + n);
    }

  private:
    std::vector<T> buf;
    size_t pos = 0;
};

template <typename U>
class SpanWriter : public Csdr::Writer<U> {
  public:
    size_t writeable() override { return cap - n; }
    U* getWritePointer() override { return out + n; }
    void advance(size_t k) override { n += k; }

    void reset(U* o, size_t c) {
        out = o;
        cap = c;
        n = 0;
    }
    size_t written() const { return n; }

  private:
    U* out = nullptr;
    size_t cap = 0;
    size_t n = 0;
};

}  // namespace

struct msdr_stage {
    virtual ~msdr_stage() = default;
    virtual long process(const void* in, size_t nIn, void* out, size_t capOut) = 0;
    virtual size_t pending() = 0;
};

namespace {

template <typename T, typename U>
class Stage : public msdr_stage {
  public:
    explicit Stage(Csdr::Module<T, U>* m, bool fft = false) : module(m), fft(fft) {
        module->setReader(&reader);
        module->setWriter(&writer);
    }

    ~Stage() override {
        if (fft) {
            std::lock_guard<std::mutex> lock(fftwPlanner);
            delete module;
        } else {
            delete module;
        }
    }

    long process(const void* in, size_t nIn, void* out, size_t capOut) override {
        reader.append(static_cast<const T*>(in), nIn);
        writer.reset(static_cast<U*>(out), capOut);
        while (module->canProcess()) {
            size_t avail = reader.available();
            size_t done = writer.written();
            module->process();
            if (reader.available() == avail && writer.written() == done) {
                break;
            }
        }
        return static_cast<long>(writer.written());
    }

    size_t pending() override { return reader.available(); }

    Csdr::Module<T, U>* module;

  private:
    CarryReader<T> reader;
    SpanWriter<U> writer;
    bool fft;
};

using cf = Csdr::complex<float>;

// NoiseStage keeps the filter of its module to change the threshold.
class NoiseStage : public Stage<float, float> {
  public:
    explicit NoiseStage(Csdr::NoiseFilter<float>* f) : Stage<float, float>(new Csdr::FilterModule<float>(f), true), filter(f) {}

    Csdr::NoiseFilter<float>* filter;
};

template <typename F>
msdr_stage* build(F f) {
    try {
        return f();
    } catch (...) {
        return nullptr;
    }
}

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
