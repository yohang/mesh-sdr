// The synchronous stage of the C ABI shim (ADR 0014), shared by the shim
// files (shim.cpp, shim_text.cpp): a module reads a linear carry and
// writes into the caller's buffer, driven by the calling goroutine. The
// libcsdr framework (AsyncRunner threads, mmap ring buffers) is not used.
#ifndef MESHSDR_CSDR_SHIM_STAGE_HPP
#define MESHSDR_CSDR_SHIM_STAGE_HPP

#include "shim.h"

#include <csdr/module.hpp>
#include <csdr/reader.hpp>
#include <csdr/writer.hpp>

#include <algorithm>
#include <cstddef>
#include <mutex>
#include <vector>

// The FFTW planner (plan creation and destruction) is not thread-safe;
// fftwf_execute is. Every construction and deletion of a module that plans
// an FFT takes this lock (defined in shim.cpp).
extern std::mutex fftwPlanner;

struct msdr_stage {
    virtual ~msdr_stage() = default;
    virtual long process(const void* in, size_t nIn, void* out, size_t capOut) = 0;
    virtual size_t pending() = 0;
};

namespace msdr {

template <typename T>
class CarryReader : public Csdr::Reader<T> {
  public:
    size_t available() override { return buf.size() - pos; }
    T* getReadPointer() override { return buf.data() + pos; }
    // A module never moves past the data it was given.
    void advance(size_t n) override { pos += std::min(n, buf.size() - pos); }
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
    void advance(size_t k) override { n += std::min(k, cap - n); }

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

// Stage owns a module and drives it while it makes progress. Modules that
// write without checking writeable() (the character decoders) must be given
// an output at least as long as their input: the Go wrappers do. fft: the
// module plans an FFT (its deletion takes fftwPlanner).
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
        long n = static_cast<long>(writer.written());
        // No pointer to the caller's buffer outlives the call.
        writer.reset(nullptr, 0);
        return n;
    }

    size_t pending() override { return reader.available(); }

    Csdr::Module<T, U>* module;

  private:
    CarryReader<T> reader;
    SpanWriter<U> writer;
    bool fft;
};

template <typename F>
msdr_stage* build(F f) {
    try {
        return f();
    } catch (...) {
        return nullptr;
    }
}

}  // namespace msdr

#endif
