// <msdr-receiver>: the receiver island (RX-002, RX-015, UI-017). Its initial
// state comes from a templ.JSONScript child (ADR 0003 5A): whether the
// visitor is signed in and the sign-in URL. It lists the devices the visitor
// may listen to (GET /api/v1/features), has the shell-level engine listen to
// the chosen one, and draws a view of it: optional spectrum (RX-016),
// frequency scale with the filter envelope (RX-019) and waterfall (RX-015),
// with click/drag tuning, zoom, levels from the device defaults plus an
// auto action (RX-017/018), in the device's palette (UI-013: the hub
// setting, Default or Turbo; no visitor choice). The view detaches when htmx swaps #main away; the engine, and so
// the audio, keeps running.
//
// The control bar under the waterfall (UI-018) holds the frequency entry and
// tune steps (RX-009, RX-011; shortcuts ← and →), the analog mode picker
// (RX-007), the pass band (RX-020; its edges are also dragged on the scale),
// the squelch with a one-shot auto (RX-023, RX-024), the noise reduction
// (RX-027) and the S-meter (RX-022). Volume and mute are in the shell audio
// dock (RX-026). The side panel (UI-019) has the Info tab only: the device
// and its node, and the station described by the hub (RX-036), whose
// server-rendered markup ([data-rx-station]) the island moves into it.
//
// Default device (RX-041): the device the engine already listens to, else
// the one this browser used last (localStorage), else the first listed
// device whose node is online, else the first listed.
//
// Accessibility (UI-009, ADR 0015 decision 12): canvases are role="img" and
// described by a text list (frequency, mode, signal level) refreshed every
// second, not live; connection states are announced in a role="status"
// region at most every 2 s, the last state winning; the signal level is
// announced in its own polite region at most every 5 s, when it moved by
// 3 dB or the squelch opened or closed. Untrusted text (device and node
// names) goes through textContent only.

import { onThemeChange } from "../tokens.js";
import { getEngine, MIN_BANDWIDTH_HZ, NR_MAX_DB, NR_MIN_DB, SQUELCH_MAX_DB, SQUELCH_MIN_DB } from "./engine.js";
import { FreqScale } from "./scale.js";
import { Spectrum } from "./spectrum.js";
import { Waterfall2D } from "./waterfall-2d.js";

const FEATURES_URL = "/api/v1/features";
const LIVE_MIN_INTERVAL_MS = 2000;
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

const BUTTON = "rounded border border-border px-3 py-1 text-sm";
const SMALL_BUTTON = "rounded border border-border px-2 py-1 text-sm";
const GROUP = "flex flex-wrap items-center gap-2";

class MsdrReceiver extends HTMLElement {
  connectedCallback() {
    const script = this.querySelector('script[type="application/json"]');
    /** @type {{signed_in?: boolean, login_url?: string}} */
    this.cfg = JSON.parse(script?.textContent || "{}");
    // Station description rendered by the hub, moved into the Info panel.
    this.station = this.querySelector("[data-rx-station]");
    this.engine = getEngine();
    /** @type {any[]} */
    this.devices = [];
    this.devicesError = false;
    this.loaded = false;
    this.squelchLevel = SQUELCH_DEFAULT_DB;
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
    this.loadDevices();
  }

  disconnectedCallback() {
    cancelAnimationFrame(this.rafId);
    clearTimeout(this.liveTimer);
    this.resize?.disconnect();
    this.stopTheme?.();
    this.engine?.removeEventListener("change", this.onChange);
    if (this.view) this.engine?.detachView(this.view);
    this.wf?.destroy();
    this.wf = undefined;
  }

  build() {
    // Toolbar: device picker, zoom, levels, spectrum toggle,
    // side panel toggle.
    this.select = /** @type {HTMLSelectElement} */ (el("select", { id: "rx-device", class: "max-w-full rounded border px-2 py-1" }));
    this.select.addEventListener("change", () => {
      savePref(LAST_DEVICE_KEY, this.select.value);
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


    this.panelToggle = el("button", { type: "button", class: BUTTON, "aria-controls": "rx-panel" }, "Info");
    this.panelToggle.addEventListener("click", () => {
      this.engine.display.panel = !this.engine.display.panel;
      this.layout();
    });

    this.toolbar = el("div", { class: "flex flex-wrap items-center gap-x-4 gap-y-2" });
    this.toolbar.append(picker, zoom, levels, this.spToggle, this.panelToggle);

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
    this.display.append(this.spCanvas, this.scaleCanvas, this.wfCanvas, this.startBtn);
    for (const c of [this.spCanvas, this.scaleCanvas, this.wfCanvas]) this.bindPointer(c);
    this.wfCanvas.addEventListener("wheel", (e) => this.onWheel(e), { passive: false });

    this.empty = el("div", { class: "flex min-h-48 flex-col items-center justify-center gap-3 rounded border border-border bg-surface p-6 text-center" });
    this.empty.hidden = true;

    this.bar = this.buildBar();

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
    left.append(this.display, this.bar, this.empty, dl, this.status, this.meterLive);
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

    const bar = el("div", {
      class: "flex flex-wrap items-center gap-x-6 gap-y-3 rounded border border-border bg-surface p-3",
      role: "group",
      "aria-label": "Receiver controls",
    });
    bar.append(freq, mode, band, squelch, noise, smeter);
    return bar;
  }

  // buildPanel returns the side panel (UI-019) with its Info tab: the
  // device, its node and the station.
  buildPanel() {
    const panel = el("section", { id: "rx-panel", class: "flex min-w-0 flex-col self-start gap-3 rounded border border-border bg-surface p-3", "aria-labelledby": "rx-info-title" });
    this.info = el("dl", { class: "grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-sm" });
    panel.append(el("h2", { id: "rx-info-title", class: "text-lg font-semibold" }, "Info"), this.info);
    if (this.station) {
      this.station.classList.remove("mt-4");
      panel.append(this.station);
    }
    return panel;
  }

  // fetchDevices reads the devices the visitor may listen to.
  async fetchDevices() {
    try {
      const res = await fetch(FEATURES_URL, { credentials: "same-origin", headers: { Accept: "application/json" } });
      if (!res.ok) throw new Error(`features API answered ${res.status}`);
      const body = await res.json();
      this.devices = Array.isArray(body?.devices) ? body.devices : [];
      this.devicesError = false;
    } catch {
      this.devices = [];
      this.devicesError = true;
    }
  }

  // refreshDevices re-reads the devices after a lost connection, so that an
  // offline node shows as such while the engine keeps retrying.
  async refreshDevices() {
    await this.fetchDevices();
    if (this.isConnected) this.layout();
  }

  async loadDevices() {
    await this.fetchDevices();
    this.loaded = true;
    if (!this.isConnected) return;

    this.select.replaceChildren();
    for (const d of this.devices) {
      this.select.append(el("option", { value: `${d.node_id}/${d.id}` }, d.name));
    }
    const key = (/** @type {any} */ d) => `${d.node_id}/${d.id}`;
    const t = this.engine.target;
    const last = loadPref(LAST_DEVICE_KEY);
    const first =
      (t && this.devices.find((d) => d.node_id === t.node_id && d.id === t.device_id)) ??
      this.devices.find((d) => key(d) === last) ??
      this.devices.find((d) => d.node_online) ??
      this.devices[0];
    if (first) {
      this.select.value = key(first);
      this.choose(this.select.value);
    }
    this.layout();
  }

  /** @param {string} key "<node_id>/<device_id>" */
  choose(key) {
    const d = this.devices.find((x) => `${x.node_id}/${x.id}` === key);
    if (!d) return;
    const t = this.engine.target;
    const same = t && t.node_id === d.node_id && t.device_id === d.id;
    // features' "online" means the device runs, which it only does while
    // someone listens: every listed device is connected to.
    if (!same) {
      Object.assign(this.engine.display, { zoom: 1, start: 0, levels: null });
      this.engine.connect({ node_id: d.node_id, device_id: d.id, name: d.name });
    } else if (!this.engine.ws && !this.engine.retryTimer) {
      this.engine.disconnect();
      this.engine.connect({ node_id: d.node_id, device_id: d.id, name: d.name });
    }
    this.layout();
  }

  /** @returns {any} the chosen device of the features list */
  chosen() {
    return this.devices.find((x) => `${x.node_id}/${x.id}` === this.select.value);
  }

  /** @param {string} what */
  changed(what) {
    if (what === "state") {
      if (this.engine.state === "reconnecting" && this.loaded) this.refreshDevices();
      this.announce();
      this.layout();
    } else if (what === "config") {
      this.reconfigure();
      this.layout();
    } else if (what === "tune") {
      this.follow();
      this.syncControls();
    } else if (what === "audio") {
      this.layout();
    }
  }

  // layout shows the display or the empty state (RX-005), and syncs the
  // controls with the engine.
  layout() {
    const e = this.engine;
    const msg = this.emptyMessage();
    this.display.hidden = msg !== null;
    this.bar.hidden = msg !== null;
    this.empty.hidden = msg === null;
    if (msg !== null) {
      this.empty.replaceChildren(el("p", { class: "font-semibold" }, msg.text));
      if (msg.login) {
        const next = encodeURIComponent(location.pathname + location.search);
        this.empty.append(el("a", { href: `${this.cfg.login_url || "/login"}?next=${next}`, "hx-boost": "false" }, "Sign in"));
      }
    }
    this.toolbar.hidden = this.devices.length === 0;
    this.spCanvas.hidden = !e.display.spectrum;
    this.spToggle.setAttribute("aria-pressed", String(e.display.spectrum));
    const open = e.display.panel && this.devices.length > 0;
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
    this.sizeCanvases();
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

  // renderInfo fills the Info tab: the device and its node (features list),
  // and what the node says of the device (device.config).
  renderInfo() {
    const c = this.chosen();
    const e = this.engine;
    /** @type {[string, string][]} */
    const rows = [];
    if (c) {
      rows.push(["Device", c.name], ["Node", c.node_id], ["Node status", c.node_online ? "Online" : "Offline"]);
      const modes = ANALOG_MODES.filter((m) => (c.modes ?? []).includes(m));
      if (modes.length) rows.push(["Modes", modes.map((m) => m.toUpperCase()).join(", ")]);
    }
    if (e.device && e.target && c && e.target.device_id === c.id && e.target.node_id === c.node_id) {
      if (e.centerHz) rows.push(["Centre", formatMHz(e.centerHz)]);
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
    if (!this.loaded) return { text: "Loading the receiver…" };
    if (this.devicesError) return { text: "The receiver could not load its devices. Reload the page to try again." };
    if (this.devices.length === 0) {
      if (!this.cfg.signed_in) return { text: "Sign in to listen to this receiver.", login: true };
      return { text: "No receiver device is available." };
    }
    const d = this.chosen();
    if (d && d.node_online === false && e.state !== "listening" && e.state !== "connected") {
      return { text: "This device's node is offline." };
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
        return "Connection lost, reconnecting…";
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
