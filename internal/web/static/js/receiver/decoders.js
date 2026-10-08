// The Decoders tab of the receiver's side panel (RX-044, DEC-002): the
// status of the listener's decoder (running, unavailable or error, with its
// reason), one card per digital mode with its messages, newest first, and
// a Clear button, and the digital modes this receiver cannot run (DIAG-004:
// admins see the missing tool). Decoded text is untrusted RF text: it goes
// through textContent only, cut to 4 KiB.

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
});

/**
 * el creates an element with attributes and optional text content.
 * @param {string} tag @param {Record<string, string>} [attrs] @param {string} [text]
 */
function el(tag, attrs = {}, text) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, v);
  if (text !== undefined) e.textContent = text;
  return e;
}

/** @param {number} ms */
function utcTime(ms) {
  return new Date(ms).toISOString().slice(11, 19);
}

/** @param {number} hz */
function mhz(hz) {
  return hz > 0 ? (hz / 1e6).toFixed(6) : "";
}

/**
 * @typedef {object} Card
 * @property {HTMLElement} el
 * @property {HTMLOListElement} list
 */

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
    this.cardsEl = el("div", { class: "flex flex-col gap-3" });
    this.unavailableHead = el("h3", { class: "text-sm font-semibold" }, "Not available on this receiver");
    this.unavailable = el("ul", { class: "list-disc pl-5 text-sm text-fg-muted" });
    this.el.append(this.status, this.hint, this.cardsEl, this.unavailableHead, this.unavailable);

    /** @type {[string, (ev: Event) => void][]} */
    this.listeners = [
      ["decode", (ev) => this.onDecode(/** @type {CustomEvent} */ (ev).detail)],
      ["decoderstatus", (ev) => this.onStatus(/** @type {CustomEvent} */ (ev).detail)],
      [
        "change",
        (ev) => {
          if (/** @type {CustomEvent} */ (ev).detail === "config") this.syncModes();
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

  // syncModes lists the digital modes the receiver cannot run.
  syncModes() {
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
    const clear = el("button", { type: "button", class: "rounded border border-border px-2 py-1 text-sm" }, "Clear");
    head.append(title, clear);
    const list = /** @type {HTMLOListElement} */ (el("ol", { class: "flex max-h-80 flex-col gap-1 overflow-y-auto font-mono text-sm", "aria-label": `${this.label(mode)} messages, newest first`, tabindex: "0" }));
    clear.addEventListener("click", () => list.replaceChildren());
    section.append(head, list);
    this.cardsEl.prepend(section);
    const c = { el: section, list };
    this.cards.set(mode, c);
    this.hint.hidden = true;
    return c;
  }

  /** @param {any} p decode */
  onDecode(p) {
    if (typeof p?.mode !== "string") return;
    const c = this.card(p.mode);
    const text = String(p.text ?? "").slice(0, MAX_TEXT);
    const li = el("li", { class: "break-all whitespace-pre-wrap" });
    const meta = el("span", { class: "text-fg-muted" }, `${utcTime(Number(p.ts) || Date.now())} ${mhz(Number(p.freq_hz))} `);
    const body = el("span");
    body.textContent = text;
    li.append(meta, body);
    c.list.prepend(li);
    while (c.list.childElementCount > MAX_MESSAGES) c.list.lastElementChild?.remove();
  }

  /** @param {any} p diag.state */
  onStatus(p) {
    const label = this.label(String(p?.decoder ?? ""));
    const reason = REASONS[p?.reason] ?? p?.reason ?? "";
    let text = "";
    switch (p?.state) {
      case "running":
        text = `${label}: decoding.`;
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
