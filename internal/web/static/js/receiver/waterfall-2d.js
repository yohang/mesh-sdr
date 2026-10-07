// Canvas 2D waterfall (RX-015, ADR 0015 decision 1): each FFT line is
// reduced to the visible bin window at most one value per pixel column
// (peak-preserving), coloured through a 256-entry level LUT and written as
// one ImageData row into a ring canvas; the ring is blitted to the element
// (two drawImage calls) on requestAnimationFrame. Level, palette, zoom or
// size changes rebuild the ring from the raw lines the engine keeps.

import { decimate } from "./decimate.js";
import { levelLUT, paletteRGBA } from "./palette.js";

/**
 * @typedef {object} WaterfallOptions
 * @property {number} start first visible bin
 * @property {number} count visible bins
 * @property {string} palette scheme name (palette.js)
 * @property {number} minDb lower level
 * @property {number} maxDb upper level
 * @property {number} dbMin u8 dB scale offset of the stream
 * @property {number} dbStep u8 dB scale step of the stream
 */

export class Waterfall2D {
  /** @param {HTMLCanvasElement} canvas */
  constructor(canvas) {
    this.canvas = canvas;
    const ctx = canvas.getContext("2d", { alpha: false });
    if (!ctx) throw new Error("no 2d context");
    this.ctx = ctx;
    this.ring = document.createElement("canvas");
    this.rctx = /** @type {CanvasRenderingContext2D} */ (this.ring.getContext("2d", { alpha: false }));
    /** @type {WaterfallOptions} */
    this.o = { start: 0, count: 1, palette: "default", minDb: -100, maxDb: -20, dbMin: -150, dbStep: 0.5 };
    this.cols = 1;
    this.rows = 1;
    this.w = 0;
    this.dirty = false;
  }

  /**
   * configure applies new options and the current canvas size, then
   * redraws the history (newest last).
   * @param {WaterfallOptions} o @param {Uint8Array[]} history
   */
  configure(o, history) {
    this.o = { ...o };
    this.cols = Math.max(1, Math.min(o.count, this.canvas.width));
    this.rows = Math.max(1, this.canvas.height);
    this.ring.width = this.cols;
    this.ring.height = this.rows;
    this.row = this.rctx.createImageData(this.cols, 1);
    this.row32 = new Uint32Array(this.row.data.buffer);
    this.lut = levelLUT(paletteRGBA(o.palette), o.dbMin, o.dbStep, o.minDb, o.maxDb);
    this.replay(history);
  }

  /** @param {Uint8Array[]} history newest last */
  replay(history) {
    this.rctx.fillStyle = "#000";
    this.rctx.fillRect(0, 0, this.cols, this.rows);
    this.w = 0;
    for (const line of history.slice(-this.rows)) this.pushLine(line);
    this.dirty = true;
  }

  /** @param {Uint8Array} bins */
  pushLine(bins) {
    if (!this.row32 || !this.row) return;
    decimate(bins, this.o.start, this.o.count, this.row32, this.lut);
    this.w = (this.w + this.rows - 1) % this.rows;
    this.rctx.putImageData(this.row, 0, this.w);
    this.dirty = true;
  }

  /** draw blits the ring, newest row at the top; false when nothing changed. */
  draw() {
    if (!this.dirty) return false;
    this.dirty = false;
    const { width, height } = this.canvas;
    const rows = this.rows;
    const ctx = this.ctx;
    const scale = height / rows;
    const h1 = rows - this.w;
    // Zoomed past one bin per pixel: keep bins crisp rather than blurred.
    ctx.imageSmoothingEnabled = this.cols >= width;
    ctx.drawImage(this.ring, 0, this.w, this.cols, h1, 0, 0, width, h1 * scale);
    if (this.w > 0) ctx.drawImage(this.ring, 0, 0, this.cols, this.w, 0, h1 * scale, width, this.w * scale);
    return true;
  }

  destroy() {
    this.ring.width = this.ring.height = 0;
  }
}
