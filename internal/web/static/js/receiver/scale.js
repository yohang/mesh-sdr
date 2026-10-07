// Frequency scale (RX-019): tick labels for the visible span and the filter
// envelope of the tuned demodulator. Colours come from the theme tokens, so
// they meet the contrast targets in both themes (UI-008, UI-009).

import { readToken } from "../tokens.js";

// Label steps in Hz: 1, 2 and 5 per decade.
const STEPS = [];
for (let d = 1; d <= 1e9; d *= 10) STEPS.push(d, 2 * d, 5 * d);

const MIN_LABEL_CSS_PX = 90;

export class FreqScale {
  /** @param {HTMLCanvasElement} canvas */
  constructor(canvas) {
    this.canvas = canvas;
    this.ctx = /** @type {CanvasRenderingContext2D} */ (canvas.getContext("2d"));
    this.readColors();
  }

  readColors() {
    this.fg = readToken("color-fg") || "#ddd";
    this.muted = readToken("color-fg-muted") || "#999";
    this.accent = readToken("color-accent") || "#58a6ff";
    this.bg = readToken("color-surface") || "#000";
    this.font = readToken("font-sans") || "sans-serif";
  }

  /**
   * draw renders the scale of [startHz, startHz + spanHz) and the envelope
   * [lowHz, highHz] of the tuned demodulator, when there is one.
   * @param {number} startHz @param {number} spanHz
   * @param {{lowHz: number, highHz: number} | null} envelope
   */
  draw(startHz, spanHz, envelope) {
    const { width: w, height: h } = this.canvas;
    const c = this.ctx;
    const dpr = Math.min(window.devicePixelRatio || 1, 2);
    c.fillStyle = this.bg;
    c.fillRect(0, 0, w, h);
    if (!(spanHz > 0)) return;
    const x = (/** @type {number} */ hz) => ((hz - startHz) / spanHz) * w;

    if (envelope) {
      // Envelope: a bracket over the pass band, at the top of the scale.
      const x0 = x(envelope.lowHz);
      const x1 = x(envelope.highHz);
      const top = 2 * dpr;
      const foot = 7 * dpr;
      c.strokeStyle = this.accent;
      c.lineWidth = 2 * dpr;
      c.beginPath();
      c.moveTo(x0 - 3 * dpr, foot);
      c.lineTo(x0, top);
      c.lineTo(x1, top);
      c.lineTo(x1 + 3 * dpr, foot);
      c.stroke();
    }

    const minStep = (spanHz * MIN_LABEL_CSS_PX * dpr) / w;
    const step = STEPS.find((s) => s >= minStep) ?? STEPS[STEPS.length - 1];
    const decimals = Math.max(0, Math.min(6, 6 - Math.floor(Math.log10(step))));
    c.font = `${11 * dpr}px ${this.font}`;
    c.textAlign = "center";
    c.textBaseline = "bottom";
    c.lineWidth = dpr;
    for (let f = Math.ceil(startHz / step) * step; f < startHz + spanHz; f += step) {
      const px = Math.round(x(f)) + 0.5;
      c.strokeStyle = this.muted;
      c.beginPath();
      c.moveTo(px, 9 * dpr);
      c.lineTo(px, 13 * dpr);
      c.stroke();
      const label = (f / 1e6).toFixed(decimals);
      const half = c.measureText(label).width / 2;
      if (px - half < 0 || px + half > w) continue; // clipped at an edge
      c.fillStyle = this.fg;
      c.fillText(label, px, h - dpr);
    }
  }
}
