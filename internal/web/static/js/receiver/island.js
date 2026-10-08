// <msdr-receiver>: the receiver island (RX-002, RX-015, UI-017). Its initial
// state comes from a templ.JSONScript child (ADR 0003 5A): whether the
// visitor is signed in and the sign-in URL. It lists the devices the visitor
// may listen to (the shell-level grid state, grid.js: GET /api/v1/features
// kept live by the hub events), has the shell-level engine listen to the
// chosen one, and draws a view of it: optional spectrum (RX-016),
// frequency scale with the filter envelope (RX-019) and waterfall (RX-015),
// with click/drag tuning, zoom, levels from the device defaults plus an
// auto action (RX-017/018), in the device's palette (UI-013: the hub
// setting, Default or Turbo; no visitor choice). The view detaches when htmx swaps #main away; the engine, and so
// the audio, keeps running.
//
// The control bar under the waterfall (UI-018) holds the frequency entry,
// tune steps and step picker (RX-009, RX-011, RX-012; shortcuts ← and →),
// the shared preset picker and "Recentre here" (RX-006, RX-010: shown only
// with the preset or retune right of device.config.permissions; refusals
// go to the status region), the analog mode picker
// (RX-007), the pass band (RX-020; its edges are also dragged on the scale),
// the squelch with a one-shot auto (RX-023, RX-024), the noise reduction
// (RX-027) and the S-meter (RX-022). Volume and mute are in the shell audio
// dock (RX-026). A shared preset switch or centre move asks for
// confirmation while others listen, stating how many (UI-012). The bar
// starts with the status chips (UI-022): hub connection, node connection,
// device state, audio (off, suspended, buffer under- or overrun) and a
// linked frequency outside the band; each is a button showing its reason
// and hint inline. "More" holds Share (UI-023: the platform share sheet on
// touch devices, else the clipboard).
//
// Bookmarks (bookmarks.js): the bookmark bar above the scale, the bandplan
// ribbon under it (toolbar toggle, key B), Scan in the control bar (key S),
// Find bookmark in the toolbar (key Y) and the Bookmarks tab.
//
// The side panel (UI-019, ARIA tabs) has the Bookmarks and Info tabs; the
// open state and the active tab are per-session runtime state. Info: the device and its node,
// its active preset, the status metrics (RX-035: audio buffer and rate,
// stream bit rate, network, node CPU and temperature, listeners),
// the station described by the hub (RX-036), whose server-rendered markup
// ([data-rx-station]) the island moves into it, the session's events
// (RX-038, notify.js) and a link to the About page.
//
// Device picker (RX-040, UI-021): a native <select> with one <optgroup> per
// node; each option names the device, its state and listeners, with a lock
// when listening needs an account; the devices of an offline node or that
// cannot run are disabled, the reason in their text. It follows the grid
// state live. Choosing a device makes the engine close the node connection
// and open one to the new device's node.
//
// Deep link (RX-028): /receiver/{nodeId}/{deviceId}?f=<Hz>&m=<mod>&m2=<mod>
// &sql=<dB> selects that device for this visitor and tunes its own
// demodulator; it never switches the preset nor moves the centre. The URL
// follows the tuning (history.replaceState) and is applied again on
// navigation (a new island) and on popstate. m2 (secondary mode) is kept
// as given: no digital mode exists yet (ADR 0026).
//
// Default device (RX-041), without a link: the device the engine already
// listens to, else the one this browser used last (localStorage), else the
// first listed device whose node is online, else the first listed.
//
// Accessibility (UI-009, ADR 0015 decision 12): canvases are role="img" and
// described by a text list (frequency, mode, signal level) refreshed every
// second, not live; connection states are announced in a role="status"
// region at most every 2 s, the last state winning; the signal level is
// announced in its own polite region at most every 5 s, when it moved by
// 3 dB or the squelch opened or closed. Untrusted text (device and node
// names) goes through textContent only.

import { confirmDialog } from "../confirm.js";
import { eventsState } from "../events.js";
import { notices, notifications } from "../notify.js";
import { onThemeChange } from "../tokens.js";
import { ReceiverBookmarks } from "./bookmarks.js";
import { getEngine, MIN_BANDWIDTH_HZ, NR_MAX_DB, NR_MIN_DB, SQUELCH_MAX_DB, SQUELCH_MIN_DB } from "./engine.js";
import { getGrid, unavailable } from "./grid.js";
import { FreqScale } from "./scale.js";
import { Spectrum } from "./spectrum.js";
import { Waterfall2D } from "./waterfall-2d.js";

const LIVE_MIN_INTERVAL_MS = 2000;
// Deep links (RX-028).
const LINK_PATH = /^\/receiver\/([A-Za-z0-9_-]{1,64})\/([A-Za-z0-9_-]{1,64})\/?$/;
const URL_DELAY_MS = 400;
// An audio under- or overrun keeps the audio chip raised this long.
const AUDIO_ALERT_MS = 10_000;
// Events of the session listed in the Info tab (RX-038).
const INFO_EVENTS = 20;
// Device states as the picker and the chip say them (SRC-025).
const STATE_TEXT = /** @type {Record<string, string>} */ ({
  unavailable: "unavailable",
  disabled: "disabled",
  stopped: "idle",
  starting: "starting",
  running: "running",
  retuning: "retuning",
  stopping: "stopping",
  retry_wait: "restarting",
  failed: "failed",
});
const TEXT_INTERVAL_MS = 1000;
const SPECTRUM_INTERVAL_MS = 150;
const METER_INTERVAL_MS = 150;
const METER_LIVE_INTERVAL_MS = 5000;
const METER_LIVE_DELTA_DB = 3;
// S-meter scale (dBFS).
const METER_MIN_DB = -140;
const METER_MAX_DB = 0;
const ZOOMS = [1, 2, 4, 8, 16];
// Fewer bins than this are never shown (deepest zoom of a small FFT).
const MIN_VIEW_BINS = 32;
// Auto levels: the noise floor is this percentile of the visible bins, the
// lower level sits AUTO_FLOOR_MARGIN_DB under it and the upper level
// AUTO_PEAK_MARGIN_DB over the strongest bin, at least auto_min_range apart.
const AUTO_LINES = 16;
const AUTO_PERCENTILE = 0.2;
const AUTO_FLOOR_MARGIN_DB = 5;
const AUTO_PEAK_MARGIN_DB = 5;
const DEFAULT_LEVELS = { min: -100, max: -20 };
// Analog modes of the mode picker, in display order (RX-007; ADR 0026: no
// digital modes in M1a). Only those the device's node reports are shown.
const ANALOG_MODES = ["am", "sam", "nfm", "wfm", "usb", "lsb", "cw"];
// Squelch level offered when the squelch is first switched on (dBFS).
const SQUELCH_DEFAULT_DB = -100;
// Pass band buttons scale the width by this factor.
const BANDPASS_FACTOR = 1.25;
// Pointer distance (CSS px) that grabs a pass band edge on the scale.
const EDGE_GRAB_PX = 6;
const EDGE_GRAB_TOUCH_PX = 16;
// The device this browser used last (RX-041), "<node_id>/<device_id>".
const LAST_DEVICE_KEY = "msdr.receiver.device";
// Tuning steps offered (RX-012, Hz); the device's step is added when it is
// not one of them.
const STEPS = [100, 1000, 5000, 6250, 9000, 10000, 12500, 25000, 100000];

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

/** @param {number} hz */
function formatMHz(hz) {
  return `${(hz / 1e6).toFixed(6)} MHz`;
}

/** @param {number} hz signed, as kHz with one decimal */
function formatKHz(hz) {
  const s = (Math.abs(hz) / 1000).toFixed(1);
  return hz < 0 ? `−${s}` : `+${s}`;
}

/** @param {number} hz a tuning step */
function formatStep(hz) {
  return hz < 1000 ? `${hz} Hz` : `${Number((hz / 1000).toFixed(3))} kHz`;
}

/** @param {number} db */
function formatDb(db) {
  return `${db < 0 ? "−" : ""}${Math.abs(db).toFixed(0)}`;
}

/**
 * parseFrequency reads a typed frequency (RX-011): a number in MHz, or with
 * a unit (Hz, kHz, MHz, GHz; the "Hz" may be left out). Returns Hz, or null.
 * @param {string} text
 * @returns {number | null}
 */
export function parseFrequency(text) {
  const m = /^\s*(\d+(?:[.,]\d*)?|[.,]\d+)\s*([kmg]?)(?:hz)?\s*$/i.exec(text);
  if (!m) return null;
  const n = Number(m[1].replace(",", "."));
  const unit = { "": 1e6, k: 1e3, m: 1e6, g: 1e9 }[m[2].toLowerCase()] ?? 1e6;
  const hz = Math.round(n * unit);
  return Number.isFinite(hz) && hz > 0 ? hz : null;
}

/** @param {string} key @returns {string | null} */
function loadPref(key) {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

/** @param {string} key @param {string} value */
function savePref(key, value) {
  try {
    localStorage.setItem(key, value);
  } catch {
    // Storage blocked: the choice is not remembered.
  }
}

/**
 * @typedef {object} Link a deep link (RX-028)
 * @property {string} node_id
 * @property {string} device_id
 * @property {string} m2 secondary mode, kept as given
 * @property {import("./engine.js").Wanted} want
 */

/**
 * parseLink reads a deep link from a location; null when it is not one.
 * Values out of range are ignored.
 * @param {Location | URL} loc
 * @returns {Link | null}
 */
export function parseLink(loc) {
  const m = LINK_PATH.exec(loc.pathname);
  if (!m) return null;
  const q = new URLSearchParams(loc.search);
  /** @type {import("./engine.js").Wanted} */
  const want = {};
  const f = Number(q.get("f"));
  if (q.has("f") && Number.isFinite(f) && f > 0) want.hz = Math.round(f);
  const mode = (q.get("m") ?? "").toLowerCase();
  if (ANALOG_MODES.includes(mode)) want.mode = mode;
  const sql = Number(q.get("sql"));
  if (q.has("sql") && Number.isFinite(sql) && sql >= SQUELCH_MIN_DB && sql <= SQUELCH_MAX_DB) want.squelchDb = Math.round(sql);
  return { node_id: m[1], device_id: m[2], m2: (q.get("m2") ?? "").slice(0, 32), want };
}

/**
 * linkURL is the deep link of the current tuning (path and query). It never
 * carries credentials or tokens (UI-023).
 * @param {{node_id: string, device_id: string}} t
 * @param {ReturnType<typeof getEngine>} e
 * @param {string} m2
 */
function linkURL(t, e, m2) {
  const q = new URLSearchParams();
  const hz = e.outOfBand ?? e.tunedHz;
  if (hz) q.set("f", String(Math.round(hz)));
  if (e.demod?.mode) q.set("m", e.demod.mode);
  if (m2) q.set("m2", m2);
  if (e.demod && e.demod.squelchDb !== null) q.set("sql", String(e.demod.squelchDb));
  const s = q.toString();
  return `/receiver/${encodeURIComponent(t.node_id)}/${encodeURIComponent(t.device_id)}${s ? `?${s}` : ""}`;
}

/**
 * deviceLabel is the text of a picker option (UI-021): name, state and
 * listeners, or why it cannot be listened to, and a lock when listening
 * needs an account.
 * @param {import("./grid.js").Device} d
 */
function deviceLabel(d) {
  const why = unavailable(d);
  const n = d.listeners ?? 0;
  const parts = why
    ? [d.name, `unavailable: ${why}`]
    : [d.name, STATE_TEXT[d.state] ?? d.state, `${n} listener${n === 1 ? "" : "s"}`];
  return parts.join(" · ") + (d.login_required ? " 🔒" : "");
}

/**
 * @typedef {object} Chip a status chip (UI-022)
 * @property {string} id
 * @property {string} label
 * @property {"ok" | "warn" | "bad" | "off"} tone
 * @property {string} reason
 * @property {string} hint
 * @property {{label: string, run: () => void}} [action]
 */

const BUTTON = "rounded border border-border px-3 py-1 text-sm";
const SMALL_BUTTON = "rounded border border-border px-2 py-1 text-sm";
const GROUP = "flex flex-wrap items-center gap-2";

class MsdrReceiver extends HTMLElement {
  connectedCallback() {
    const script = this.querySelector('script[type="application/json"]');
    /** @type {{signed_in?: boolean, login_url?: string, audio_codec?: string}} */
    this.cfg = JSON.parse(script?.textContent || "{}");
    // Station description rendered by the hub, moved into the Info panel.
    this.station = this.querySelector("[data-rx-station]");
    this.engine = getEngine();
    this.grid = getGrid();
    // The codec applies from the next connection.
    if (this.cfg.audio_codec) this.engine.codec = this.cfg.audio_codec;
    this.loaded = false;
    // The device this visitor chose, "<node_id>/<device_id>"; a deep link
    // to a device the visitor may not list leaves it in linkMissing.
    this.selected = "";
    this.linkMissing = "";
    // The link's secondary mode, kept in the URL as given.
    this.m2 = "";
    this.squelchLevel = SQUELCH_DEFAULT_DB;
    this.openChip = "";
    /** @type {{underruns: number, overruns: number, underrunAt: number, overrunAt: number}} */
    this.audioSeen = { underruns: 0, overruns: 0, underrunAt: -Infinity, overrunAt: -Infinity };
    this.build();

    this.wf = new Waterfall2D(this.wfCanvas);
    this.spectrum = new Spectrum(this.spCanvas);
    this.scale = new FreqScale(this.scaleCanvas);
    this.configKey = "";

    /** @type {import("./engine.js").View} */
    this.view = {
      onFFT: (bins) => {
        this.wf?.pushLine(bins);
        this.spectrum?.push(bins);
      },
    };
    this.onChange = (/** @type {Event} */ e) => this.changed(/** @type {CustomEvent} */ (e).detail);
    this.engine.addEventListener("change", this.onChange);
    this.engine.attachView(this.view);
    this.onGrid = () => {
      if (!this.loaded) return;
      this.renderPicker();
      this.layout();
    };
    this.grid.addEventListener("change", this.onGrid);
    this.onNotices = () => this.renderEvents();
    notices.addEventListener("change", this.onNotices);
    this.onPop = () => {
      if (this.loaded && parseLink(location)) this.applyLocation(false);
    };
    window.addEventListener("popstate", this.onPop);
    this.onHub = () => this.renderChips();
    document.body.addEventListener("msdr:events.state", this.onHub);
    this.stopTheme = onThemeChange(() => {
      this.spectrum?.readColors();
      this.scale?.readColors();
    });
    this.resize = new ResizeObserver(() => this.sizeCanvases());
    this.resize.observe(this.display);

    this.lastSpec = 0;
    this.lastText = 0;
    this.lastMeter = 0;
    this.lastLive = 0;
    this.lastLiveText = "";
    this.lastMeterLive = 0;
    /** @type {{db: number, open: boolean} | null} */
    this.meterSaid = null;
    this.loop = this.loop.bind(this);
    this.rafId = requestAnimationFrame(this.loop);

    this.sizeCanvases();
    this.syncControls();
    this.renderEvents();
    this.start();
  }

  disconnectedCallback() {
    cancelAnimationFrame(this.rafId);
    clearTimeout(this.liveTimer);
    clearTimeout(this.urlTimer);
    this.resize?.disconnect();
    this.stopTheme?.();
    this.bookmarks?.destroy();
    this.engine?.removeEventListener("change", this.onChange);
    this.grid?.removeEventListener("change", this.onGrid);
    notices.removeEventListener("change", this.onNotices);
    window.removeEventListener("popstate", this.onPop);
    document.body.removeEventListener("msdr:events.state", this.onHub);
    if (this.view) this.engine?.detachView(this.view);
    this.wf?.destroy();
    this.wf = undefined;
  }

  build() {
    this.bookmarks = new ReceiverBookmarks({
      engine: this.engine,
      grid: this.grid,
      modes: () => this.chosen()?.modes ?? [],
      squelch: () => this.engine.demod?.squelchDb ?? this.squelchLevel,
      m2: () => this.m2,
      manageURL: this.cfg.bookmarks_url ?? "",
      showTab: () => this.showTab("bookmarks"),
      layout: () => this.layout(),
    });
    const bmk = this.bookmarks;

    // Toolbar: device picker, zoom, levels, spectrum toggle,
    // side panel toggle.
    this.select = /** @type {HTMLSelectElement} */ (el("select", { id: "rx-device", class: "max-w-full min-w-0 rounded border px-2 py-1" }));
    this.select.addEventListener("change", () => {
      if (!this.select.value) return;
      savePref(LAST_DEVICE_KEY, this.select.value);
      this.m2 = "";
      this.choose(this.select.value);
    });
    const picker = el("div", { class: "flex min-w-0 items-center gap-2" });
    picker.append(el("label", { for: "rx-device", class: "font-semibold" }, "Device"), this.select);

    this.zoomOut = el("button", { type: "button", class: BUTTON, "aria-label": "Zoom out" }, "−");
    this.zoomText = el("span", { class: "min-w-10 text-center font-mono tabular-nums" });
    this.zoomIn = el("button", { type: "button", class: BUTTON, "aria-label": "Zoom in" }, "+");
    this.zoomOut.addEventListener("click", () => this.zoom(-1));
    this.zoomIn.addEventListener("click", () => this.zoom(1));
    const zoom = el("div", { class: "flex items-center gap-1", role: "group", "aria-label": "Zoom" });
    zoom.append(this.zoomOut, this.zoomText, this.zoomIn);

    const auto = el("button", { type: "button", class: BUTTON }, "Auto levels");
    auto.addEventListener("click", () => this.autoLevels());
    const reset = el("button", { type: "button", class: BUTTON }, "Default levels");
    reset.addEventListener("click", () => this.setLevels(null));
    const levels = el("div", { class: "flex flex-wrap items-center gap-1", role: "group", "aria-label": "Waterfall levels" });
    levels.append(auto, reset);

    this.spToggle = el("button", { type: "button", class: BUTTON, "aria-pressed": "true" }, "Spectrum");
    this.spToggle.addEventListener("click", () => {
      this.engine.display.spectrum = !this.engine.display.spectrum;
      this.layout();
    });


    this.panelToggle = el("button", { type: "button", class: BUTTON, "aria-controls": "rx-panel" }, "Side panel");
    this.panelToggle.addEventListener("click", () => {
      this.engine.display.panel = !this.engine.display.panel;
      this.layout();
    });

    const shown = el("div", { class: "flex flex-wrap items-center gap-1", role: "group", "aria-label": "Display" });
    shown.append(this.spToggle, bmk.ribbonBtn);

    this.toolbar = el("div", { class: "flex flex-wrap items-center gap-x-4 gap-y-2" });
    this.toolbar.append(picker, zoom, levels, shown, this.panelToggle, bmk.findBtn);

    // Display: spectrum, scale and waterfall; the empty state replaces them.
    const describe = { role: "img", "aria-describedby": "rx-text" };
    this.spCanvas = /** @type {HTMLCanvasElement} */ (
      el("canvas", { ...describe, class: "block h-32 w-full touch-pan-y", "aria-label": "Spectrum" })
    );
    this.scaleCanvas = /** @type {HTMLCanvasElement} */ (
      el("canvas", { ...describe, class: "block h-6 w-full touch-pan-y", "aria-label": "Frequency scale and pass band" })
    );
    this.wfCanvas = /** @type {HTMLCanvasElement} */ (
      el("canvas", { ...describe, class: "block h-[55vh] min-h-48 w-full touch-pan-y bg-black", "aria-label": "Waterfall" })
    );
    this.startBtn = el(
      "button",
      { type: "button", class: "absolute left-1/2 top-40 -translate-x-1/2 rounded bg-accent px-4 py-2 font-semibold text-accent-fg" },
      "Start audio",
    );
    this.startBtn.hidden = true;
    this.startBtn.addEventListener("click", () => this.engine.startAudio());
    this.display = el("div", { class: "relative cursor-crosshair select-none overflow-hidden rounded border border-border" });
    this.display.append(this.spCanvas, bmk.bar, this.scaleCanvas, bmk.ribbon.el, this.wfCanvas, this.startBtn, bmk.tip);
    for (const c of [this.spCanvas, this.scaleCanvas, this.wfCanvas]) this.bindPointer(c);
    this.wfCanvas.addEventListener("wheel", (e) => this.onWheel(e), { passive: false });

    this.empty = el("div", { class: "flex min-h-48 flex-col items-center justify-center gap-3 rounded border border-border bg-surface p-6 text-center" });
    this.empty.hidden = true;

    this.controls = this.buildBar();
    // Status chips (UI-022): live, above the controls, also shown with the
    // empty state.
    this.chipsRow = el("div", { class: "flex flex-wrap items-center gap-2", role: "group", "aria-label": "Status" });
    this.chipReason = el("p", { class: "font-semibold" });
    this.chipHint = el("p", { class: "text-fg-muted" });
    this.chipAction = el("button", { type: "button", class: `${SMALL_BUTTON} mt-2 self-start` });
    this.chipAction.addEventListener("click", () => this.chipRun?.());
    this.chipDetail = el("div", { id: "rx-chip-detail", class: "flex flex-col gap-1 rounded border border-border bg-surface-raised p-2 text-sm" });
    this.chipDetail.append(this.chipReason, this.chipHint, this.chipAction);
    this.chipDetail.hidden = true;
    /** @type {Map<string, HTMLButtonElement>} */
    this.chipButtons = new Map();
    this.bar = el("div", { class: "flex flex-col gap-3" });
    this.bar.append(this.chipsRow, this.chipDetail, this.controls);

    // Text equivalents, refreshed every second, not live.
    this.freqText = el("dd", { class: "font-mono tabular-nums" }, "—");
    this.modeText = el("dd", {}, "—");
    this.levelText = el("dd", { class: "font-mono tabular-nums" }, "—");
    const dl = el("dl", { id: "rx-text", class: "flex flex-wrap gap-x-6 gap-y-1 text-sm" });
    for (const [label, dd] of [
      ["Frequency", this.freqText],
      ["Mode", this.modeText],
      ["Signal level", this.levelText],
    ]) {
      const row = el("div", { class: "flex gap-2" });
      row.append(el("dt", { class: "text-fg-muted" }, /** @type {string} */ (label)), /** @type {HTMLElement} */ (dd));
      dl.append(row);
    }

    this.status = el("p", { class: "text-sm text-fg-muted", role: "status" });
    this.meterLive = el("p", { class: "sr-only", "aria-live": "polite" });

    this.panel = this.buildPanel();

    const left = el("div", { class: "flex min-w-0 flex-col gap-3" });
    left.append(this.display, this.empty, this.bar, dl, this.status, this.meterLive);
    this.body = el("div", { class: "grid gap-3" });
    this.body.append(left, this.panel);

    const root = el("div", { class: "mt-4 flex flex-col gap-3" });
    root.append(this.toolbar, this.body);
    this.replaceChildren(root);
    this.layout();
  }

  // buildBar returns the control bar docked under the waterfall (UI-018).
  buildBar() {
    const e = this.engine;

    // Frequency entry and tune steps (RX-011, RX-009).
    this.freqInput = /** @type {HTMLInputElement} */ (
      el("input", {
        id: "rx-freq",
        type: "text",
        inputmode: "decimal",
        autocomplete: "off",
        spellcheck: "false",
        class: "w-36 rounded border px-2 py-1 font-mono tabular-nums",
        "aria-describedby": "rx-freq-hint",
      })
    );
    this.freqInput.addEventListener("change", () => this.enterFrequency());
    this.freqInput.addEventListener("keydown", (ev) => {
      if (ev.key === "Escape") {
        this.freqInput.value = "";
        this.freqInput.removeAttribute("aria-invalid");
        this.freqHint.textContent = "";
        this.syncControls();
        this.freqInput.blur();
      }
    });
    this.freqHint = el("span", { id: "rx-freq-hint", class: "text-sm text-danger" });
    this.stepDown = el(
      "button",
      { type: "button", class: SMALL_BUTTON, "aria-label": "Tune down one step", "aria-keyshortcuts": "ArrowLeft", "data-shortcut": "arrowleft" },
      "‹",
    );
    this.stepUp = el(
      "button",
      { type: "button", class: SMALL_BUTTON, "aria-label": "Tune up one step", "aria-keyshortcuts": "ArrowRight", "data-shortcut": "arrowright" },
      "›",
    );
    this.stepDown.addEventListener("click", () => e.step(-1));
    this.stepUp.addEventListener("click", () => e.step(1));
    const freq = el("div", { class: GROUP });
    freq.append(el("label", { for: "rx-freq", class: "font-semibold" }, "Frequency (MHz)"), this.stepDown, this.freqInput, this.stepUp, this.freqHint);

    // Tuning step (RX-012): drives ‹ › and the arrow keys.
    this.stepSelect = /** @type {HTMLSelectElement} */ (el("select", { id: "rx-step", class: "rounded border px-2 py-1" }));
    this.stepSelect.addEventListener("change", () => e.setStep(Number(this.stepSelect.value)));
    const step = el("div", { class: GROUP });
    step.append(el("label", { for: "rx-step", class: "font-semibold" }, "Step"), this.stepSelect);

    // Shared preset (RX-006) and centre (RX-010), for callers with the
    // right only; a switch moves the device for every listener.
    this.presetSelect = /** @type {HTMLSelectElement} */ (el("select", { id: "rx-preset", class: "max-w-48 rounded border px-2 py-1" }));
    this.presetSelect.addEventListener("change", async () => {
      const id = this.presetSelect.value;
      if (!id) return;
      if (!(await this.confirmShared("preset")) || !(await e.selectPreset(id))) this.syncControls();
    });
    this.presetGroup = el("div", { class: GROUP });
    this.presetGroup.append(el("label", { for: "rx-preset", class: "font-semibold" }, "Preset"), this.presetSelect);
    this.recentre = el(
      "button",
      { type: "button", class: SMALL_BUTTON, "aria-describedby": "rx-recentre-hint" },
      "Recentre here",
    );
    this.recentre.addEventListener("click", async () => {
      if (await this.confirmShared("centre")) e.retune(e.tunedHz);
    });
    this.recentreGroup = el("div", { class: GROUP });
    this.recentreGroup.append(
      this.recentre,
      el("span", { id: "rx-recentre-hint", class: "sr-only" }, "Moves the device centre to the tuned frequency for every listener."),
    );

    // Mode picker: one button per analog mode (RX-007).
    this.modes = el("div", { class: "flex flex-wrap gap-1", role: "group", "aria-labelledby": "rx-mode-label" });
    const mode = el("div", { class: GROUP });
    mode.append(el("span", { id: "rx-mode-label", class: "font-semibold" }, "Mode"), this.modes);

    // Pass band (RX-020): read-out, narrower and wider; the edges are also
    // dragged on the scale.
    this.bandText = el("span", { class: "font-mono tabular-nums text-sm" }, "—");
    this.narrower = el("button", { type: "button", class: SMALL_BUTTON }, "Narrower");
    this.wider = el("button", { type: "button", class: SMALL_BUTTON }, "Wider");
    this.narrower.addEventListener("click", () => this.scaleBand(1 / BANDPASS_FACTOR));
    this.wider.addEventListener("click", () => this.scaleBand(BANDPASS_FACTOR));
    const band = el("div", { class: GROUP, role: "group", "aria-labelledby": "rx-band-label" });
    band.append(el("span", { id: "rx-band-label", class: "font-semibold" }, "Filter"), this.bandText, this.narrower, this.wider);

    // Squelch: on/off, level and a one-shot auto (RX-023, RX-024).
    this.sqOn = /** @type {HTMLInputElement} */ (el("input", { id: "rx-sq-on", type: "checkbox", class: "accent-accent" }));
    this.sqLevel = /** @type {HTMLInputElement} */ (
      el("input", {
        id: "rx-sq-level",
        type: "range",
        min: String(SQUELCH_MIN_DB + 1),
        max: String(SQUELCH_MAX_DB),
        step: "1",
        class: "w-28 accent-accent",
        "aria-label": "Squelch level (dBFS)",
      })
    );
    this.sqText = el("span", { class: "min-w-16 font-mono tabular-nums text-sm" });
    this.sqAuto = el("button", { type: "button", class: SMALL_BUTTON, "aria-label": "Auto squelch" }, "Auto");
    this.sqOn.addEventListener("change", () => e.setSquelch(this.sqOn.checked ? this.squelchLevel : null));
    this.sqLevel.addEventListener("input", () => {
      this.squelchLevel = Number(this.sqLevel.value);
      e.setSquelch(this.squelchLevel);
    });
    this.sqAuto.addEventListener("click", () => {
      const level = e.autoSquelch();
      if (level !== null) this.squelchLevel = level;
    });
    const sqLabel = el("label", { class: "flex items-center gap-1 font-semibold" });
    sqLabel.append(this.sqOn, document.createTextNode("Squelch"));
    const squelch = el("div", { class: GROUP });
    squelch.append(sqLabel, this.sqLevel, this.sqText, this.sqAuto);

    // Noise reduction: on/off and threshold (RX-027).
    this.nrOn = /** @type {HTMLInputElement} */ (el("input", { id: "rx-nr-on", type: "checkbox", class: "accent-accent" }));
    this.nrLevel = /** @type {HTMLInputElement} */ (
      el("input", {
        id: "rx-nr-level",
        type: "range",
        min: String(NR_MIN_DB),
        max: String(NR_MAX_DB),
        step: "1",
        class: "w-28 accent-accent",
        "aria-label": "Noise reduction threshold (dB)",
      })
    );
    this.nrText = el("span", { class: "min-w-12 font-mono tabular-nums text-sm" });
    const nr = () => e.setNR(this.nrOn.checked, Number(this.nrLevel.value));
    this.nrOn.addEventListener("change", nr);
    this.nrLevel.addEventListener("input", nr);
    const nrLabel = el("label", { class: "flex items-center gap-1 font-semibold" });
    nrLabel.append(this.nrOn, document.createTextNode("Noise reduction"));
    const noise = el("div", { class: GROUP });
    noise.append(nrLabel, this.nrLevel, this.nrText);

    // S-meter and dB read-out (RX-022): the text carries the value, the bar
    // only illustrates it.
    this.meter = /** @type {HTMLMeterElement} */ (
      el("meter", { id: "rx-meter", min: String(METER_MIN_DB), max: String(METER_MAX_DB), class: "h-4 w-32", "aria-labelledby": "rx-meter-label" })
    );
    this.meterText = el("span", { class: "min-w-32 font-mono tabular-nums text-sm" }, "—");
    const smeter = el("div", { class: GROUP });
    smeter.append(el("span", { id: "rx-meter-label", class: "font-semibold" }, "Signal"), this.meter, this.meterText);

    // More (UI-018): Share (UI-023).
    this.moreBtn = el("button", { type: "button", class: SMALL_BUTTON, "aria-expanded": "false", "aria-controls": "rx-more" }, "More");
    this.shareBtn = el("button", { type: "button", class: SMALL_BUTTON }, "Share link");
    this.shareBtn.addEventListener("click", () => this.share());
    this.moreMenu = el("div", { id: "rx-more", class: GROUP });
    this.moreMenu.append(this.shareBtn);
    this.moreMenu.hidden = true;
    this.moreBtn.addEventListener("click", () => {
      this.moreMenu.hidden = !this.moreMenu.hidden;
      this.moreBtn.setAttribute("aria-expanded", String(!this.moreMenu.hidden));
    });
    const more = el("div", { class: GROUP });
    more.append(this.moreBtn, this.moreMenu);

    const bar = el("div", {
      class: "flex flex-wrap items-center gap-x-6 gap-y-3 rounded border border-border bg-surface p-3",
      role: "group",
      "aria-label": "Receiver controls",
    });
    bar.append(freq, step, this.presetGroup, this.recentreGroup, mode, band, squelch, noise, smeter, this.bookmarks.scanGroup, more);
    return bar;
  }

  // buildPanel returns the side panel (UI-019): ARIA tabs (arrows, Home and
  // End move between them) for the Bookmarks tab (bookmarks.js) and the
  // Info tab.
  buildPanel() {
    const section = el("section", { id: "rx-panel", class: "flex min-w-0 flex-col self-start gap-3 rounded border border-border bg-surface p-3", "aria-label": "Side panel" });
    const list = el("div", { role: "tablist", "aria-label": "Side panel", class: "flex gap-1 border-b border-border" });
    const bookmarks = el("div", { class: "flex min-w-0 flex-col gap-3" });
    bookmarks.append(el("h2", { class: "text-lg font-semibold" }, "Bookmarks"), this.bookmarks.panel);
    /** @type {Map<string, {tab: HTMLElement, panel: HTMLElement}>} */
    this.tabs = new Map();
    for (const [id, label, content] of /** @type {[string, string, HTMLElement][]} */ ([
      ["bookmarks", "Bookmarks", bookmarks],
      ["info", "Info", this.buildInfo()],
    ])) {
      const tab = el("button", { type: "button", role: "tab", id: `rx-tab-${id}`, "aria-controls": `rx-tabpanel-${id}`, class: "-mb-px border-b-2 px-3 py-1 font-semibold" }, label);
      tab.addEventListener("click", () => this.showTab(id));
      tab.addEventListener("keydown", (ev) => {
        const ids = [...this.tabs.keys()];
        const i = ids.indexOf(id);
        const j = { ArrowRight: (i + 1) % ids.length, ArrowLeft: (i - 1 + ids.length) % ids.length, Home: 0, End: ids.length - 1 }[ev.key];
        if (j === undefined) return;
        ev.preventDefault();
        this.showTab(ids[j]);
        this.tabs.get(ids[j])?.tab.focus();
      });
      const panel = el("div", { role: "tabpanel", id: `rx-tabpanel-${id}`, "aria-labelledby": `rx-tab-${id}`, tabindex: "0", class: "min-w-0" });
      panel.append(content);
      list.append(tab);
      this.tabs.set(id, { tab, panel });
    }
    section.append(list, ...[...this.tabs.values()].map((t) => t.panel));
    this.showTab(this.engine.display.tab ?? "info", false);
    return section;
  }

  /**
   * showTab selects a side panel tab, opening the panel when asked to.
   * @param {string} id @param {boolean} [open]
   */
  showTab(id, open = true) {
    if (!this.tabs?.has(id)) return;
    this.engine.display.tab = id;
    for (const [k, { tab, panel }] of this.tabs) {
      const on = k === id;
      tab.setAttribute("aria-selected", String(on));
      tab.tabIndex = on ? 0 : -1;
      tab.classList.toggle("border-accent", on);
      tab.classList.toggle("border-transparent", !on);
      panel.hidden = !on;
    }
    if (open && !this.engine.display.panel) {
      this.engine.display.panel = true;
      this.layout();
    }
  }

  // buildInfo returns the Info tab: the device, its node, the status
  // metrics, the station, the session's events and the About link.
  buildInfo() {
    const panel = el("div", { class: "flex min-w-0 flex-col gap-3" });
    const dl = "grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-sm";
    this.info = el("dl", { class: dl });
    this.metrics = el("dl", { class: dl });
    this.events = el("ol", { class: "flex max-h-64 flex-col gap-2 overflow-y-auto text-sm" });
    this.eventsEmpty = el("p", { class: "text-sm text-fg-muted" }, "No events in this session.");
    const about = el("p", { class: "text-sm" });
    about.append(el("a", { href: "/about" }, "About this receiver: version, help and source code"));
    panel.append(
      el("h2", { id: "rx-info-title", class: "text-lg font-semibold" }, "Info"),
      this.info,
      el("h3", { class: "font-semibold" }, "Status"),
      this.metrics,
    );
    if (this.station) {
      this.station.classList.remove("mt-4");
      panel.append(this.station);
    }
    panel.append(el("h3", { class: "font-semibold" }, "Events"), this.eventsEmpty, this.events, about);
    return panel;
  }

  // renderEvents lists the session's latest notifications (RX-038), as text.
  renderEvents() {
    const list = notifications().slice(0, INFO_EVENTS);
    this.eventsEmpty.hidden = list.length > 0;
    this.events.replaceChildren(
      ...list.map((n) => {
        const li = el("li", { class: `notice notice-${n.level}` });
        const at = new Date(n.at);
        li.append(
          el("time", { class: "block text-xs text-fg-muted", datetime: at.toISOString() }, at.toLocaleTimeString()),
          el("p", { class: "break-words" }, `${{ error: "Error", warning: "Warning" }[n.level] ?? "Info"}: ${n.text}`),
        );
        return li;
      }),
    );
  }

  // start loads the grid state, then selects the linked or default device.
  async start() {
    if (this.grid.loaded) this.grid.refresh();
    else await this.grid.load();
    this.loaded = true;
    if (!this.isConnected) return;
    this.renderPicker();
    this.applyLocation(true);
    this.layout();
  }

  /**
   * applyLocation selects the device of a deep link and applies its tuning
   * (RX-028); without a link, on first load, the default device (RX-041).
   * @param {boolean} initial
   */
  applyLocation(initial) {
    const link = parseLink(location);
    const g = this.grid;
    if (link) {
      const key = `${link.node_id}/${link.device_id}`;
      this.m2 = link.m2;
      if (!g.device(link.node_id, link.device_id)) {
        // Unknown, or not listenable by this visitor: the same state.
        this.selected = "";
        this.linkMissing = key;
        this.renderPicker();
        this.layout();
        return;
      }
      this.choose(key, link.want);
      return;
    }
    if (!initial) return;
    const key = (/** @type {import("./grid.js").Device} */ d) => `${d.node_id}/${d.id}`;
    const t = this.engine.target;
    const last = loadPref(LAST_DEVICE_KEY);
    const first =
      (t && g.device(t.node_id, t.device_id)) ??
      g.devices.find((d) => key(d) === last && !unavailable(d)) ??
      g.devices.find((d) => !unavailable(d)) ??
      g.devices[0];
    if (first) this.choose(key(first));
  }

  // renderPicker fills the device picker (UI-021): one group per node,
  // unavailable devices disabled with the reason.
  renderPicker() {
    const g = this.grid;
    const key = JSON.stringify([
      g.devices.map((d) => [d.node_id, d.id, deviceLabel(d)]),
      [...g.nodes.values()].map((n) => [n.id, n.name, n.online]),
      this.linkMissing,
    ]);
    if (key !== this.pickerKey) {
      this.pickerKey = key;
      /** @type {Map<string, import("./grid.js").Device[]>} */
      const byNode = new Map([...g.nodes.keys()].map((id) => [id, []]));
      for (const d of g.devices) {
        if (!byNode.has(d.node_id)) byNode.set(d.node_id, []);
        byNode.get(d.node_id)?.push(d);
      }
      /** @type {HTMLElement[]} */
      const items = [];
      if (this.linkMissing) items.push(el("option", { value: "", disabled: "" }, "Linked device unavailable"));
      for (const [id, devices] of byNode) {
        if (devices.length === 0) continue;
        const n = g.node(id);
        const group = el("optgroup", { label: `${n?.name || id}${n && !n.online ? " (offline)" : ""}` });
        for (const d of devices) {
          const o = el("option", { value: `${d.node_id}/${d.id}` }, deviceLabel(d));
          if (unavailable(d)) o.setAttribute("disabled", "");
          group.append(o);
        }
        items.push(group);
      }
      this.select.replaceChildren(...items);
    }
    this.select.value = this.selected;
  }

  /**
   * choose listens to a device, with the tuning of a deep link if any.
   * @param {string} key "<node_id>/<device_id>" @param {import("./engine.js").Wanted | null} [want]
   */
  choose(key, want = null) {
    const d = this.grid.devices.find((x) => `${x.node_id}/${x.id}` === key);
    if (!d) return;
    this.selected = key;
    this.linkMissing = "";
    this.renderPicker();
    const e = this.engine;
    const t = e.target;
    const same = t && t.node_id === d.node_id && t.device_id === d.id;
    const target = { node_id: d.node_id, device_id: d.id, name: d.name };
    // features' "online" means the device runs, which it only does while
    // someone listens: every listed device is connected to. Another device
    // closes the node connection and opens one for the new device (RX-040).
    if (!same) {
      Object.assign(e.display, { zoom: 1, start: 0, levels: null });
      e.connect(target, want);
    } else if (!e.ws && !e.retryTimer) {
      e.disconnect();
      e.connect(target, want);
    } else if (want) {
      e.applyLink(want);
    }
    this.layout();
  }

  /** @returns {import("./grid.js").Device | undefined} the chosen device */
  chosen() {
    return this.grid.devices.find((x) => `${x.node_id}/${x.id}` === this.selected);
  }

  /**
   * confirmShared asks before a shared change while others listen (UI-012),
   * stating how many will be retuned.
   * @param {"preset" | "centre"} kind
   * @returns {Promise<boolean>}
   */
  async confirmShared(kind) {
    const c = this.chosen();
    const self = this.engine.state === "listening" ? 1 : 0;
    const others = Math.max(0, (c?.listeners ?? 0) - self);
    if (others === 0) return true;
    const who = others === 1 ? "1 other listener" : `${others} other listeners`;
    return confirmDialog(
      kind === "preset"
        ? { title: "Switch the shared preset?", message: `${who} will be retuned to the new preset.`, confirmLabel: "Switch preset" }
        : { title: "Move the shared centre?", message: `${who} will be retuned to the new centre.`, confirmLabel: "Move the centre" },
    );
  }

  // syncURL keeps the deep link of the current tuning in the address bar
  // (RX-028), without a history entry.
  syncURL() {
    clearTimeout(this.urlTimer);
    this.urlTimer = setTimeout(() => {
      const e = this.engine;
      const t = e.target;
      if (!this.isConnected || !t || !e.demod || `${t.node_id}/${t.device_id}` !== this.selected) return;
      if (location.pathname !== "/" && !LINK_PATH.test(location.pathname)) return;
      const url = linkURL(t, e, this.m2);
      if (url !== location.pathname + location.search) history.replaceState(history.state, "", url);
    }, URL_DELAY_MS);
  }

  // share shares the deep link (UI-023): the platform share sheet on touch
  // devices, else the clipboard; the outcome goes to the status region.
  async share() {
    const t = this.engine.target;
    if (!t) return;
    const url = new URL(linkURL(t, this.engine, this.m2), location.origin).href;
    const touch = matchMedia("(pointer: coarse)").matches;
    if (touch && typeof navigator.share === "function") {
      try {
        await navigator.share({ title: document.title, url });
        this.say("Link shared.");
      } catch (err) {
        if (/** @type {any} */ (err)?.name !== "AbortError") this.say("The link could not be shared.");
      }
      return;
    }
    try {
      await navigator.clipboard.writeText(url);
      this.say("Link copied to the clipboard.");
    } catch {
      this.say(`The link could not be copied: ${url}`);
    }
  }

  /** @param {string} text said now in the status region */
  say(text) {
    clearTimeout(this.liveTimer);
    this.status.textContent = text;
    this.lastLive = performance.now();
    this.lastLiveText = text;
  }

  /** @param {string} what */
  changed(what) {
    if (what === "state") {
      this.announce();
      this.layout();
      this.syncURL();
    } else if (what === "config") {
      this.reconfigure();
      this.layout();
    } else if (what === "tune") {
      this.follow();
      this.syncControls();
      this.renderChips();
      this.syncURL();
    } else if (what === "audio") {
      this.layout();
    }
  }

  // layout shows the display or the empty state (RX-005), and syncs the
  // controls with the engine.
  layout() {
    const e = this.engine;
    const msg = this.emptyMessage();
    const none = this.grid.devices.length === 0 && !this.linkMissing;
    this.display.hidden = msg !== null;
    this.controls.hidden = msg !== null;
    this.bar.hidden = none || !this.loaded;
    this.empty.hidden = msg === null;
    if (msg !== null) {
      const key = JSON.stringify(msg);
      if (key !== this.emptyKey) {
        this.emptyKey = key;
        this.empty.replaceChildren(el("p", { class: "font-semibold" }, msg.text));
        if (msg.login) {
          const next = encodeURIComponent(location.pathname + location.search);
          this.empty.append(el("a", { href: `${this.cfg.login_url || "/login"}?next=${next}`, "hx-boost": "false" }, "Sign in"));
        }
      }
    } else {
      this.emptyKey = "";
    }
    this.toolbar.hidden = none;
    this.spCanvas.hidden = !e.display.spectrum;
    this.spToggle.setAttribute("aria-pressed", String(e.display.spectrum));
    const open = e.display.panel && !none;
    this.panel.hidden = !open;
    this.panelToggle.setAttribute("aria-expanded", String(open));
    this.body.classList.toggle("lg:grid-cols-[minmax(0,1fr)_20rem]", open);
    const i = ZOOMS.indexOf(e.display.zoom);
    this.zoomText.textContent = `×${e.display.zoom}`;
    this.zoomOut.toggleAttribute("disabled", i <= 0);
    this.zoomIn.toggleAttribute("disabled", i >= ZOOMS.length - 1 || this.viewBins(ZOOMS[i + 1]) < MIN_VIEW_BINS);
    this.startBtn.hidden = e.audio.running || !e.demod;
    this.syncControls();
    this.renderInfo();
    this.renderMetrics();
    this.renderChips();
    this.sizeCanvases();
  }

  // chips returns the status chips (UI-022): hub, node, device, audio, and
  // a linked frequency outside the band.
  /** @returns {Chip[]} */
  chips() {
    const e = this.engine;
    /** @type {Chip[]} */
    const out = [];
    switch (eventsState()) {
      case "open":
        out.push({ id: "hub", label: "Hub: connected", tone: "ok", reason: "Live updates from the hub are on.", hint: "" });
        break;
      case "connecting":
        out.push({ id: "hub", label: "Hub: connecting", tone: "warn", reason: "Connecting to the hub for live updates.", hint: "Listening does not depend on it." });
        break;
      case "closed":
        out.push({
          id: "hub",
          label: "Hub: reconnecting",
          tone: "warn",
          reason: "The connection to the hub was lost; it is retried with a growing delay.",
          hint: "Listening goes on; device and station states may be out of date meanwhile.",
        });
        break;
      case "stopped":
        out.push({
          id: "hub",
          label: "Hub: offline",
          tone: "bad",
          reason: "The hub could not be reached after several attempts.",
          hint: "Reload the page to resume live updates.",
        });
        break;
      default:
        out.push({ id: "hub", label: "Hub: idle", tone: "off", reason: "No live updates are needed yet.", hint: "" });
    }

    const t = e.target;
    const c = this.chosen();
    if (!t || !c || `${t.node_id}/${t.device_id}` !== this.selected) return out;
    const node = this.grid.node(c.node_id)?.name || c.node_id;

    switch (e.state) {
      case "listening":
      case "connected":
        out.push({ id: "node", label: "Node: connected", tone: "ok", reason: `Connected to the station ${node}.`, hint: "" });
        break;
      case "connecting":
      case "reconnecting":
        if (!c.node_online) {
          out.push({
            id: "node",
            label: "Node: offline",
            tone: "bad",
            reason: `Station ${node} is offline, reconnecting.`,
            hint: "The receiver resumes the same device, frequency, mode and squelch when the station is back.",
          });
        } else {
          out.push({
            id: "node",
            label: e.state === "connecting" ? "Node: connecting" : "Node: reconnecting",
            tone: "warn",
            reason: e.state === "connecting" ? `Connecting to the station ${node}.` : "The connection to the station was lost.",
            hint: e.state === "connecting" ? "" : "It is retried with a growing delay.",
          });
        }
        break;
      case "failed":
        out.push({
          id: "node",
          label: "Node: refused",
          tone: "bad",
          reason: `The station refused the connection${e.detail ? `: ${e.detail}` : ""}.`,
          hint: this.cfg.signed_in ? "Choose another device, or reload the page." : "Signing in may be required.",
        });
        break;
      case "error":
        out.push({ id: "node", label: "Node: error", tone: "bad", reason: e.detail || "The station reported an error.", hint: "Choose the device again to retry." });
        break;
    }

    const state = e.deviceState?.state ?? c.state;
    const text = STATE_TEXT[state] ?? state;
    const reason = e.deviceState?.reason ? ` (${e.deviceState.reason})` : "";
    if (state === "running") {
      out.push({ id: "device", label: "Device: running", tone: "ok", reason: `${c.name} is running.`, hint: "" });
    } else if (state === "failed" || state === "disabled" || state === "unavailable") {
      out.push({
        id: "device",
        label: `Device: ${text}`,
        tone: "bad",
        reason: `${c.name} is ${text}${reason}.`,
        hint: "The station restarts failed devices on its own; another device may be available.",
      });
    } else {
      out.push({ id: "device", label: `Device: ${text}`, tone: "warn", reason: `${c.name} is ${text}${reason}.`, hint: "" });
    }

    const a = e.audio;
    const now = performance.now();
    if (!a.ctx) {
      out.push({ id: "audio", label: "Audio: off", tone: "warn", reason: "Audio has not been started.", hint: "Press Start audio to listen." });
    } else if (!a.running) {
      out.push({
        id: "audio",
        label: "Audio: suspended",
        tone: "warn",
        reason: "Audio is paused.",
        hint: "Press Start audio to resume; browsers suspend audio until you interact with the page.",
      });
    } else if (now - this.audioSeen.underrunAt < AUDIO_ALERT_MS) {
      out.push({
        id: "audio",
        label: "Audio: buffer underrun",
        tone: "warn",
        reason: `The audio buffer ran empty (${this.audioSeen.underruns} times in this session).`,
        hint: `Audio arrived late; the buffer grew to ${Math.round(a.stats.targetMs)} ms to absorb the delays.`,
      });
    } else if (now - this.audioSeen.overrunAt < AUDIO_ALERT_MS) {
      out.push({
        id: "audio",
        label: "Audio: buffer overrun",
        tone: "warn",
        reason: "Audio arrived faster than it plays; the excess was dropped.",
        hint: "This follows a burst after a network stall; it settles on its own.",
      });
    } else if (!e.audioStream) {
      out.push({ id: "audio", label: "Audio: waiting", tone: "off", reason: "No audio stream yet.", hint: "" });
    } else {
      out.push({ id: "audio", label: "Audio: playing", tone: "ok", reason: "Audio is playing.", hint: "" });
    }

    if (e.outOfBand !== null) {
      const [lo, hi] = e.band();
      const hz = e.outOfBand;
      out.push({
        id: "link",
        label: "Link: outside the band",
        tone: "warn",
        reason: `${formatMHz(hz)} is outside the band received now (${(lo / 1e6).toFixed(3)} to ${(hi / 1e6).toFixed(3)} MHz).`,
        hint: e.device?.permissions?.retune
          ? "You may move the centre for every listener, or tune within the band."
          : "Tune within the band; an operator may move the centre.",
        action: e.device?.permissions?.retune
          ? {
              label: `Move the centre to ${formatMHz(hz)}`,
              run: async () => {
                if (await this.confirmShared("centre")) e.retune(hz);
              },
            }
          : undefined,
      });
    }
    return out;
  }

  // renderChips shows the chips, updated in place so that a focused chip
  // keeps the focus, and the reason and hint of the expanded one.
  renderChips() {
    if (!this.chipsRow) return;
    const list = this.chips();
    const ids = list.map((c) => c.id);
    if (ids.join() !== [...this.chipButtons.keys()].join()) {
      for (const [id, b] of this.chipButtons) {
        if (!ids.includes(id)) {
          b.remove();
          this.chipButtons.delete(id);
        }
      }
      for (const c of list) {
        let b = this.chipButtons.get(c.id);
        if (!b) {
          b = /** @type {HTMLButtonElement} */ (el("button", { type: "button", "aria-controls": "rx-chip-detail", "aria-expanded": "false" }));
          const id = c.id;
          b.addEventListener("click", () => {
            this.openChip = this.openChip === id ? "" : id;
            this.renderChips();
          });
          this.chipButtons.set(c.id, b);
        }
        this.chipsRow.append(b);
      }
      // Map order follows the row.
      this.chipButtons = new Map(ids.map((id) => [id, /** @type {HTMLButtonElement} */ (this.chipButtons.get(id))]));
    }
    if (!ids.includes(this.openChip)) this.openChip = "";
    for (const c of list) {
      const b = /** @type {HTMLButtonElement} */ (this.chipButtons.get(c.id));
      if (b.textContent !== c.label) b.textContent = c.label;
      const cls = `chip chip-${c.tone}`;
      if (b.className !== cls) b.className = cls;
      b.setAttribute("aria-expanded", String(this.openChip === c.id));
    }
    const open = list.find((c) => c.id === this.openChip);
    this.chipDetail.hidden = !open;
    if (open) {
      if (this.chipReason.textContent !== open.reason) this.chipReason.textContent = open.reason;
      if (this.chipHint.textContent !== open.hint) this.chipHint.textContent = open.hint;
      this.chipHint.hidden = !open.hint;
      this.chipAction.hidden = !open.action;
      if (open.action && this.chipAction.textContent !== open.action.label) this.chipAction.textContent = open.action.label;
      this.chipRun = open.action?.run;
    }
  }

  // watchAudio notes new under- and overruns of the jitter buffer (RX-035:
  // they raise the audio chip).
  watchAudio() {
    const s = this.engine.audio.stats;
    const seen = this.audioSeen;
    const now = performance.now();
    if (s.underruns > seen.underruns) seen.underrunAt = now;
    if ((s.overruns ?? 0) > seen.overruns) seen.overrunAt = now;
    seen.underruns = s.underruns;
    seen.overruns = s.overruns ?? 0;
  }

  // renderMetrics fills the status metrics of the Info tab (RX-035), as
  // text (refreshed every second, not live).
  renderMetrics() {
    const e = this.engine;
    const st = e.audio.stats;
    const c = this.chosen();
    const n = c ? this.grid.node(c.node_id) : undefined;
    const listening = !!e.target && !!c && `${e.target.node_id}/${e.target.device_id}` === this.selected;
    /** @type {[string, string | Node][]} */
    const rows = [
      ["Audio buffer", listening && e.audio.running && e.audioStream ? `${Math.round(st.bufferedMs)} ms (target ${Math.round(st.targetMs)} ms)` : "—"],
      ["Audio rate", listening && e.audioStream ? `${Number((e.audioStream.rate / 1000).toFixed(1))} kHz` : "—"],
      ["Stream", listening && e.ws ? `${Math.round(e.net.bitRate / 1000)} kbit/s` : "—"],
      ["Network", listening && e.ws ? this.network() : "—"],
      ["Node CPU", typeof n?.cpu === "number" ? `${Math.round(n.cpu * 100)} %` : "—"],
      ["Temperature", typeof n?.temp_c === "number" ? `${n.temp_c.toFixed(1)} °C` : "—"],
    ];
    rows.push(["Listeners on this device", c ? String(c.listeners ?? 0) : "—"]);
    const key = JSON.stringify(rows.map(([k, v]) => [k, typeof v === "string" ? v : v.textContent]));
    if (key === this.metricsKey) return;
    this.metricsKey = key;
    this.metrics.replaceChildren(
      ...rows.flatMap(([k, v]) => {
        const dd = el("dd", { class: "min-w-0 break-words" });
        dd.append(v);
        return [el("dt", { class: "text-fg-muted" }, k), dd];
      }),
    );
  }

  // network describes the link to the node (RX-035): bars and text.
  network() {
    const { rttMs, jitterMs } = this.engine.net;
    const span = el("span");
    if (rttMs === null) {
      span.textContent = "measuring…";
      return span;
    }
    let bars = rttMs < 100 ? 4 : rttMs < 200 ? 3 : rttMs < 400 ? 2 : 1;
    if ((jitterMs ?? 0) > 50 && bars > 1) bars--;
    const quality = ["", "poor", "fair", "good", "excellent"][bars];
    const icon = el("span", { class: "net-bars", "aria-hidden": "true" });
    for (let i = 1; i <= 4; i++) icon.append(el("span", i <= bars ? { "data-on": "" } : {}));
    span.append(icon, `${quality} (round trip ${Math.round(rttMs)} ms, jitter ${Math.round(jitterMs ?? 0)} ms)`);
    return span;
  }

  // syncControls shows the demodulator in force in the control bar. A field
  // being edited keeps what the visitor typed.
  syncControls() {
    const e = this.engine;
    const d = e.demod;
    for (const c of [this.stepDown, this.stepUp, this.freqInput, this.narrower, this.wider, this.sqOn, this.sqAuto, this.nrOn]) {
      c.toggleAttribute("disabled", !d);
    }
    if (document.activeElement !== this.freqInput) {
      this.freqInput.value = d ? (e.tunedHz / 1e6).toFixed(6) : "";
    }

    const available = new Set(this.chosen()?.modes ?? []);
    const modes = ANALOG_MODES.filter((m) => available.has(m) || m === d?.mode);
    const shown = [...this.modes.children].map((b) => /** @type {HTMLElement} */ (b).dataset.mode).join();
    if (shown !== modes.join()) {
      this.modes.replaceChildren(
        ...modes.map((m) => {
          const b = el("button", { type: "button", class: SMALL_BUTTON, "data-mode": m, "aria-pressed": "false" }, m.toUpperCase());
          b.addEventListener("click", () => this.engine.setMode(m));
          return b;
        }),
      );
    }
    for (const b of this.modes.children) {
      const m = /** @type {HTMLElement} */ (b).dataset.mode;
      b.setAttribute("aria-pressed", String(m === d?.mode));
      b.classList.toggle("bg-accent", m === d?.mode);
      b.classList.toggle("text-accent-fg", m === d?.mode);
      b.toggleAttribute("disabled", !d);
    }

    this.bandText.textContent = d ? `${formatKHz(d.lowHz)} … ${formatKHz(d.highHz)} kHz` : "—";

    this.syncShared();

    const sq = d?.squelchDb ?? null;
    if (sq !== null) this.squelchLevel = sq;
    this.sqOn.checked = sq !== null;
    this.sqLevel.value = String(this.squelchLevel);
    this.sqLevel.toggleAttribute("disabled", !d || sq === null);
    this.sqText.textContent = `${formatDb(this.squelchLevel)} dBFS`;
    this.sqLevel.setAttribute("aria-valuetext", this.sqText.textContent);

    const nr = d?.nr ?? { enabled: false, threshold: 0 };
    this.nrOn.checked = nr.enabled;
    this.nrLevel.value = String(nr.threshold);
    this.nrLevel.toggleAttribute("disabled", !d || !nr.enabled);
    this.nrText.textContent = `${formatDb(nr.threshold)} dB`;
    this.nrLevel.setAttribute("aria-valuetext", this.nrText.textContent);
  }

  // syncShared shows the step picker, and the preset picker and "Recentre
  // here" to callers with the right (device.config.permissions).
  syncShared() {
    const e = this.engine;
    const d = e.demod;
    const dev = e.device;

    const current = e.tuningStep();
    const steps = [...new Set([...STEPS, dev?.tuning_step_hz || 0, current])].filter((v) => v > 1).sort((a, b) => a - b);
    if (steps.join() !== [...this.stepSelect.options].map((o) => o.value).join()) {
      this.stepSelect.replaceChildren(...steps.map((v) => el("option", { value: String(v) }, formatStep(v))));
    }
    this.stepSelect.value = String(current);
    this.stepSelect.toggleAttribute("disabled", !d);

    const perms = dev?.permissions ?? {};
    const presets = Array.isArray(dev?.presets_available) ? dev.presets_available : [];
    const active = dev?.active_preset?.id ?? "";
    this.presetGroup.hidden = !perms.preset || presets.length === 0;
    const key = JSON.stringify([active === "", presets]);
    if (key !== this.presetKey) {
      this.presetKey = key;
      const opts = presets.map((/** @type {any} */ p) => el("option", { value: p.id }, p.name));
      if (active === "") opts.unshift(el("option", { value: "", disabled: "" }, "No preset"));
      this.presetSelect.replaceChildren(...opts);
    }
    this.presetSelect.value = active;
    this.presetSelect.toggleAttribute("disabled", !d);

    this.recentreGroup.hidden = !perms.retune;
    this.recentre.toggleAttribute("disabled", !d);
  }

  // renderInfo fills the Info tab: the device and its node (features list),
  // and what the node says of the device (device.config).
  renderInfo() {
    const c = this.chosen();
    const e = this.engine;
    /** @type {[string, string][]} */
    const rows = [];
    if (c) {
      rows.push(
        ["Device", c.name],
        ["Node", this.grid.node(c.node_id)?.name || c.node_id],
        ["Node status", c.node_online ? "Online" : "Offline"],
        ["Device state", STATE_TEXT[c.state] ?? c.state],
        ["Access", c.login_required ? "Signed-in listeners" : "Everyone"],
      );
      const modes = ANALOG_MODES.filter((m) => (c.modes ?? []).includes(m));
      if (modes.length) rows.push(["Modes", modes.map((m) => m.toUpperCase()).join(", ")]);
    }
    if (e.device && e.target && c && e.target.device_id === c.id && e.target.node_id === c.node_id) {
      rows.push(["Preset", e.device.active_preset?.name ?? "None"]);
      if (e.centerHz) rows.push(["Centre", formatMHz(e.centerHz)]);
      // The bandplan ribbon's text equivalent (RX-029).
      if (e.demod) rows.push(["Band", this.bookmarks?.bandAt(e.tunedHz) || "None in the band plan"]);
      if (e.device.sample_rate) rows.push(["Bandwidth", `${(e.device.sample_rate / 1e6).toFixed(3)} MHz`]);
      if (e.device.tuning_step_hz) rows.push(["Tuning step", `${(e.device.tuning_step_hz / 1e3).toFixed(1)} kHz`]);
    }
    const key = JSON.stringify(rows);
    if (key === this.infoKey) return;
    this.infoKey = key;
    this.info.replaceChildren(
      ...rows.flatMap(([k, v]) => [el("dt", { class: "text-fg-muted" }, k), el("dd", { class: "min-w-0 break-words" }, v)]),
    );
  }

  /** @returns {{text: string, login?: boolean} | null} */
  emptyMessage() {
    const e = this.engine;
    const g = this.grid;
    if (!this.loaded) return { text: "Loading the receiver…" };
    if (g.error) return { text: "The receiver could not load its devices. Reload the page to try again." };
    if (this.linkMissing) {
      if (!this.cfg.signed_in) return { text: "The linked device is not available. Signing in may give access to it.", login: true };
      return { text: "The linked device is not available. Choose another device." };
    }
    if (g.devices.length === 0) {
      if (!this.cfg.signed_in) return { text: "Sign in to listen to this receiver.", login: true };
      return { text: "No receiver device is available." };
    }
    const d = this.chosen();
    if (d && d.node_online === false && e.state !== "listening" && e.state !== "connected") {
      // GRID-021: the engine keeps retrying and resumes the same tuning.
      const t = e.target;
      const retrying = t && t.node_id === d.node_id && t.device_id === d.id && (e.state === "reconnecting" || e.state === "connecting");
      return { text: retrying ? "Station offline, reconnecting…" : "This device's station is offline." };
    }
    const st = e.deviceState?.state;
    if (st === "failed" || st === "disabled" || st === "unavailable") {
      return { text: `This device is unavailable (${st}).` };
    }
    if (e.state === "failed") {
      if (!this.cfg.signed_in) return { text: "Sign in to listen to this device.", login: true };
      return { text: "The node refused the connection." };
    }
    return null;
  }

  // enterFrequency tunes to the typed frequency (RX-011), inside the
  // capture band; a refused entry says why.
  enterFrequency() {
    const e = this.engine;
    const text = this.freqInput.value;
    const hz = parseFrequency(text);
    let error = "";
    if (hz === null) {
      error = "Type a frequency in MHz, such as 145.5, or with a unit (kHz, MHz).";
    } else if (!e.tune(hz, false)) {
      const [lo, hi] = e.band();
      error = `Outside the band received now (${(lo / 1e6).toFixed(3)} to ${(hi / 1e6).toFixed(3)} MHz).`;
    }
    this.freqHint.textContent = error;
    if (error) {
      this.freqInput.setAttribute("aria-invalid", "true");
    } else {
      this.freqInput.removeAttribute("aria-invalid");
      this.freqInput.value = (e.tunedHz / 1e6).toFixed(6);
    }
  }

  /** @param {number} factor scale the pass band width around its middle */
  scaleBand(factor) {
    const d = this.engine.demod;
    if (!d) return;
    const mid = (d.lowHz + d.highHz) / 2;
    const half = Math.max(MIN_BANDWIDTH_HZ, (d.highHz - d.lowHz) * factor) / 2;
    this.engine.setBandpass(mid - half, mid + half);
  }

  // sizeCanvases matches the canvas buffers to their CSS size (device
  // pixels, at most 2×) and rebuilds the renderers when it changed.
  sizeCanvases() {
    if (!this.display || this.display.hidden) return;
    const dpr = Math.min(window.devicePixelRatio || 1, 2);
    let changed = false;
    for (const c of [this.spCanvas, this.scaleCanvas, this.wfCanvas]) {
      if (!c || c.hidden) continue;
      const r = c.getBoundingClientRect();
      const w = Math.max(1, Math.round(r.width * dpr));
      const h = Math.max(1, Math.round(r.height * dpr));
      if (c.width !== w || c.height !== h) {
        c.width = w;
        c.height = h;
        changed = true;
      }
    }
    if (changed) {
      this.configKey = "";
      this.reconfigure();
    }
  }

  /** @param {number} zoom @returns {number} visible bins at zoom */
  viewBins(zoom) {
    const size = this.engine.fft?.size ?? 0;
    return Math.max(1, Math.floor(size / zoom));
  }

  /** @returns {{start: number, count: number, startHz: number, spanHz: number} | null} */
  window() {
    const e = this.engine;
    const fft = e.fft;
    if (!fft) return null;
    const count = this.viewBins(e.display.zoom);
    const start = Math.min(fft.size - count, Math.max(0, Math.round(e.display.start)));
    const hzPerBin = fft.spanHz / fft.size;
    return { start, count, startHz: fft.startHz + start * hzPerBin, spanHz: count * hzPerBin };
  }

  /** @returns {{min: number, max: number}} */
  levels() {
    const d = this.engine.device?.waterfall?.levels;
    return this.engine.display.levels ?? (d ? { min: d.min, max: d.max } : DEFAULT_LEVELS);
  }

  // reconfigure rebuilds the renderers when what they draw changed.
  reconfigure() {
    const e = this.engine;
    const win = this.window();
    if (!win || !e.fft || !this.wf || !this.spectrum) return;
    const lv = this.levels();
    const o = {
      start: win.start,
      count: win.count,
      palette: e.device?.waterfall?.scheme ?? "default",
      minDb: lv.min,
      maxDb: lv.max,
      dbMin: e.fft.dbMin,
      dbStep: e.fft.dbStep,
    };
    const key = JSON.stringify(o);
    if (key === this.configKey) return;
    this.configKey = key;
    this.wf.configure(o, e.history);
    this.spectrum.configure(o);
  }

  /** @param {number} dir +1 zoom in, −1 zoom out */
  zoom(dir) {
    const e = this.engine;
    const i = ZOOMS.indexOf(e.display.zoom) + dir;
    if (i < 0 || i >= ZOOMS.length || this.viewBins(ZOOMS[i]) < MIN_VIEW_BINS) return;
    e.display.zoom = ZOOMS[i];
    this.centerOn(e.tunedHz || e.centerHz);
    this.layout();
  }

  /** @param {number} hz centre the view on hz */
  centerOn(hz) {
    const e = this.engine;
    const fft = e.fft;
    if (!fft) return;
    const count = this.viewBins(e.display.zoom);
    const bin = ((hz - fft.startHz) / fft.spanHz) * fft.size;
    e.display.start = Math.min(fft.size - count, Math.max(0, Math.round(bin - count / 2)));
    this.reconfigure();
  }

  // follow recentres the zoomed view when the tuned frequency left it.
  follow() {
    const win = this.window();
    const hz = this.engine.tunedHz;
    if (win && hz && (hz < win.startHz || hz > win.startHz + win.spanHz)) this.centerOn(hz);
  }

  /** @param {{min: number, max: number} | null} lv null: device defaults */
  setLevels(lv) {
    this.engine.display.levels = lv;
    this.reconfigure();
  }

  // autoLevels sets the levels once from the recent visible lines (RX-018).
  autoLevels() {
    const e = this.engine;
    const win = this.window();
    if (!win || !e.fft || e.history.length === 0) return;
    const values = [];
    for (const line of e.history.slice(-AUTO_LINES)) {
      for (let i = win.start; i < Math.min(line.length, win.start + win.count); i++) values.push(line[i]);
    }
    if (values.length === 0) return;
    values.sort((a, b) => a - b);
    const db = (/** @type {number} */ q) => e.fft.dbMin + q * e.fft.dbStep;
    const floor = db(values[Math.floor(values.length * AUTO_PERCENTILE)]);
    const peak = db(values[values.length - 1]);
    const minRange = e.device?.waterfall?.auto_min_range || 0;
    const min = Math.round(floor - AUTO_FLOOR_MARGIN_DB);
    const max = Math.round(Math.max(peak + AUTO_PEAK_MARGIN_DB, min + minRange));
    this.setLevels({ min, max });
  }

  // bindPointer tunes on click and drag (RX-008); on the scale, a pointer
  // on a pass band edge drags that edge instead (RX-020).
  /** @param {HTMLCanvasElement} canvas */
  bindPointer(canvas) {
    const isScale = canvas === this.scaleCanvas;
    /** @param {PointerEvent} ev */
    const edgeAt = (ev) =>
      isScale && this.scale ? this.scale.edgeAt(ev.clientX, ev.pointerType === "touch" ? EDGE_GRAB_TOUCH_PX : EDGE_GRAB_PX) : null;
    canvas.addEventListener("pointerdown", (ev) => {
      if (ev.button !== 0) return;
      canvas.setPointerCapture(ev.pointerId);
      this.dragging = true;
      this.dragEdge = edgeAt(ev);
      if (this.dragEdge) this.dragBand(canvas, ev.clientX);
      else this.tuneAt(canvas, ev.clientX);
    });
    canvas.addEventListener("pointermove", (ev) => {
      if (this.dragging) {
        if (this.dragEdge) this.dragBand(canvas, ev.clientX);
        else this.tuneAt(canvas, ev.clientX);
      } else if (isScale) {
        canvas.classList.toggle("cursor-ew-resize", edgeAt(ev) !== null);
      }
    });
    const stop = () => {
      this.dragging = false;
      this.dragEdge = null;
    };
    canvas.addEventListener("pointerup", stop);
    canvas.addEventListener("pointercancel", stop);
  }

  /** @param {HTMLCanvasElement} canvas @param {number} clientX @returns {number | null} Hz under clientX */
  hzAt(canvas, clientX) {
    const win = this.window();
    if (!win) return null;
    const r = canvas.getBoundingClientRect();
    const f = Math.min(1, Math.max(0, (clientX - r.left) / r.width));
    return win.startHz + f * win.spanHz;
  }

  /** @param {HTMLCanvasElement} canvas @param {number} clientX */
  tuneAt(canvas, clientX) {
    const hz = this.hzAt(canvas, clientX);
    if (hz !== null) this.engine.tune(hz);
  }

  /** @param {HTMLCanvasElement} canvas @param {number} clientX */
  dragBand(canvas, clientX) {
    const e = this.engine;
    const d = e.demod;
    const hz = this.hzAt(canvas, clientX);
    if (!d || hz === null) return;
    const rel = hz - e.tunedHz;
    if (this.dragEdge === "low") e.setBandpass(Math.min(rel, d.highHz - MIN_BANDWIDTH_HZ), d.highHz);
    else e.setBandpass(d.lowHz, Math.max(rel, d.lowHz + MIN_BANDWIDTH_HZ));
  }

  /** @param {WheelEvent} ev */
  onWheel(ev) {
    if (ev.deltaY === 0) return;
    ev.preventDefault();
    this.zoom(ev.deltaY < 0 ? 1 : -1);
  }

  /** @param {number} now */
  loop(now) {
    this.rafId = requestAnimationFrame(this.loop);
    this.wf?.draw();
    if (now - this.lastSpec >= SPECTRUM_INTERVAL_MS) {
      this.lastSpec = now;
      this.drawOverlays();
    }
    if (now - this.lastMeter >= METER_INTERVAL_MS) {
      this.lastMeter = now;
      this.updateMeter(now);
    }
    if (now - this.lastText >= TEXT_INTERVAL_MS) {
      this.lastText = now;
      this.updateText();
      this.watchAudio();
      this.renderChips();
      this.renderMetrics();
      this.renderInfo();
    }
  }

  // drawOverlays redraws the spectrum and the scale with the tuned
  // frequency and its pass band.
  drawOverlays() {
    const e = this.engine;
    const win = this.window();
    if (!win || this.display.hidden) return;
    const d = e.demod;
    let tune = null;
    let envelope = null;
    if (d) {
      const hz = e.tunedHz;
      const x = (/** @type {number} */ f) => (f - win.startHz) / win.spanHz;
      tune = { center: x(hz), low: x(hz + d.lowHz), high: x(hz + d.highHz) };
      envelope = { lowHz: hz + d.lowHz, highHz: hz + d.highHz };
    }
    if (e.display.spectrum) this.spectrum?.draw(tune);
    this.scale?.draw(win.startHz, win.spanHz, envelope);
    this.bookmarks?.draw(win);
  }

  // updateMeter shows the last demod.meter (RX-022) and announces the
  // signal level, throttled.
  /** @param {number} now */
  updateMeter(now) {
    const m = this.engine.demod ? this.engine.meter : null;
    if (!m) {
      this.meter.value = METER_MIN_DB;
      this.meterText.textContent = "—";
      return;
    }
    this.meter.value = Math.min(METER_MAX_DB, Math.max(METER_MIN_DB, m.levelDb));
    const text = `${m.levelDb.toFixed(1)} dBFS${m.open ? "" : ", squelched"}`;
    this.meterText.textContent = text;
    const said = this.meterSaid;
    if (now - this.lastMeterLive < METER_LIVE_INTERVAL_MS) return;
    if (said && said.open === m.open && Math.abs(said.db - m.levelDb) < METER_LIVE_DELTA_DB) return;
    this.lastMeterLive = now;
    this.meterSaid = { db: m.levelDb, open: m.open };
    this.meterLive.textContent = `Signal ${m.levelDb.toFixed(0)} dBFS${m.open ? "" : ", squelched"}`;
  }

  updateText() {
    const e = this.engine;
    this.freqText.textContent = e.tunedHz ? formatMHz(e.tunedHz) : "—";
    this.modeText.textContent = e.demod?.mode ? e.demod.mode.toUpperCase() : "—";
    if (e.meter) {
      this.levelText.textContent = `${e.meter.levelDb.toFixed(1)} dBFS${e.meter.open ? "" : " (squelched)"}`;
    } else {
      this.levelText.textContent = "—";
    }
  }

  // announce puts the connection state in the status region, at most once
  // every 2 s; the last state wins.
  announce() {
    const text = this.stateText();
    clearTimeout(this.liveTimer);
    if (text === this.lastLiveText) return;
    const wait = Math.max(0, LIVE_MIN_INTERVAL_MS - (performance.now() - this.lastLive));
    this.liveTimer = setTimeout(() => {
      this.status.textContent = text;
      this.lastLive = performance.now();
      this.lastLiveText = text;
    }, wait);
  }

  stateText() {
    const e = this.engine;
    const name = e.target?.name ?? "the device";
    switch (e.state) {
      case "connecting":
        return `Connecting to ${name}…`;
      case "connected":
        return `Connected, opening ${name}…`;
      case "listening":
        return `Receiving ${name}.${e.detail ? ` The node refused the change: ${e.detail}.` : ""}`;
      case "reconnecting":
        return this.chosen()?.node_online === false ? "Station offline, reconnecting…" : "Connection lost, reconnecting…";
      case "failed":
        return `Connection refused${e.detail ? `: ${e.detail}` : ""}.`;
      case "error":
        return `Error: ${e.detail || "unknown"}.`;
      default:
        return "";
    }
  }
}

if (!customElements.get("msdr-receiver")) {
  customElements.define("msdr-receiver", MsdrReceiver);
}
