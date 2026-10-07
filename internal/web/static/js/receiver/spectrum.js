// Spectrum line with peak hold (RX-016), Canvas 2D, redrawn every 150 ms by
// the island. It shows the visible bin window (zoom), max per pixel column so
// narrow carriers survive, and the tuned frequency with its pass band.
// Colours come from the theme tokens (UI-008), re-read on theme change.

import { readToken } from "../tokens.js";
import { decimate } from "./decimate.js";

/**
 * @typedef {object} SpectrumOptions
 * @property {number} start first visible bin
 * @property {number} count visible bins
 * @property {number} minDb lower level
 * @property {number} maxDb upper level
 * @property {number} dbMin u8 dB scale offset of the stream
 * @property {number} dbStep u8 dB scale step of the stream
 */

/**
 * @typedef {object} Tuning positions as fractions of the canvas width
 * @property {number} center tuned frequency
 * @property {number} low lower pass-band edge
 * @property {number} high upper pass-band edge
 */

export class Spectrum {
  /** @param {HTMLCanvasElement} canvas */
  constructor(canvas) {
    this.canvas = canvas;
    this.ctx = /** @type {CanvasRenderingContext2D} */ (canvas.getContext("2d"));
    /** @type {SpectrumOptions} */
    this.o = { start: 0, count: 1, minDb: -100, maxDb: -20, dbMin: -150, dbStep: 0.5 };
    /** @type {Uint8Array | null} */
    this.last = null;
    /** @type {Uint8Array | null} */
    this.peak = null;
    this.readColors();
  }

  readColors() {
    this.fg = readToken("color-fg") || "#ddd";
    this.muted = readToken("color-fg-muted") || "#999";
    this.accent = readToken("color-accent") || "#58a6ff";
    this.bg = readToken("color-surface") || "#000";
  }

  /** @param {SpectrumOptions} o */
  configure(o) {
    this.o = { ...o };
  }

  /** @param {Uint8Array} bins */
  push(bins) {
    if (!this.peak || this.peak.length !== bins.length) this.peak = new Uint8Array(bins.length);
    const p = this.peak;
    for (let i = 0; i < bins.length; i++) {
      const v = p[i] > 0 ? p[i] - 1 : 0; // slow decay
      p[i] = bins[i] > v ? bins[i] : v;
    }
    this.last = bins;
  }

  /** @param {Tuning | null} tune */
  draw(tune) {
    const { width: w, height: h } = this.canvas;
    const c = this.ctx;
    c.fillStyle = this.bg;
    c.fillRect(0, 0, w, h);
    if (tune) {
      c.globalAlpha = 0.25;
      c.fillStyle = this.accent;
      c.fillRect(tune.low * w, 0, Math.max(1, (tune.high - tune.low) * w), h);
      c.globalAlpha = 1;
    }
    if (this.last && this.peak) {
      const o = this.o;
      const range = Math.max(1, o.maxDb - o.minDb);
      const cols = new Uint8Array(w);
      const dpr = Math.min(window.devicePixelRatio || 1, 2);
      c.lineWidth = dpr;
      for (const [line, color] of /** @type {[Uint8Array, string][]} */ ([
        [this.peak, this.muted],
        [this.last, this.fg],
      ])) {
        decimate(line, o.start, o.count, cols);
        c.beginPath();
        for (let x = 0; x < w; x++) {
          const y = h - ((o.dbMin + cols[x] * o.dbStep - o.minDb) / range) * h;
          if (x === 0) c.moveTo(x, y);
          else c.lineTo(x, y);
        }
        c.strokeStyle = color;
        c.stroke();
      }
    }
    if (tune) {
      c.fillStyle = this.accent;
      c.fillRect(Math.round(tune.center * w), 0, Math.max(1, Math.round(window.devicePixelRatio || 1)), h);
    }
  }
}
