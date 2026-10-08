// Bookmark scanner (BMK-006): client-side, it visits the scannable
// bookmarks of the current band round robin and moves only this visitor's
// own demodulator (engine.tune and setMode); it never switches the preset
// nor moves the centre. At each bookmark it waits for the demodulator to
// settle, then listens: a bookmark is a hit when the smoothed signal level
// (demod.meter) exceeds the squelch level minus 13 dB. On a hit it dwells
// while the signal is present (plus a short hang time), then goes on.
//
// It stops on a manual tune (the tuned frequency is not the one it set),
// a bookmark click (the caller stops it), a preset switch or centre move,
// another device, or a lost connection. State changes are announced with a
// "change" event; "message" ({text}) carries what to tell the visitor.

// A hit is a smoothed level above the squelch level minus this (BMK-006).
const HIT_MARGIN_DB = 13;
// Time for the demodulator to retune and the meter to follow (ms).
const SETTLE_MS = 400;
// Time listened to at each bookmark before moving on (ms).
const LISTEN_MS = 900;
// The signal may drop this long during a dwell before the scan goes on (ms).
const HANG_MS = 2000;
const DWELL_CHECK_MS = 250;
// Exponential smoothing of the meter readings.
const SMOOTHING = 0.35;

/**
 * @typedef {import("./marks.js").Mark} Mark
 */

export class Scanner extends EventTarget {
  /**
   * @param {ReturnType<typeof import("./engine.js").getEngine>} engine
   * @param {{marks: () => Mark[], tune: (m: Mark) => void, squelch: () => number}} deps
   *   marks: the bookmarks in band, sorted; tune: tunes this visitor's
   *   demodulator to a bookmark; squelch: the squelch level in dBFS
   */
  constructor(engine, deps) {
    super();
    this.engine = engine;
    this.deps = deps;
    this.running = false;
    /** @type {Mark | null} */
    this.current = null;
    /** @type {"settle" | "listen" | "dwell" | ""} */
    this.phase = "";
    /** @type {number | null} */
    this.level = null;
    this.expectHz = 0;
    this.lastAbove = 0;
    this.timer = 0;
    this.band = "";
    this.onEngine = (/** @type {Event} */ ev) => this.engineChanged(/** @type {CustomEvent} */ (ev).detail);
    engine.addEventListener("change", this.onEngine);
  }

  destroy() {
    this.stop("");
    this.engine.removeEventListener("change", this.onEngine);
  }

  /** bandKey names the device, its centre, span and preset. */
  bandKey() {
    const e = this.engine;
    const t = e.target;
    return JSON.stringify([t?.node_id, t?.device_id, e.centerHz, e.device?.sample_rate, e.device?.active_preset?.id ?? ""]);
  }

  /** @returns {Mark[]} the scannable bookmarks in band */
  list() {
    return this.deps.marks().filter((m) => m.scannable);
  }

  /** @returns {boolean} false when there is nothing to scan */
  start() {
    if (this.running) return true;
    if (!this.engine.demod) {
      this.say("The scanner needs a running demodulator.");
      return false;
    }
    if (this.list().length === 0) {
      this.say("No scannable bookmarks in this band.");
      return false;
    }
    this.running = true;
    this.band = this.bandKey();
    this.current = null;
    this.say("Scanner on.");
    this.hop();
    return true;
  }

  /** @param {string} reason why, said to the visitor ("" says nothing) */
  stop(reason) {
    if (!this.running) return;
    this.running = false;
    clearTimeout(this.timer);
    this.timer = 0;
    this.phase = "";
    this.current = null;
    if (reason) this.say(`Scanner off: ${reason}.`);
    this.emit();
  }

  toggle() {
    if (this.running) this.stop("stopped");
    else this.start();
  }

  // hop tunes to the next scannable bookmark, round robin by frequency.
  hop() {
    clearTimeout(this.timer);
    if (!this.running) return;
    const list = this.list();
    if (list.length === 0) {
      this.stop("no scannable bookmarks left in this band");
      return;
    }
    const cur = this.current;
    let i = cur ? list.findIndex((m) => m.key === cur.key) : -1;
    i = i >= 0 ? (i + 1) % list.length : Math.max(0, list.findIndex((m) => !cur || m.frequency > cur.frequency));
    const next = list[i];
    this.current = next;
    this.phase = "settle";
    this.level = null;
    this.expectHz = Math.round(next.frequency);
    this.deps.tune(next);
    if (!this.running) return;
    if (this.engine.tunedHz !== this.expectHz) {
      this.stop("a bookmark is outside the band");
      return;
    }
    this.emit();
    this.timer = setTimeout(() => {
      this.phase = "listen";
      this.emit();
      this.timer = setTimeout(() => {
        if (this.running && this.phase === "listen") this.hop();
      }, LISTEN_MS);
    }, SETTLE_MS);
  }

  /** @param {string} what */
  engineChanged(what) {
    if (!this.running) return;
    const e = this.engine;
    if (what === "meter" && e.meter) {
      // Readings while settling may still be of the previous bookmark.
      if (this.phase === "settle") return;
      this.level = this.level === null ? e.meter.levelDb : this.level + (e.meter.levelDb - this.level) * SMOOTHING;
      this.check();
    } else if (what === "tune") {
      if (e.demod && e.tunedHz !== this.expectHz) this.stop("manual tune");
    } else if (what === "config") {
      if (this.bandKey() !== this.band) this.stop("the band changed");
    } else if (what === "state") {
      if (e.state !== "listening") this.stop("connection lost");
    }
  }

  // check moves to a dwell on a hit, and keeps the dwell while the signal
  // is present.
  check() {
    if (this.level === null || this.phase === "settle") return;
    const threshold = this.deps.squelch() - HIT_MARGIN_DB;
    const above = this.level > threshold;
    if (this.phase === "listen" && above) {
      clearTimeout(this.timer);
      this.phase = "dwell";
      this.lastAbove = performance.now();
      if (this.current) this.say(`Signal on ${this.current.name}.`);
      this.emit();
      const watch = () => {
        if (!this.running || this.phase !== "dwell") return;
        if (performance.now() - this.lastAbove > HANG_MS) this.hop();
        else this.timer = setTimeout(watch, DWELL_CHECK_MS);
      };
      this.timer = setTimeout(watch, DWELL_CHECK_MS);
    } else if (this.phase === "dwell" && above) {
      this.lastAbove = performance.now();
    }
  }

  emit() {
    this.dispatchEvent(new Event("change"));
  }

  /** @param {string} text */
  say(text) {
    this.dispatchEvent(new CustomEvent("message", { detail: { text } }));
  }
}
