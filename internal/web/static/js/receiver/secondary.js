// The decoder waterfall of the Decoders tab (RX-043, DEC-004, DEC-005): the
// secondary FFT of the listener's text decoder (stream kind fft2), cropped
// to the demodulator's pass band, with the decoder band marked. A click
// moves the decoder there; the offset field does the same from the
// keyboard. The text equivalent of the canvas (its label and the line
// below it, not live) names the band shown, the decoder offset and
// bandwidth, and the strongest signal, updated at most once a second.

import { el, SMALL_BUTTON, TOUCH } from "./dom.js";
import { levelLUT, paletteRGBA } from "./palette.js";

/** Rows of history drawn. */
const ROWS = 96;
/** The text equivalent is updated at most this often (UI-009). */
const SUMMARY_EVERY_MS = 1000;
/** Levels: from below the noise floor to this much above it (dB). */
const FLOOR_MARGIN_DB = 5;
const RANGE_DB = 45;

/** @param {number} hz */
function signed(hz) {
  return `${hz >= 0 ? "+" : "−"}${Math.abs(Math.round(hz))} Hz`;
}

export class SecondaryWaterfall {
  /**
   * @param {EventTarget & {fft2: any, secondary: any, demod: any, device: any, setDecoderOffset: (hz: number) => void}} engine
   */
  constructor(engine) {
    this.engine = engine;
    this.rgba = paletteRGBA("default");
    this.floor = null;
    this.summaryAt = 0;
    /** @type {{hz: number, db: number} | null} */
    this.peak = null;

    this.el = el("section", { class: "flex flex-col gap-2", "aria-labelledby": "rx-fft2-title" });
    this.el.hidden = true;
    const title = el("h3", { id: "rx-fft2-title", class: "font-semibold" }, "Decoder waterfall");
    const wrap = el("div", { class: "relative" });
    this.canvas = /** @type {HTMLCanvasElement} */ (
      el("canvas", { width: "400", height: String(ROWS), class: "block h-24 w-full cursor-crosshair rounded border border-border bg-bg", role: "img" })
    );
    this.mark = el("div", { class: "pointer-events-none absolute inset-y-0 border-x-2 border-accent bg-accent/20", "aria-hidden": "true" });
    wrap.append(this.canvas, this.mark);
    this.summary = el("p", { class: "text-sm text-fg-muted" });

    const form = el("form", { class: "flex flex-wrap items-end gap-2 text-sm" });
    const label = el("label", { for: "rx-fft2-offset", class: "font-medium" }, "Decoder offset (Hz)");
    this.offset = /** @type {HTMLInputElement} */ (el("input", { id: "rx-fft2-offset", type: "number", step: "1", class: `w-28 rounded border border-border px-2 py-1 ${TOUCH}` }));
    const set = el("button", { type: "submit", class: SMALL_BUTTON }, "Set");
    form.append(label, this.offset, set);
    form.addEventListener("submit", (ev) => {
      ev.preventDefault();
      const hz = Number(this.offset.value);
      if (Number.isFinite(hz)) this.engine.setDecoderOffset(hz);
    });
    this.canvas.addEventListener("click", (ev) => {
      const r = this.canvas.getBoundingClientRect();
      if (r.width > 0) this.engine.setDecoderOffset(this.hzAt((ev.clientX - r.left) / r.width));
    });

    this.el.append(title, wrap, this.summary, form);
    this.ctx = this.canvas.getContext("2d");
  }

  /** @returns {{low: number, high: number}} the band shown, Hz from the dial */
  band() {
    const d = this.engine.demod;
    const s = this.engine.fft2;
    const low = Math.max(d?.lowHz ?? s.startHz, s.startHz);
    const high = Math.min(d?.highHz ?? s.startHz + s.spanHz, s.startHz + s.spanHz);
    return high > low ? { low, high } : { low: s.startHz, high: s.startHz + s.spanHz };
  }

  /** @param {number} x 0..1 across the canvas @returns {number} Hz from the dial */
  hzAt(x) {
    const b = this.band();
    return b.low + Math.min(1, Math.max(0, x)) * (b.high - b.low);
  }

  // sync shows the waterfall while the decoder has one, and updates the
  // decoder band mark and the offset field.
  sync() {
    const s = this.engine.fft2;
    const sec = this.engine.secondary;
    const on = !!(s && sec);
    if (this.el.hidden === on) {
      this.el.hidden = !on;
      this.floor = null;
      this.peak = null;
      this.ctx?.clearRect(0, 0, this.canvas.width, this.canvas.height);
    }
    if (!on) return;
    const b = this.band();
    const span = b.high - b.low;
    const left = ((sec.offsetHz - sec.bandwidthHz - b.low) / span) * 100;
    const width = ((2 * sec.bandwidthHz) / span) * 100;
    this.mark.hidden = left + width < 0 || left > 100;
    this.mark.style.left = `${Math.max(0, left)}%`;
    this.mark.style.width = `${Math.max(0.5, Math.min(width, 100 - Math.max(0, left)))}%`;
    if (document.activeElement !== this.offset) this.offset.value = String(sec.offsetHz);
    this.offset.min = String(Math.round(b.low));
    this.offset.max = String(Math.round(b.high));
    this.describe(true);
  }

  /** @param {{bins: Uint8Array, dbMin: number, dbStep: number}} f a secondary FFT line */
  onFFT(f) {
    const s = this.engine.fft2;
    const ctx = this.ctx;
    if (!s || this.el.hidden || !ctx || f.bins.length !== s.size) return;
    const b = this.band();
    const binHz = s.spanHz / s.size;
    const first = Math.max(0, Math.floor((b.low - s.startHz) / binHz));
    const last = Math.min(s.size, Math.ceil((b.high - s.startHz) / binHz));
    const n = last - first;
    if (n <= 1) return;

    // Levels follow the noise floor (the median of the band shown).
    const sorted = Array.from(f.bins.subarray(first, last)).sort((x, y) => x - y);
    const median = f.dbMin + sorted[sorted.length >> 1] * f.dbStep;
    this.floor = this.floor === null ? median : this.floor + (median - this.floor) * 0.1;
    const lut = levelLUT(this.rgba, f.dbMin, f.dbStep, this.floor - FLOOR_MARGIN_DB, this.floor + RANGE_DB);

    if (this.canvas.width !== n) {
      this.canvas.width = n;
      ctx.clearRect(0, 0, n, ROWS);
    } else {
      ctx.drawImage(this.canvas, 0, 0, n, ROWS - 1, 0, 1, n, ROWS - 1);
    }
    const row = ctx.createImageData(n, 1);
    const px = new Uint32Array(row.data.buffer);
    let best = first;
    for (let i = 0; i < n; i++) {
      const q = f.bins[first + i];
      px[i] = lut[q];
      if (q > f.bins[best]) best = first + i;
    }
    ctx.putImageData(row, 0, 0);
    this.peak = { hz: s.startHz + (best + 0.5) * binHz, db: f.dbMin + f.bins[best] * f.dbStep };
    this.describe(false);
  }

  /**
   * describe updates the text equivalent of the canvas, at most once a
   * second unless now.
   * @param {boolean} now
   */
  describe(now) {
    const t = Date.now();
    if (!now && t - this.summaryAt < SUMMARY_EVERY_MS) return;
    this.summaryAt = t;
    const sec = this.engine.secondary;
    if (!sec || !this.engine.fft2) return;
    const b = this.band();
    let text = `Shows ${signed(b.low)} to ${signed(b.high)} from the dial; the decoder listens at ${signed(sec.offsetHz)}, ±${sec.bandwidthHz} Hz.`;
    if (this.peak && this.floor !== null && this.peak.db - this.floor > 10) {
      text += ` Strongest signal at ${signed(this.peak.hz)}, ${Math.round(this.peak.db - this.floor)} dB above the noise.`;
    }
    this.canvas.setAttribute("aria-label", `Decoder waterfall. ${text} Click to move the decoder.`);
    if (this.summary.textContent !== text) this.summary.textContent = text;
  }
}
