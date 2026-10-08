// Bandplan ribbon (RX-029): the bands of the hub's bandplan.region
// (GET /api/v1/bandplan, read with the bookmarks of the band by
// bookmarks.js) drawn under the frequency scale, aligned with it. A band's
// background comes from its first tag (hamradio, broadcast, public,
// service), theme tokens whose text contrast is tested; its name is the
// text. The ribbon is decorative for assistive technologies: the Info tab
// says the band of the tuned frequency (bandAt).
//
// Hidden by default. Its visibility is per-session runtime state (the
// engine's display settings, kept across navigation), remembered by this
// browser as a convenience (localStorage).

const VISIBLE_KEY = "msdr.receiver.bandplan";
const TAGS = new Set(["hamradio", "broadcast", "public", "service"]);

/**
 * @typedef {object} Band a band of the band plan
 * @property {string} name
 * @property {number} low Hz
 * @property {number} high Hz
 * @property {string[]} tags
 */

/** @returns {boolean} the visibility this browser remembers */
export function rememberedVisible() {
  try {
    return localStorage.getItem(VISIBLE_KEY) === "on";
  } catch {
    return false;
  }
}

/** @param {boolean} on */
export function rememberVisible(on) {
  try {
    localStorage.setItem(VISIBLE_KEY, on ? "on" : "off");
  } catch {
    // Storage blocked: not remembered.
  }
}

export class BandplanRibbon {
  constructor() {
    this.el = document.createElement("div");
    this.el.className = "band-ribbon";
    this.el.setAttribute("aria-hidden", "true");
    /** @type {Band[]} */
    this.bands = [];
    this.key = "";
  }

  /** @param {Band[]} bands */
  setBands(bands) {
    this.bands = bands;
    this.key = "";
  }

  /**
   * bandAt returns the names of the bands that hold hz.
   * @param {number} hz
   * @returns {string[]}
   */
  bandAt(hz) {
    return this.bands.filter((b) => hz >= b.low && hz <= b.high).map((b) => b.name);
  }

  /**
   * draw places the bands of the visible span [startHz, startHz + spanHz).
   * @param {number} startHz @param {number} spanHz
   */
  draw(startHz, spanHz) {
    if (this.el.hidden) return;
    const width = this.el.clientWidth;
    const key = `${startHz}|${spanHz}|${width}`;
    if (key === this.key || !(spanHz > 0) || width === 0) return;
    this.key = key;
    const x = (/** @type {number} */ hz) => ((hz - startHz) / spanHz) * width;
    const segs = [];
    for (const b of this.bands) {
      const x0 = Math.max(0, x(b.low));
      const x1 = Math.min(width, x(b.high));
      if (x1 <= 0 || x0 >= width || x1 - x0 < 1) continue;
      const tag = b.tags.find((t) => TAGS.has(t));
      const seg = document.createElement("div");
      seg.className = tag ? `band-seg band-${tag}` : "band-seg";
      seg.textContent = b.name;
      seg.style.left = `${x0}px`;
      seg.style.width = `${x1 - x0}px`;
      segs.push(seg);
    }
    this.el.replaceChildren(...segs);
  }
}
