// AudioWorklet player for the receiver: an adaptive jitter buffer (60–150 ms,
// TECHNICAL_SPEC §2.3) fed with decoded mono PCM through the port, a linear
// resampler from the stream rate to the context rate, silence concealment for
// gaps (§6.7), and stats posted back every 250 ms. Self-contained: a worklet
// module cannot carry a CSP nonce, so it imports nothing.

const MIN_TARGET_MS = 60;
const MAX_TARGET_MS = 150;

class RxPlayer extends AudioWorkletProcessor {
  constructor() {
    super();
    this.cap = sampleRate * 2; // 2 s ring
    this.ring = new Float32Array(this.cap);
    this.r = 0;
    this.w = 0;
    this.n = 0;
    this.targetMs = MIN_TARGET_MS;
    this.priming = true;
    this.underruns = 0;
    this.droppedMs = 0;
    // Overruns: audio arrived faster than played, the excess was dropped
    // (shrink or full ring); reported for the audio chip (UI-022).
    this.overruns = 0;
    this.concealedMs = 0;
    this.lastUnderrun = currentTime;
    this.lastStats = 0;
    // Resampler state: fractional position between the last two inputs.
    this.inRate = sampleRate;
    this.pos = 0;
    this.prev = 0;
    this.port.onmessage = (e) => this.onMessage(e.data);
  }

  onMessage(m) {
    if (m.type === "pcm") {
      if (m.rate !== this.inRate) {
        this.inRate = m.rate;
        this.pos = 0;
      }
      this.resampleIn(m.samples);
    } else if (m.type === "gap") {
      // Concealment: silence for the missing duration, capped at the target
      // so a long gap never builds a backlog (no time-stretching).
      const ms = Math.min(m.ms, this.targetMs);
      const k = Math.round((ms * sampleRate) / 1000);
      for (let i = 0; i < k; i++) this.put(0);
      this.concealedMs += ms;
    } else if (m.type === "flush") {
      this.r = this.w = this.n = 0;
      this.priming = true;
    }
  }

  put(v) {
    if (this.n === this.cap) {
      this.r = (this.r + 1) % this.cap;
      this.n--;
    }
    this.ring[this.w] = v;
    this.w = (this.w + 1) % this.cap;
    this.n++;
  }

  resampleIn(x) {
    const step = this.inRate / sampleRate;
    if (step === 1) {
      for (let i = 0; i < x.length; i++) this.put(x[i]);
      return;
    }
    let pos = this.pos;
    let prev = this.prev;
    for (let i = 0; i < x.length; i++) {
      const cur = x[i];
      while (pos < 1) {
        this.put(prev + (cur - prev) * pos);
        pos += step;
      }
      pos -= 1;
      prev = cur;
    }
    this.pos = pos;
    this.prev = prev;
  }

  bufferedMs() {
    return (this.n * 1000) / sampleRate;
  }

  process(_inputs, outputs) {
    const out = outputs[0][0];
    const len = out.length;
    const ms = this.bufferedMs();

    // Shrink: drop the excess above target + 60 ms (jitter calmed down).
    if (!this.priming && ms > this.targetMs + 60) {
      const drop = Math.round(((ms - this.targetMs) * sampleRate) / 1000);
      this.r = (this.r + drop) % this.cap;
      this.n -= drop;
      this.droppedMs += (drop * 1000) / sampleRate;
      this.overruns++;
    }
    // Decay the target towards 60 ms after 5 s without underrun.
    if (currentTime - this.lastUnderrun > 5 && this.targetMs > MIN_TARGET_MS) {
      this.targetMs = Math.max(MIN_TARGET_MS, this.targetMs - 10);
      this.lastUnderrun = currentTime;
    }
    if (this.priming && this.bufferedMs() >= this.targetMs) this.priming = false;

    if (this.priming || this.n < len) {
      if (!this.priming) {
        this.underruns++;
        this.priming = true;
        this.targetMs = Math.min(MAX_TARGET_MS, this.targetMs + 20);
        this.lastUnderrun = currentTime;
      }
      out.fill(0);
    } else {
      for (let i = 0; i < len; i++) {
        out[i] = this.ring[this.r];
        this.r = (this.r + 1) % this.cap;
      }
      this.n -= len;
    }
    for (let c = 1; c < outputs[0].length; c++) outputs[0][c].set(out);

    if (currentTime - this.lastStats >= 0.25) {
      this.lastStats = currentTime;
      this.port.postMessage({
        bufferedMs: this.bufferedMs(),
        targetMs: this.targetMs,
        underruns: this.underruns,
        droppedMs: this.droppedMs,
        overruns: this.overruns,
        concealedMs: this.concealedMs,
        priming: this.priming,
        t: currentTime,
      });
    }
    return true;
  }
}

registerProcessor("msdr-rx-player", RxPlayer);
