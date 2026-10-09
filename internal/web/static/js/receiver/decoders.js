// The Decoders tab of the receiver's side panel (RX-044, DEC-002): the
// status of the listener's decoder (running, unavailable or error, with its
// reason), the variant of each digital mode that has some (SelCall: DTMF,
// EEA, EIA or CCIR; ZVEI: ZVEI1/2/3, DZVEI or PZVEI), one card per digital
// mode with its messages, newest first, and a Clear button, and the digital
// modes this receiver cannot run (DIAG-004: admins see the missing tool).
// Decoded text is untrusted RF text: it goes through textContent only, cut
// to 4 KiB.
//
// The image decoders (SSTV DEC-037, FAX DEC-038) send image.v1 messages:
// a start, then each row, drawn on the card's canvas as it arrives, and an
// end. The canvas has a text equivalent (its label and caption) and can be
// saved as PNG (FIL-006).
//
// Text decoders (PSK, RTTY, SITOR-B, CW) also show the decoder waterfall
// (secondary.js) and, at the top of their card, the line being printed
// (partial decodes).

import { el, formatMHz, SMALL_BUTTON, TOUCH } from "./dom.js";
import { saveFile } from "./recorder.js";
import { SecondaryWaterfall } from "./secondary.js";

/** Messages kept per card. */
const MAX_MESSAGES = 200;
/** Longest decoded text shown (bytes of the node's cap, as characters). */
const MAX_TEXT = 4096;
/** The status region is updated at most this often (UI-009). */
const STATUS_EVERY_MS = 1000;

/** Readable reasons of the node's decoder states. */
const REASONS = /** @type {Record<string, string>} */ ({
  tool_missing: "the decoder program is missing on the node",
  permission: "the decoder program cannot be run",
  tool_misconfigured: "the decoder refused its configuration",
  input_format: "the decoder refused its input",
  crash: "the decoder crashed; it restarts",
  exit_error: "the decoder stopped with an error; it restarts",
  unexpected_exit: "the decoder stopped; it restarts",
  resource: "the decoder ran out of resources; it restarts",
  crash_loop: "the decoder keeps failing; it retries every 10 minutes",
  start_timeout: "the decoder did not start; it restarts",
  workdir: "the node could not prepare the decoder",
  start_failed: "the decoder could not be started",
  "node busy": "the node runs as many decoders as it can; try again later",
  queue_overflow: "the node could not decode every slot in time; a slot was skipped",
  job_timeout: "a slot took too long to decode and was skipped",
});

/** Readable warnings of a running decoder. */
const WARNINGS = /** @type {Record<string, string>} */ ({
  clock_unsynced: "the receiver clock is not synchronised, decodes may be missed",
});

/** Frames of a JS8 thread are within this many hertz (DEC-030). */
const THREAD_SPAN_HZ = 5;
/** A JS8 thread whose next frame does not come within this ends. */
const THREAD_GAP_MS = 5 * 60 * 1000;

/**
 * @typedef {object} Thread
 * @property {number} df
 * @property {string} submode
 * @property {number} last
 * @property {string[]} calls
 * @property {HTMLSpanElement} body
 * @property {HTMLSpanElement} more
 * @property {HTMLSpanElement} callsEl
 */

/** Characters kept per skimmer signal. */
const SKIMMER_TEXT = 80;

/**
 * hue derives a stable hue from a paging address (per-address colour).
 * @param {string} s
 */
function hue(s) {
  let h = 0;
  for (const ch of s) h = (h * 31 + ch.charCodeAt(0)) % 360;
  return h;
}

/** @param {number} ms */
function utcTime(ms) {
  return new Date(ms).toISOString().slice(11, 19);
}

/** @param {number} hz */
function mhz(hz) {
  return hz > 0 ? formatMHz(hz, { unit: false }) : "";
}

/**
 * @typedef {object} ImageView
 * @property {HTMLElement} figure
 * @property {HTMLCanvasElement} canvas
 * @property {HTMLElement} caption
 * @property {HTMLButtonElement} save
 * @property {string} title the image description
 * @property {number} rows the rows drawn
 * @property {number} height
 * @property {number} ts start time (ms)
 * @property {number} freq
 */

/**
 * @typedef {object} Card
 * @property {HTMLElement} el
 * @property {HTMLOListElement} list
 * @property {ImageView | null} image
 * @property {HTMLElement} live the line being printed
 */

/** Largest image side accepted from a node (the hub's limit). */
const MAX_SIDE = 16384;

/**
 * imageName is the file name of an image received at ms on hz:
 * SSTV-<yymmdd-HHMMSS>-<kHz>.png.
 * @param {string} mode @param {number} ms @param {number} hz
 */
function imageName(mode, ms, hz) {
  const d = new Date(ms);
  const p = (/** @type {number} */ n) => String(n).padStart(2, "0");
  const stamp = `${p(d.getUTCFullYear() % 100)}${p(d.getUTCMonth() + 1)}${p(d.getUTCDate())}-${p(d.getUTCHours())}${p(d.getUTCMinutes())}${p(d.getUTCSeconds())}`;
  return `${mode.toUpperCase().replace(/[^A-Z0-9]/g, "")}-${stamp}-${Math.round(hz / 1000)}.png`;
}

/** @param {string} b64 @returns {Uint8Array} */
function fromBase64(b64) {
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

export class DecodersTab {
  /**
   * @param {EventTarget & {device: any, demod: any}} engine the receiver engine
   */
  constructor(engine) {
    this.engine = engine;
    /** @type {Map<string, Card>} */
    this.cards = new Map();
    this.statusText = "";
    this.statusAt = 0;
    this.statusTimer = 0;

    this.el = el("div", { class: "flex flex-col gap-3" });
    this.status = el("p", { class: "text-sm", role: "status", "aria-live": "polite" });
    this.hint = el("p", { class: "text-sm text-fg-muted" }, "Choose a digital mode next to the analog modes to start its decoder.");
    /** @type {Map<string, HTMLSelectElement>} the variant pickers */
    this.variants = new Map();
    this.variantsEl = el("div", { class: "flex flex-wrap gap-3" });
    this.cardsEl = el("div", { class: "flex flex-col gap-3" });
    this.unavailableHead = el("h3", { class: "text-sm font-semibold" }, "Not available on this receiver");
    this.unavailable = el("ul", { class: "list-disc pl-5 text-sm text-fg-muted" });
    this.waterfall = new SecondaryWaterfall(/** @type {any} */ (engine));
    this.el.append(this.status, this.hint, this.variantsEl, this.waterfall.el, this.cardsEl, this.unavailableHead, this.unavailable);

    /** @type {[string, (ev: Event) => void][]} */
    this.listeners = [
      ["decode", (ev) => this.onDecode(/** @type {CustomEvent} */ (ev).detail)],
      ["decoderstatus", (ev) => this.onStatus(/** @type {CustomEvent} */ (ev).detail)],
      ["fft2", (ev) => this.waterfall.onFFT(/** @type {CustomEvent} */ (ev).detail)],
      [
        "change",
        (ev) => {
          const what = /** @type {CustomEvent} */ (ev).detail;
          if (what === "config") this.syncModes();
          if (what === "config" || what === "secondary" || what === "tune") this.waterfall.sync();
        },
      ],
    ];
    for (const [type, fn] of this.listeners) engine.addEventListener(type, fn);
    this.syncModes();
  }

  // detach stops listening to the engine (the island is gone; the engine
  // keeps running).
  detach() {
    for (const [type, fn] of this.listeners) this.engine.removeEventListener(type, fn);
    clearTimeout(this.statusTimer);
  }

  /** @returns {any[]} the digital modes of the device */
  modes() {
    return this.engine.device?.decoders ?? [];
  }

  /** @param {string} mode */
  label(mode) {
    return this.modes().find((m) => m.mode === mode)?.label ?? mode;
  }

  /**
   * variant returns the variant chosen for a mode ("" for the default).
   * @param {string} mode
   */
  variant(mode) {
    return this.variants.get(mode)?.value ?? "";
  }

  // syncVariants shows a variant picker for each available mode that has
  // variants; a change restarts the running decoder of that mode.
  syncVariants() {
    const modes = this.modes().filter((m) => m.available && (m.variants ?? []).length > 0);
    const shown = [...this.variants.keys()].join();
    if (shown === modes.map((m) => m.mode).join()) return;
    const kept = new Map([...this.variants].map(([k, s]) => [k, s.value]));
    this.variants.clear();
    this.variantsEl.replaceChildren(
      ...modes.map((m) => {
        const id = `rx-variant-${m.mode.replace(/[^a-z0-9-]/gi, "")}`;
        const wrap = el("div", { class: "flex items-center gap-2 text-sm" });
        const select = /** @type {HTMLSelectElement} */ (el("select", { id, class: `rounded border border-border px-2 py-1 ${TOUCH}` }));
        for (const v of m.variants) select.append(el("option", { value: v }, v));
        select.value = kept.get(m.mode) ?? m.variants[0];
        select.addEventListener("change", () => {
          const e = /** @type {any} */ (this.engine);
          if (e.demod?.decoder === m.mode) e.setDecoder(m.mode, select.value);
        });
        this.variants.set(m.mode, select);
        wrap.append(el("label", { for: id, class: "font-medium" }, `${m.label} variant`), select);
        return wrap;
      }),
    );
  }

  // syncModes lists the digital modes the receiver cannot run.
  syncModes() {
    this.syncVariants();
    const off = this.modes().filter((m) => !m.available);
    this.unavailable.replaceChildren(...off.map((m) => el("li", {}, `${m.label}: ${m.reason || "not available"}`)));
    this.unavailableHead.hidden = off.length === 0;
    this.unavailable.hidden = off.length === 0;
  }

  /** @param {string} mode @returns {Card} */
  card(mode) {
    const have = this.cards.get(mode);
    if (have) return have;
    const id = `rx-decoder-${mode.replace(/[^a-z0-9-]/gi, "")}`;
    const section = el("section", { class: "flex flex-col gap-2 rounded border border-border p-2", "aria-labelledby": `${id}-title` });
    const head = el("div", { class: "flex items-center gap-2" });
    const title = el("h3", { id: `${id}-title`, class: "min-w-0 flex-1 font-semibold" }, this.label(mode));
    const clear = el("button", { type: "button", class: SMALL_BUTTON }, "Clear");
    head.append(title, clear);
    const list = /** @type {HTMLOListElement} */ (el("ol", { class: "flex max-h-80 flex-col gap-1 overflow-y-auto font-mono text-sm", "aria-label": `${this.label(mode)} messages, newest first`, tabindex: "0" }));
    const live = el("p", { class: "break-all whitespace-pre-wrap font-mono text-sm text-fg-muted" });
    live.hidden = true;
    section.append(head, live, list);
    this.cardsEl.prepend(section);
    /** @type {Card} */
    const c = { el: section, list, image: null, live };
    clear.addEventListener("click", () => {
      list.replaceChildren();
      c.image?.figure.remove();
      c.image = null;
      live.textContent = "";
      live.hidden = true;
    });
    this.cards.set(mode, c);
    this.hint.hidden = true;
    return c;
  }

  /** @param {any} p decode */
  onDecode(p) {
    if (typeof p?.mode !== "string") return;
    const c = this.card(p.mode);
    if (p.schema === "image.v1" && p.payload && typeof p.payload === "object") this.onImage(c, p);
    if (p.schema === "skimmer.v1" && p.payload?.kind === "text") {
      this.skimmerText(c, p);
      return;
    }
    const text = String(p.text ?? "").slice(0, MAX_TEXT);
    // A text decoder's line being printed replaces the previous one.
    c.live.textContent = p.partial ? text : "";
    c.live.hidden = !p.partial;
    if (p.partial || !text) return;
    if (p.schema === "js8.v1" && this.thread(c, p, text)) return;
    const li = el("li", { class: "break-all whitespace-pre-wrap" });
    // Paging: a colour per address, besides the address in the text.
    const address = p.schema === "paging.v1" ? p.payload?.address : undefined;
    if (typeof address === "string") {
      li.classList.add("border-l-4", "pl-1");
      li.style.borderLeftColor = `hsl(${hue(address)} 70% 45%)`;
    }
    let head = `${utcTime(Number(p.ts) || Date.now())} ${mhz(Number(p.freq_hz))} `;
    const db = Number(p.payload?.db);
    if (p.schema === "wsjt.v1" && Number.isFinite(db)) head += `${db} dB `;
    const meta = el("span", { class: "text-fg-muted" }, head);
    const body = el("span");
    body.textContent = text;
    li.append(meta, body);
    this.add(c, li);
  }

  /** add puts a message at the top of a card. @param {Card} c @param {HTMLElement} li */
  add(c, li) {
    c.list.prepend(li);
    while (c.list.childElementCount > MAX_MESSAGES) c.list.lastElementChild?.remove();
  }

  /**
   * imageView returns the canvas of a card for an image of width × height,
   * a new one when the size changes.
   * @param {Card} c @param {string} mode @param {number} width @param {number} height
   * @returns {ImageView | null}
   */
  imageView(c, mode, width, height) {
    if (!(width > 0 && height > 0 && width <= MAX_SIDE && height <= MAX_SIDE)) return null;
    const have = c.image;
    if (have && have.canvas.width === width && have.height === height) return have;
    have?.figure.remove();
    const figure = el("figure", { class: "flex flex-col gap-1" });
    const scroll = el("div", { class: "max-h-96 overflow-auto rounded border border-border", tabindex: "0", "aria-label": `${this.label(mode)} image` });
    const canvas = /** @type {HTMLCanvasElement} */ (el("canvas", { role: "img", class: "block h-auto w-full bg-surface" }));
    canvas.width = width;
    canvas.height = height;
    scroll.append(canvas);
    const caption = el("figcaption", { class: "text-sm text-fg-muted" });
    const save = /** @type {HTMLButtonElement} */ (el("button", { type: "button", class: `self-start ${SMALL_BUTTON}` }, "Save image"));
    figure.append(scroll, caption, save);
    c.list.before(figure);
    /** @type {ImageView} */
    const v = { figure, canvas, caption, save, title: this.label(mode), rows: 0, height, ts: Date.now(), freq: 0 };
    save.addEventListener("click", () => {
      canvas.toBlob((blob) => {
        if (blob) saveFile(blob, imageName(mode, v.ts, v.freq));
      }, "image/png");
    });
    c.image = v;
    return v;
  }

  /**
   * describe sets the text equivalent of an image.
   * @param {ImageView} v @param {string} state
   */
  describe(v, state) {
    const text = `${v.title}: ${state}`;
    v.caption.textContent = text;
    v.canvas.setAttribute("aria-label", text);
  }

  /**
   * onImage draws an image.v1 message: start, row or end.
   * @param {Card} c @param {any} p decode
   */
  onImage(c, p) {
    const m = p.payload;
    const width = Number(m.width);
    const height = Number(m.height);
    switch (m.event) {
      case "start": {
        c.image?.figure.remove();
        c.image = null;
        const v = this.imageView(c, p.mode, width, height);
        if (!v) return;
        v.title = String(p.text ?? "").slice(0, 200) || this.label(p.mode);
        v.ts = Number(p.ts) || Date.now();
        v.freq = Number(p.freq_hz) || 0;
        this.describe(v, `receiving, 0 of ${height} lines`);
        break;
      }
      case "row": {
        const v = this.imageView(c, p.mode, width, height);
        const y = Number(m.row);
        const channels = Number(m.channels) === 1 ? 1 : 3;
        if (!v || !(y >= 0 && y < height) || typeof m.pixels !== "string") return;
        if (v.freq === 0) {
          v.ts = Number(p.ts) || Date.now();
          v.freq = Number(p.freq_hz) || 0;
        }
        const px = fromBase64(m.pixels);
        if (px.length !== width * channels) return;
        const row = new ImageData(width, 1);
        for (let x = 0; x < width; x++) {
          const i = x * channels;
          row.data[x * 4] = px[i];
          row.data[x * 4 + 1] = px[channels === 1 ? i : i + 1];
          row.data[x * 4 + 2] = px[channels === 1 ? i : i + 2];
          row.data[x * 4 + 3] = 255;
        }
        v.canvas.getContext("2d")?.putImageData(row, 0, y);
        v.rows = Math.max(v.rows, y + 1);
        this.describe(v, `receiving, ${v.rows} of ${height} lines`);
        break;
      }
      case "end": {
        const v = c.image;
        if (!v) return;
        const lines = Math.min(Number(m.lines) || 0, v.height);
        // The image keeps the lines received only.
        if (lines > 0 && lines < v.canvas.height) {
          const ctx = v.canvas.getContext("2d");
          const kept = ctx?.getImageData(0, 0, v.canvas.width, lines);
          v.canvas.height = lines;
          if (kept) ctx?.putImageData(kept, 0, 0);
        }
        const done = m.complete ? "complete" : "incomplete";
        const saved = m.saved ? "saved to Files" : "too short to be saved to Files";
        this.describe(v, `${done}, ${lines} of ${v.height} lines, ${saved}`);
        break;
      }
      default:
    }
  }

  /**
   * thread adds a JS8 frame to its thread (DEC-030): frames on one audio
   * frequency (±5 Hz) and submode, from a first frame (thread type bit 1)
   * to a last one (bit 2), are shown on one line with their call signs; an
   * unfinished thread ends with a continuation marker. Returns false when
   * the frame needs a line of its own.
   * @param {Card & {threads?: Thread[]}} c @param {any} p @param {string} text
   */
  thread(c, p, text) {
    const f = p.payload ?? {};
    const df = Number(f.df), type = Number(f.thread_type), submode = String(f.submode ?? "");
    if (!Number.isFinite(df) || !Number.isFinite(type)) return false;
    const now = Number(p.ts) || Date.now();
    c.threads = (c.threads ?? []).filter((t) => now - t.last <= THREAD_GAP_MS && t.body.isConnected);
    let t = (type & 1) === 0 ? c.threads.find((x) => x.submode === submode && Math.abs(x.df - df) <= THREAD_SPAN_HZ) : undefined;
    if (!t) {
      const li = el("li", { class: "break-all whitespace-pre-wrap" });
      const meta = el("span", { class: "text-fg-muted" }, `${utcTime(now)} ${mhz(Number(p.freq_hz))} `);
      const body = /** @type {HTMLSpanElement} */ (el("span"));
      const more = /** @type {HTMLSpanElement} */ (el("span"));
      more.append(el("span", { "aria-hidden": "true" }, " …"), el("span", { class: "sr-only" }, " (continues)"));
      const callsEl = /** @type {HTMLSpanElement} */ (el("span", { class: "block font-sans text-fg-muted" }));
      li.append(meta, body, more, callsEl);
      t = { df, submode, last: now, calls: [], body, more, callsEl };
      c.threads.push(t);
      this.add(c, li);
    }
    t.last = now;
    t.body.textContent = (t.body.textContent + text).slice(0, MAX_TEXT);
    for (const call of [f.callsign, f.to]) {
      if (typeof call === "string" && call && !t.calls.includes(call)) t.calls.push(call);
    }
    t.callsEl.textContent = t.calls.length ? `Calls: ${t.calls.join(", ")}` : "";
    if (type & 2) {
      t.more.remove();
      c.threads = c.threads.filter((x) => x !== t);
    }
    return true;
  }

  /**
   * skimmerText shows the text of a skimmer (DEC-013, DEC-014): one row per
   * signal frequency with its latest characters, the newest row first; a
   * change of the dial frequency clears the rows.
   * @param {Card & {rows?: Map<number, HTMLLIElement>}} c @param {any} p decode
   */
  skimmerText(c, p) {
    if (!c.rows || p.payload.changed) {
      if (c.rows) for (const li of c.rows.values()) li.remove();
      c.rows = new Map();
    }
    const freq = Number(p.freq_hz) || 0;
    let li = c.rows.get(freq);
    if (!li || !li.isConnected) {
      li = /** @type {HTMLLIElement} */ (el("li", { class: "break-all whitespace-pre-wrap" }));
      li.append(el("span", { class: "text-fg-muted" }), el("span"));
      c.rows.set(freq, li);
    }
    const [meta, body] = /** @type {HTMLElement[]} */ ([...li.children]);
    const db = Number(p.payload.db) || 0;
    meta.textContent = `${utcTime(Number(p.ts) || Date.now())} ${mhz(freq)} ${db} dB `;
    body.textContent = ((body.textContent ?? "") + String(p.text ?? "")).slice(-SKIMMER_TEXT);
    c.list.prepend(li);
    while (c.list.childElementCount > MAX_MESSAGES) c.list.lastElementChild?.remove();
  }

  /** @param {any} p diag.state */
  onStatus(p) {
    const variant = typeof p?.variant === "string" ? p.variant : "";
    const select = this.variants.get(String(p?.decoder ?? ""));
    if (select && variant && p.state === "running") select.value = variant;
    const label = this.label(String(p?.decoder ?? "")) + (variant ? ` ${variant}` : "");
    const reason = REASONS[p?.reason] ?? p?.reason ?? "";
    let text = "";
    switch (p?.state) {
      case "running":
        text = `${label}: decoding${p.warning ? ` (warning: ${WARNINGS[p.warning] ?? p.warning})` : ""}.`;
        this.card(String(p.decoder));
        break;
      case "unavailable":
        text = `${label}: not available${reason ? ` (${reason})` : ""}.`;
        break;
      case "error":
        text = `${label}: error${reason ? ` (${reason})` : ""}.`;
        break;
      case "stopped":
        text = `${label}: stopped.`;
        break;
      default:
        return;
    }
    this.say(text);
  }

  /** say updates the status region, throttled. @param {string} text */
  say(text) {
    this.statusText = text;
    const wait = this.statusAt + STATUS_EVERY_MS - Date.now();
    if (wait > 0) {
      if (!this.statusTimer) this.statusTimer = window.setTimeout(() => this.flush(), wait);
      return;
    }
    this.flush();
  }

  flush() {
    this.statusTimer = 0;
    this.statusAt = Date.now();
    this.status.textContent = this.statusText;
  }
}
