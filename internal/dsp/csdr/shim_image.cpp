// C ABI shim over the libcsdr++ image decoders (ADR 0014): SstvDecoder and
// FaxDecoder, driven synchronously like the stages of shim.cpp.

#include "shim_image.h"

#include <csdr/fax.hpp>
#include <csdr/module.hpp>
#include <csdr/reader.hpp>
#include <csdr/sstv.hpp>
#include <csdr/writer.hpp>

#include <algorithm>
#include <cstring>
#include <mutex>
#include <new>
#include <vector>

// The FFTW planner lock of shim.cpp: SstvDecoder creates and destroys its
// plans in its constructor and destructor.
extern std::mutex fftwPlanner;

namespace {

using ImageModule = Csdr::Module<float, unsigned char>;

// SstvDecoder keeps the chroma of the previous scanline in a buffer of
// 640 pixels, but PD-290 lines are 800 pixels wide: a PD-290 header (VIS
// 94, from any signal) makes it write 160 bytes past the end of the object.
// The decoder is built in storage that has room for them.
constexpr size_t sstvSlack = 1024;

class FloatCarry : public Csdr::Reader<float> {
  public:
    size_t available() override { return buf.size() - pos; }
    float* getReadPointer() override { return buf.data() + pos; }
    // A module never moves past the data it was given.
    void advance(size_t n) override { pos += std::min(n, buf.size() - pos); }
    void wait() override {}
    void unblock() override {}

    void append(const float* in, size_t n) {
        if (pos > 0) {
            buf.erase(buf.begin(), buf.begin() + static_cast<std::ptrdiff_t>(pos));
            pos = 0;
        }
        buf.insert(buf.end(), in, in + n);
    }

  private:
    std::vector<float> buf;
    size_t pos = 0;
};

class ByteSpan : public Csdr::Writer<unsigned char> {
  public:
    size_t writeable() override { return cap - n; }
    unsigned char* getWritePointer() override { return out + n; }
    void advance(size_t k) override { n += k; }

    void reset(unsigned char* o, size_t c) {
        out = o;
        cap = c;
        n = 0;
    }
    size_t written() const { return n; }

  private:
    unsigned char* out = nullptr;
    size_t cap = 0;
    size_t n = 0;
};

}  // namespace

struct msdr_image {
    ImageModule* module = nullptr;
    // storage is the placement storage of an SSTV decoder (nullptr for a
    // decoder built with new).
    void* storage = nullptr;
    FloatCarry reader;
    ByteSpan writer;

    void attach() {
        module->setReader(&reader);
        module->setWriter(&writer);
    }

    ~msdr_image() {
        if (module == nullptr) return;
        std::lock_guard<std::mutex> lock(fftwPlanner);
        if (storage != nullptr) {
            module->~ImageModule();
            ::operator delete(storage);
        } else {
            delete module;
        }
    }
};

extern "C" {

msdr_image* msdr_sstv_new(unsigned sample_rate) {
    void* storage = nullptr;
    msdr_image* s = nullptr;
    try {
        s = new msdr_image();
        storage = ::operator new(sizeof(Csdr::SstvDecoder<float>) + sstvSlack);
        std::memset(storage, 0, sizeof(Csdr::SstvDecoder<float>) + sstvSlack);
        {
            std::lock_guard<std::mutex> lock(fftwPlanner);
            s->module = new (storage) Csdr::SstvDecoder<float>(sample_rate, 0);
        }
        s->storage = storage;
        s->attach();
        return s;
    } catch (...) {
        if (s != nullptr && s->module == nullptr) ::operator delete(storage);
        delete s;
        return nullptr;
    }
}

msdr_image* msdr_fax_new(unsigned sample_rate, unsigned lpm, unsigned max_lines, unsigned options) {
    try {
        unsigned opt = 0;
        if (options & MSDR_FAX_AM) opt |= Csdr::FaxDecoder<float>::OPT_AM;
        if (options & MSDR_FAX_POST) opt |= Csdr::FaxDecoder<float>::OPT_POST;
        if (options & MSDR_FAX_COLOR) opt |= Csdr::FaxDecoder<float>::OPT_COLOR;
        auto* s = new msdr_image();
        s->module = new Csdr::FaxDecoder<float>(sample_rate, lpm, max_lines, opt, 0);
        s->attach();
        return s;
    } catch (...) {
        return nullptr;
    }
}

long msdr_image_process(msdr_image* s, const float* in, size_t n_in, unsigned char* out, size_t cap_out) {
    try {
        s->reader.append(in, n_in);
        s->writer.reset(out, cap_out);
        while (s->module->canProcess()) {
            size_t avail = s->reader.available();
            size_t done = s->writer.written();
            s->module->process();
            if (s->reader.available() == avail && s->writer.written() == done) {
                break;
            }
        }
        return static_cast<long>(s->writer.written());
    } catch (...) {
        return -1;
    }
}

size_t msdr_image_pending(msdr_image* s) { return s->reader.available(); }

void msdr_image_free(msdr_image* s) {
    try {
        delete s;
    } catch (...) {
    }
}

}  // extern "C"
