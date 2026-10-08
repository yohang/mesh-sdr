// Audio output of the receiver engine (ADR 0015 decisions 3, 4 and 7). One
// long-lived AudioContext at the device default rate, created on a user
// gesture (RX-004), playing decoded mono PCM through:
//   - "worklet": the AudioWorklet jitter buffer (rx-worklet.js), whose exact
//     URL the CSP allows (ADR 0015 decision 5);
//   - "scheduled": AudioBufferSourceNodes scheduled from the main thread, the
//     degraded path when the worklet cannot load.
// Volume and mute (RX-026) are a gain node after either path.
//
// Recording (REC-001) taps the PCM actually played, before the volume: the
// worklet's output (context rate, with its concealment and silences), or
// on the degraded path the scheduled samples (at the rate of the first
// one, later rates resampled to it). Samples are kept as 16-bit integers.

const WORKLET_URL = new URL("./rx-worklet.js", import.meta.url).href;

export class AudioPlayer {
  constructor() {
    /** @type {AudioContext | null} */
    this.ctx = null;
    /** @type {AudioWorkletNode | null} */
    this.node = null;
    /** @type {GainNode | null} */
    this.gain = null;
    this.mode = "";
    this.volume = 0.8;
    this.muted = false;
    // Jitter buffer stats (rx-worklet.js posts them every 250 ms): the
    // receiver shows them (RX-035) and raises its audio chip on under- and
    // overruns (UI-022).
    this.stats = { bufferedMs: 0, targetMs: 0, underruns: 0, overruns: 0, droppedMs: 0, concealedMs: 0 };
    this.nextTime = 0;
    /** @type {{rate: number, chunks: Int16Array[], samples: number} | null} */
    this.rec = null;
  }

  get running() {
    return this.ctx?.state === "running";
  }

  /** start creates or resumes the context; it must run in a user gesture. */
  async start() {
    if (this.ctx) {
      await this.ctx.resume();
      return;
    }
    const ctx = new AudioContext({ latencyHint: "interactive" });
    this.ctx = ctx;
    this.gain = ctx.createGain();
    this.gain.connect(ctx.destination);
    this.applyGain();
    const resumed = ctx.resume(); // keep the gesture: resume before awaiting
    try {
      await ctx.audioWorklet.addModule(WORKLET_URL);
      this.node = new AudioWorkletNode(ctx, "msdr-rx-player", { numberOfInputs: 0, outputChannelCount: [1] });
      this.node.port.onmessage = (e) => {
        if (e.data?.rec) this.keep(e.data.rec, ctx.sampleRate);
        else Object.assign(this.stats, e.data);
      };
      this.node.connect(this.gain);
      this.mode = "worklet";
    } catch {
      this.mode = "scheduled";
    }
    await resumed;
  }

  async suspend() {
    await this.ctx?.suspend();
  }

  /** @param {number} v 0..1 */
  setVolume(v) {
    this.volume = Math.min(1, Math.max(0, v));
    this.applyGain();
  }

  /** @param {boolean} m */
  setMuted(m) {
    this.muted = m;
    this.applyGain();
  }

  applyGain() {
    if (this.gain) this.gain.gain.value = this.muted ? 0 : this.volume;
  }

  /**
   * @param {Float32Array} samples mono PCM, transferred to the worklet
   * @param {number} rate stream sample rate
   */
  push(samples, rate) {
    if (!this.ctx || this.ctx.state !== "running") return;
    if (this.node) {
      this.node.port.postMessage({ type: "pcm", samples, rate }, [samples.buffer]);
      return;
    }
    this.schedule(samples, rate);
  }

  /** @param {number} ms missing audio, concealed with silence */
  gap(ms) {
    if (this.node) this.node.port.postMessage({ type: "gap", ms });
    else if (this.ctx) this.nextTime += ms / 1000;
  }

  /** flush drops the buffered audio (stream reset, new demodulator). */
  flush() {
    if (this.node) this.node.port.postMessage({ type: "flush" });
    this.nextTime = 0;
  }

  get recording() {
    return this.rec !== null;
  }

  /** @returns {number} seconds recorded so far */
  recordedSeconds() {
    return this.rec && this.rec.rate ? this.rec.samples / this.rec.rate : 0;
  }

  /** startRecording starts keeping the played PCM; the audio must be on. */
  startRecording() {
    if (!this.ctx || this.rec) return;
    this.rec = { rate: this.node ? this.ctx.sampleRate : 0, chunks: [], samples: 0 };
    this.node?.port.postMessage({ type: "record", on: true });
  }

  /**
   * stopRecording returns what was recorded since startRecording; the
   * worklet's last block arrives after a short delay.
   * @returns {Promise<{rate: number, chunks: Int16Array[], samples: number} | null>}
   */
  async stopRecording() {
    const rec = this.rec;
    if (!rec) return null;
    if (this.node) {
      this.node.port.postMessage({ type: "record", on: false });
      // The last block comes back on the worklet's next message turn.
      await new Promise((r) => setTimeout(r, 50));
    }
    this.rec = null;
    return rec;
  }

  /**
   * keep adds played samples to the recording, as 16-bit PCM.
   * @param {Float32Array} samples @param {number} rate
   */
  keep(samples, rate) {
    const rec = this.rec;
    if (!rec) return;
    if (!rec.rate) rec.rate = rate;
    const x = rate === rec.rate ? samples : resample(samples, rate, rec.rate);
    const out = new Int16Array(x.length);
    for (let i = 0; i < x.length; i++) {
      const v = Math.max(-1, Math.min(1, x[i]));
      out[i] = v < 0 ? v * 0x8000 : v * 0x7fff;
    }
    rec.chunks.push(out);
    rec.samples += out.length;
  }

  /** @param {Float32Array} samples @param {number} rate */
  schedule(samples, rate) {
    if (this.rec) this.keep(samples, rate);
    const ctx = /** @type {AudioContext} */ (this.ctx);
    const buf = ctx.createBuffer(1, samples.length, rate);
    buf.copyToChannel(samples, 0);
    const src = ctx.createBufferSource();
    src.buffer = buf;
    src.connect(/** @type {GainNode} */ (this.gain));
    const now = ctx.currentTime;
    if (this.nextTime < now) {
      if (this.nextTime > 0) this.stats.underruns++;
      this.nextTime = now + 0.06; // re-prime at the 60 ms target
    }
    src.start(this.nextTime);
    this.nextTime += buf.duration;
    this.stats.bufferedMs = (this.nextTime - now) * 1000;
    this.stats.targetMs = 60;
  }
}

/**
 * resample converts mono samples from one rate to another (linear).
 * @param {Float32Array} x @param {number} from @param {number} to
 * @returns {Float32Array}
 */
function resample(x, from, to) {
  const n = Math.max(1, Math.round((x.length * to) / from));
  const y = new Float32Array(n);
  const step = from / to;
  for (let i = 0; i < n; i++) {
    const p = i * step;
    const j = Math.floor(p);
    const a = x[Math.min(j, x.length - 1)];
    const b = x[Math.min(j + 1, x.length - 1)];
    y[i] = a + (b - a) * (p - j);
  }
  return y;
}
