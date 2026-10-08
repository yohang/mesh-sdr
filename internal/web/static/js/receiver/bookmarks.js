// Bookmarks of the receiver (BMK-001, BMK-004, BMK-006, BMK-007, RX-029,
// RX-030, RX-039): what the island gets from one ReceiverBookmarks.
//
// Data: for the device listened to, GET /api/v1/bookmarks?device_id=&from=
// &to= over the current band (centre ± sample rate / 2; the hub applies
// the listen policy and the scopes, BMK-008) and GET /api/v1/bandplan over
// the same band (bands and dial frequencies of bandplan.region). Both are
// fetched again when the band changes (device, centre, span or preset) and
// when a bookmark.changed hub event (topic devices) concerns the band.
//
// Bookmark bar: above the frequency scale, one marker per bookmark or dial
// frequency in view, at its frequency, in two lanes; a marker that does not
// fit with its name keeps only its shape. Order and cues (BMK-007): by
// frequency, then dial, hub, pack; each origin has a color and a shape
// (● hub, ■ pack, ◆ dial) and says its origin in its accessible name. The
// bar is one tab stop (role toolbar, roving tabindex, ← → Home End);
// hover or focus shows the description as text. A click tunes this
// visitor's own demodulator: mode (or the underlying analog mode of a
// digital bookmark), exact frequency; it stops the scanner and shows the
// name for 3 s in the control bar, announced politely (RX-039). Digital
// mode offsets (fax, RTTY, NAVTEX…) are not applied: there are no digital
// modes before M2 (ADR 0026).
//
// Bookmarks tab: the in-band marks as buttons doing the same, the legend,
// the search (bookmark-search.js, key Y) and, for operators and admins, a
// link to Bookmarks › Manage pre-filled from the current tuning (BMK-003).
//
// Control bar: Scan (scanner.js, key S) with its state as text and
// aria-pressed. Toolbar: Bandplan (bandplan.js, key B). Keys go through
// the data-shortcut mechanism (shortcuts.js) on these visible controls.

import { BookmarkSearch } from "./bookmark-search.js";
import { BandplanRibbon, rememberedVisible, rememberVisible } from "./bandplan.js";
import { compareMarks, dialOf, formatMHz, markOf, originText, tuneMode } from "./marks.js";
import { Scanner } from "./scanner.js";

const BOOKMARKS_URL = "/api/v1/bookmarks";
const BANDPLAN_URL = "/api/v1/bandplan";
// Hub events come in bursts: one refetch.
const REFETCH_DELAY_MS = 300;
// The name of a bookmark tuned to stays this long (RX-039).
const LABEL_MS = 3000;
// Marker layout (CSS px): the shape's centre sits on the frequency.
const LANES = 2;
const LANE_PX = 20;
const SHAPE_HALF_PX = 8;
const COMPACT_PX = 16;
const CHAR_PX = 6.5;
const LABEL_MAX_PX = 192;
const GAP_PX = 4;

const BUTTON = "rounded border border-border px-3 py-1 text-sm";
const SMALL_BUTTON = "rounded border border-border px-2 py-1 text-sm";

/**
 * @typedef {import("./marks.js").Mark} Mark
 * @typedef {import("./bandplan.js").Band} Band
 */

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

/** @param {Mark} m the accessible name of a mark */
function markLabel(m) {
  const mode = m.modulation.toUpperCase();
  return `${m.name}, ${formatMHz(m.frequency)}${mode && mode !== m.name ? `, ${mode}` : ""}, ${originText(m.origin)}`;
}

/** @param {Mark} m */
function shape(m) {
  return el("span", { class: `bmk-shape bmk-shape-${m.origin}` });
}

export class ReceiverBookmarks {
  /**
   * @param {{
   *   engine: ReturnType<typeof import("./engine.js").getEngine>,
   *   grid: ReturnType<typeof import("./grid.js").getGrid>,
   *   modes: () => string[],
   *   squelch: () => number,
   *   m2: () => string,
   *   manageURL: string,
   *   showTab: () => void,
   *   layout: () => void,
   * }} deps
   *   modes: the modes of the device chosen; squelch: the squelch level
   *   (dBFS) the scanner compares with; m2: the secondary mode of the link;
   *   manageURL: Bookmarks › Manage ("" when the visitor may not add);
   *   showTab: opens the side panel on the Bookmarks tab; layout: the
   *   island lays itself out again (the ribbon was toggled).
   */
  constructor(deps) {
    this.d = deps;
    const e = deps.engine;
    this.engine = e;
    /** @type {Mark[]} */
    this.marks = [];
    /** @type {Band[]} */
    this.bands = [];
    this.bandKey = "";
    this.drawKey = "";
    /** @type {AbortController | null} */
    this.abort = null;
    this.refetchTimer = 0;
    this.labelTimer = 0;
    // The marker that has the bar's tab stop.
    this.focusKey = "";
    /** @type {Map<string, HTMLButtonElement>} */
    this.buttons = new Map();
    e.display.bandplan ??= rememberedVisible();

    this.buildBar();
    this.ribbon = new BandplanRibbon();
    this.search = new BookmarkSearch(deps.grid);
    this.scanner = new Scanner(e, { marks: () => this.marks, tune: (m) => this.tune(m), squelch: deps.squelch });
    this.buildControls();
    this.buildPanel();

    this.onEngine = (/** @type {Event} */ ev) => {
      const what = /** @type {CustomEvent} */ (ev).detail;
      if (what === "config" || what === "state") this.refresh(false);
      if (what === "tune") this.tuned();
    };
    e.addEventListener("change", this.onEngine);
    this.onScanner = () => this.syncScan();
    this.scanner.addEventListener("change", this.onScanner);
    this.onScanMessage = (/** @type {Event} */ ev) => this.announce(/** @type {CustomEvent} */ (ev).detail.text);
    this.scanner.addEventListener("message", this.onScanMessage);
    this.onChanged = (/** @type {Event} */ ev) => this.bookmarkChanged(/** @type {CustomEvent} */ (ev).detail?.payload);
    document.body.addEventListener("msdr:bookmark.changed", this.onChanged);
    this.onResync = () => this.refresh(true);
    document.body.addEventListener("msdr:resync", this.onResync);

    this.syncRibbon();
    this.syncScan();
    this.refresh(false);
  }

  destroy() {
    this.abort?.abort();
    clearTimeout(this.refetchTimer);
    clearTimeout(this.labelTimer);
    this.engine.removeEventListener("change", this.onEngine);
    this.scanner.removeEventListener("change", this.onScanner);
    this.scanner.removeEventListener("message", this.onScanMessage);
    this.scanner.destroy();
    this.search.destroy();
    document.body.removeEventListener("msdr:bookmark.changed", this.onChanged);
    document.body.removeEventListener("msdr:resync", this.onResync);
  }

  buildBar() {
    this.bar = el("div", { class: "bmk-bar", role: "toolbar", "aria-label": "Bookmarks in view", "aria-orientation": "horizontal" });
    this.tip = el("div", { id: "rx-bmk-tip", class: "bmk-tip", role: "tooltip" });
    this.tip.hidden = true;
    this.bar.addEventListener("keydown", (ev) => this.barKey(ev));
  }

  buildControls() {
    // Scan (BMK-006): a toggle with its state in text.
    this.scanBtn = el("button", { type: "button", class: SMALL_BUTTON, "aria-pressed": "false", "aria-keyshortcuts": "S", "data-shortcut": "s" }, "Scan");
    this.scanBtn.addEventListener("click", () => this.scanner.toggle());
    this.scanState = el("span", { class: "text-sm" });
    // RX-039: the name of the bookmark tuned to, for 3 s.
    this.tuneLabel = el("span", { class: "text-sm font-semibold", "aria-live": "polite" });
    this.scanGroup = el("div", { class: "flex flex-wrap items-center gap-2", role: "group", "aria-label": "Bookmark scanner" });
    this.scanGroup.append(this.scanBtn, this.scanState, this.tuneLabel);

    // Bandplan ribbon toggle (RX-029).
    this.ribbonBtn = el("button", { type: "button", class: BUTTON, "aria-pressed": "false", "aria-keyshortcuts": "B", "data-shortcut": "b" }, "Bandplan");
    this.ribbonBtn.addEventListener("click", () => {
      const on = !this.engine.display.bandplan;
      this.engine.display.bandplan = on;
      rememberVisible(on);
      this.syncRibbon();
      this.d.layout();
    });

    // Find a bookmark (BMK-004): opens the Bookmarks tab on the search.
    this.findBtn = el("button", { type: "button", class: BUTTON, "aria-keyshortcuts": "Y", "data-shortcut": "y" }, "Find bookmark");
    this.findBtn.addEventListener("click", () => {
      this.d.showTab();
      this.search.focus();
    });
  }

  buildPanel() {
    this.list = el("ul", { class: "flex max-h-72 flex-col gap-1 overflow-y-auto text-sm" });
    this.listEmpty = el("p", { class: "text-sm text-fg-muted" }, "No bookmarks in this band.");
    const legend = el("p", { class: "flex flex-wrap gap-x-3 text-sm text-fg-muted" });
    for (const o of /** @type {const} */ (["hub", "pack", "dial"])) {
      const item = el("span");
      item.append(el("span", { class: `bmk-shape bmk-shape-${o}` }), ` ${originText(o)}`);
      legend.append(item);
    }
    this.addLink = /** @type {HTMLAnchorElement} */ (el("a", { class: "text-sm" }, "Add a bookmark at this frequency"));
    this.addLink.hidden = !this.d.manageURL;
    this.panel = el("div", { class: "flex min-w-0 flex-col gap-3" });
    this.panel.append(
      this.search.el,
      el("h3", { class: "font-semibold" }, "In this band"),
      legend,
      this.listEmpty,
      this.list,
      this.addLink,
    );
  }

  /** @returns {boolean} whether the ribbon is shown */
  get ribbonVisible() {
    return !!this.engine.display.bandplan;
  }

  syncRibbon() {
    const on = this.ribbonVisible;
    this.ribbon.el.hidden = !on;
    this.ribbonBtn.setAttribute("aria-pressed", String(on));
    this.ribbonBtn.classList.toggle("bg-accent", on);
    this.ribbonBtn.classList.toggle("text-accent-fg", on);
    this.drawKey = "";
  }

  syncScan() {
    const s = this.scanner;
    this.scanBtn.setAttribute("aria-pressed", String(s.running));
    this.scanBtn.classList.toggle("bg-accent", s.running);
    this.scanBtn.classList.toggle("text-accent-fg", s.running);
    const what = s.current ? `${s.current.name} (${formatMHz(s.current.frequency)})` : "";
    this.scanState.textContent = !s.running ? "Off" : s.phase === "dwell" ? `On: signal on ${what}` : `On: ${what}`;
  }

  /** @param {string} text said politely in the control bar */
  announce(text) {
    clearTimeout(this.labelTimer);
    this.tuneLabel.textContent = text;
    this.labelTimer = setTimeout(() => {
      this.tuneLabel.textContent = "";
    }, LABEL_MS);
  }

  /**
   * refresh fetches the bookmarks and the band plan of the band listened
   * to, when it changed (or always with force).
   * @param {boolean} force
   */
  refresh(force) {
    const e = this.engine;
    const t = e.target;
    const ready = !!t && !!e.device && (e.device.sample_rate ?? 0) > 0;
    const [lo, hi] = ready ? e.band() : [0, 0];
    const key = ready ? JSON.stringify([t?.device_id, lo, hi, e.device?.active_preset?.id ?? ""]) : "";
    if (key === this.bandKey && !force) return;
    this.bandKey = key;
    this.abort?.abort();
    this.abort = null;
    if (!key || !t) {
      this.set([], []);
      return;
    }
    const abort = new AbortController();
    this.abort = abort;
    const get = async (/** @type {string} */ url) => {
      const res = await fetch(url, { credentials: "same-origin", headers: { Accept: "application/json" }, signal: abort.signal });
      if (!res.ok) throw new Error(`${url} answered ${res.status}`);
      return res.json();
    };
    const range = `from=${Math.max(0, lo)}&to=${hi}`;
    Promise.allSettled([get(`${BOOKMARKS_URL}?device_id=${encodeURIComponent(t.device_id)}&${range}`), get(`${BANDPLAN_URL}?${range}`)]).then(
      ([b, p]) => {
        if (abort.signal.aborted) return;
        const bookmarks = b.status === "fulfilled" && Array.isArray(b.value?.bookmarks) ? b.value.bookmarks : [];
        const plan = p.status === "fulfilled" ? p.value : null;
        const marks = [...bookmarks.map(markOf), ...(Array.isArray(plan?.dials) ? plan.dials.map(dialOf) : [])];
        this.set(marks, Array.isArray(plan?.bands) ? plan.bands : []);
      },
    );
  }

  /** @param {Mark[]} marks @param {Band[]} bands */
  set(marks, bands) {
    this.marks = marks.filter((m) => Number.isFinite(m.frequency)).sort(compareMarks);
    this.bands = bands;
    this.ribbon.setBands(bands);
    this.drawKey = "";
    this.renderList();
  }

  /** @param {any} p bookmark.changed payload: {op, bookmark} */
  bookmarkChanged(p) {
    const b = p?.bookmark;
    if (!b || !this.bandKey) return;
    const [lo, hi] = this.engine.band();
    const near = typeof b.frequency === "number" && b.frequency >= lo && b.frequency <= hi;
    if (!near && !this.marks.some((m) => m.key === `b:${b.id}`)) return;
    clearTimeout(this.refetchTimer);
    this.refetchTimer = setTimeout(() => this.refresh(true), REFETCH_DELAY_MS);
  }

  /**
   * tune tunes this visitor's demodulator to a mark: its mode (or analog
   * underlying mode) when the device offers it, and its exact frequency.
   * @param {Mark} m
   */
  tune(m) {
    const e = this.engine;
    const mode = tuneMode(m.modulation, m.underlying, this.d.modes());
    if (mode && e.demod && mode !== e.demod.mode) e.setMode(mode);
    e.tune(m.frequency, false);
  }

  /** @param {Mark} m a mark chosen by the visitor (bar or Bookmarks tab) */
  pick(m) {
    this.scanner.stop("bookmark chosen");
    this.tune(m);
    this.announce(m.name);
  }

  // tuned marks the mark of the tuned frequency, and updates the add link.
  tuned() {
    const e = this.engine;
    const hz = e.demod ? e.tunedHz : 0;
    for (const [key, b] of this.buttons) {
      const m = this.marks.find((x) => x.key === key);
      if (m) b.setAttribute("aria-current", String(m.frequency === hz));
    }
    if (this.d.manageURL && hz) {
      const q = new URLSearchParams({ f: String(Math.round(hz)) });
      if (e.demod?.mode) q.set("m", e.demod.mode);
      const m2 = this.d.m2();
      if (m2) q.set("m2", m2);
      this.addLink.href = `${this.d.manageURL}?${q}`;
      this.addLink.hidden = false;
    } else {
      this.addLink.hidden = true;
    }
  }

  // renderList fills the Bookmarks tab with the in-band marks.
  renderList() {
    this.listEmpty.hidden = this.marks.length > 0;
    this.list.replaceChildren(
      ...this.marks.map((m) => {
        const li = el("li", { class: "flex flex-col" });
        const b = el("button", { type: "button", class: "flex items-center gap-1 rounded px-1 text-left hover:bg-surface-raised", "aria-label": markLabel(m) });
        b.append(shape(m), el("span", { class: "min-w-0 break-words" }, `${m.name} · ${formatMHz(m.frequency)} · ${m.modulation.toUpperCase()}`));
        b.addEventListener("click", () => this.pick(m));
        li.append(b);
        if (m.description) li.append(el("p", { class: "ml-5 break-words text-fg-muted" }, m.description));
        return li;
      }),
    );
    this.tuned();
  }

  /**
   * draw places the bar's markers and the ribbon for the visible span.
   * @param {{startHz: number, spanHz: number} | null} win
   */
  draw(win) {
    if (!win) return;
    if (this.ribbonVisible) this.ribbon.draw(win.startHz, win.spanHz);
    const width = this.bar.clientWidth;
    const key = `${win.startHz}|${win.spanHz}|${width}`;
    if (key === this.drawKey || width === 0) return;
    this.drawKey = key;
    const x = (/** @type {number} */ hz) => ((hz - win.startHz) / win.spanHz) * width;
    const lanes = new Array(LANES).fill(-Infinity);
    /** @type {Map<string, HTMLButtonElement>} */
    const next = new Map();
    for (const m of this.marks) {
      const px = x(m.frequency);
      if (px < 0 || px > width) continue;
      const left = px - SHAPE_HALF_PX;
      const full = Math.min(LABEL_MAX_PX, COMPACT_PX + m.name.length * CHAR_PX + GAP_PX);
      let lane = lanes.findIndex((end) => left >= end + GAP_PX && left + full <= width);
      let compact = false;
      if (lane < 0) {
        compact = true;
        lane = lanes.findIndex((end) => left >= end);
        if (lane < 0) lane = lanes.indexOf(Math.min(...lanes));
      }
      lanes[lane] = left + (compact ? COMPACT_PX : full);
      const b = this.buttons.get(m.key) ?? this.marker(m);
      b.style.left = `${Math.max(0, left)}px`;
      b.style.top = `${2 + lane * LANE_PX}px`;
      /** @type {HTMLElement} */ (b.querySelector(".bmk-label")).hidden = compact;
      next.set(m.key, b);
    }
    this.buttons = next;
    if (!next.has(this.focusKey)) this.focusKey = next.keys().next().value ?? "";
    for (const [k, b] of next) b.tabIndex = k === this.focusKey ? 0 : -1;
    const had = this.bar.contains(document.activeElement);
    this.bar.replaceChildren(...next.values());
    if (had) next.get(this.focusKey)?.focus();
    this.tuned();
  }

  /** @param {Mark} m @returns {HTMLButtonElement} a marker of the bar */
  marker(m) {
    const b = /** @type {HTMLButtonElement} */ (el("button", { type: "button", class: "bmk-marker", "aria-label": markLabel(m) }));
    b.append(shape(m), el("span", { class: "bmk-label" }, m.name));
    if (m.description) b.setAttribute("aria-describedby", "rx-bmk-tip");
    b.addEventListener("click", () => this.pick(m));
    const show = () => this.showTip(m, b);
    const hide = () => this.hideTip(b);
    b.addEventListener("mouseenter", show);
    b.addEventListener("focus", () => {
      this.focusKey = m.key;
      for (const [k, x] of this.buttons) x.tabIndex = k === m.key ? 0 : -1;
      show();
    });
    b.addEventListener("mouseleave", () => {
      if (document.activeElement !== b) hide();
    });
    b.addEventListener("blur", hide);
    return b;
  }

  /** @param {Mark} m @param {HTMLElement} b */
  showTip(m, b) {
    if (!m.description) {
      this.tip.hidden = true;
      return;
    }
    this.tip.textContent = m.description;
    this.tipOwner = b;
    this.tip.hidden = false;
    const host = /** @type {HTMLElement | null} */ (this.tip.offsetParent);
    if (!host) return;
    const r = b.getBoundingClientRect();
    const h = host.getBoundingClientRect();
    const left = Math.min(Math.max(0, r.left - h.left), Math.max(0, h.width - this.tip.offsetWidth));
    this.tip.style.left = `${left}px`;
    this.tip.style.top = `${r.bottom - h.top + 2}px`;
  }

  /** @param {HTMLElement} [b] hides the tip of b (any when omitted) */
  hideTip(b) {
    if (b && this.tipOwner !== b) return;
    this.tip.hidden = true;
    this.tipOwner = null;
  }

  // barKey moves the focus among the markers (roving tabindex) and hides
  // the description with Escape (WCAG 1.4.13).
  /** @param {KeyboardEvent} ev */
  barKey(ev) {
    if (ev.key === "Escape") {
      if (!this.tip.hidden) {
        ev.preventDefault();
        this.hideTip();
      }
      return;
    }
    const keys = [...this.buttons.keys()];
    const i = keys.indexOf(this.focusKey);
    let j = -1;
    if (ev.key === "ArrowRight") j = Math.min(keys.length - 1, i + 1);
    else if (ev.key === "ArrowLeft") j = Math.max(0, i - 1);
    else if (ev.key === "Home") j = 0;
    else if (ev.key === "End") j = keys.length - 1;
    else return;
    // Not the tune step shortcuts of the arrows (shortcuts.js).
    ev.preventDefault();
    if (j >= 0) this.buttons.get(keys[j])?.focus();
  }

  /**
   * bandAt says the band plan's bands at hz (the ribbon's text equivalent).
   * @param {number} hz
   */
  bandAt(hz) {
    return this.ribbon.bandAt(hz).join(", ");
  }
}
