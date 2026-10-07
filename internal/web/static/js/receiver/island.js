// <msdr-receiver>: the receiver island (RX-002, RX-015, UI-017). Its initial
// state comes from a templ.JSONScript child (ADR 0003 5A): whether the
// visitor is signed in and the sign-in URL. It lists the devices the visitor
// may listen to (GET /api/v1/features), has the shell-level engine listen to
// the chosen one, and draws a view of it: optional spectrum (RX-016),
// frequency scale with the filter envelope (RX-019) and waterfall (RX-015),
// with click/drag tuning, zoom, and levels from the device defaults plus an
// auto action (RX-017/018). The view detaches when htmx swaps #main away;
// the engine, and so the audio, keeps running.
//
// Accessibility (UI-009, ADR 0015 decision 12): canvases are role="img" and
// described by a text list (frequency, mode, signal level) refreshed every
// second, not live; connection states are announced in a role="status"
// region at most every 2 s, the last state winning. Untrusted text (device
// names) goes through textContent only.

import { onThemeChange } from "../tokens.js";
import { getEngine } from "./engine.js";
import { FreqScale } from "./scale.js";
import { Spectrum } from "./spectrum.js";
import { Waterfall2D } from "./waterfall-2d.js";

const FEATURES_URL = "/api/v1/features";
const LIVE_MIN_INTERVAL_MS = 2000;
const TEXT_INTERVAL_MS = 1000;
const SPECTRUM_INTERVAL_MS = 150;
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

const BUTTON = "rounded border border-border px-3 py-1 text-sm";

class MsdrReceiver extends HTMLElement {
  connectedCallback() {
    const script = this.querySelector('script[type="application/json"]');
    /** @type {{signed_in?: boolean, login_url?: string}} */
    this.cfg = JSON.parse(script?.textContent || "{}");
    this.engine = getEngine();
    /** @type {any[]} */
    this.devices = [];
    this.devicesError = false;
    this.loaded = false;
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
    this.lastLive = 0;
    this.lastLiveText = "";
    this.loop = this.loop.bind(this);
    this.rafId = requestAnimationFrame(this.loop);

    this.sizeCanvases();
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
    // Toolbar: device picker, zoom, levels, spectrum toggle.
    this.select = /** @type {HTMLSelectElement} */ (el("select", { id: "rx-device", class: "rounded border px-2 py-1" }));
    this.select.addEventListener("change", () => this.choose(this.select.value));
    const picker = el("div", { class: "flex items-center gap-2" });
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
    const levels = el("div", { class: "flex items-center gap-1", role: "group", "aria-label": "Waterfall levels" });
    levels.append(auto, reset);

    this.spToggle = el("button", { type: "button", class: BUTTON, "aria-pressed": "true" }, "Spectrum");
    this.spToggle.addEventListener("click", () => {
      this.engine.display.spectrum = !this.engine.display.spectrum;
      this.layout();
    });

    this.toolbar = el("div", { class: "flex flex-wrap items-center gap-x-4 gap-y-2" });
    this.toolbar.append(picker, zoom, levels, this.spToggle);

    // Display: spectrum, scale and waterfall; the empty state replaces them.
    const describe = { role: "img", "aria-describedby": "rx-text" };
    this.spCanvas = /** @type {HTMLCanvasElement} */ (
      el("canvas", { ...describe, class: "block h-32 w-full touch-pan-y", "aria-label": "Spectrum" })
    );
    this.scaleCanvas = /** @type {HTMLCanvasElement} */ (
      el("canvas", { ...describe, class: "block h-6 w-full touch-pan-y", "aria-label": "Frequency scale" })
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

    const root = el("div", { class: "mt-4 flex flex-col gap-3" });
    root.append(this.toolbar, this.display, this.empty, dl, this.status);
    this.replaceChildren(root);
    this.layout();
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
    const t = this.engine.target;
    const current = t && this.devices.find((d) => d.node_id === t.node_id && d.id === t.device_id);
    const first = current ?? this.devices[0];
    if (first) {
      this.select.value = `${first.node_id}/${first.id}`;
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
    const i = ZOOMS.indexOf(e.display.zoom);
    this.zoomText.textContent = `×${e.display.zoom}`;
    this.zoomOut.toggleAttribute("disabled", i <= 0);
    this.zoomIn.toggleAttribute("disabled", i >= ZOOMS.length - 1 || this.viewBins(ZOOMS[i + 1]) < MIN_VIEW_BINS);
    this.startBtn.hidden = e.audio.running || !e.demod;
    this.sizeCanvases();
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

  /** @param {HTMLCanvasElement} canvas */
  bindPointer(canvas) {
    canvas.addEventListener("pointerdown", (ev) => {
      if (ev.button !== 0) return;
      canvas.setPointerCapture(ev.pointerId);
      this.dragging = true;
      this.tuneAt(canvas, ev.clientX);
    });
    canvas.addEventListener("pointermove", (ev) => {
      if (this.dragging) this.tuneAt(canvas, ev.clientX);
    });
    const stop = () => {
      this.dragging = false;
    };
    canvas.addEventListener("pointerup", stop);
    canvas.addEventListener("pointercancel", stop);
  }

  /** @param {HTMLCanvasElement} canvas @param {number} clientX */
  tuneAt(canvas, clientX) {
    const win = this.window();
    if (!win) return;
    const r = canvas.getBoundingClientRect();
    const f = Math.min(1, Math.max(0, (clientX - r.left) / r.width));
    this.engine.tune(win.startHz + f * win.spanHz);
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

  updateText() {
    const e = this.engine;
    this.freqText.textContent = e.tunedHz ? formatMHz(e.tunedHz) : "—";
    this.modeText.textContent = e.demod?.mode ? e.demod.mode.toUpperCase() : "—";
    if (e.meter) {
      this.levelText.textContent = `${e.meter.levelDb.toFixed(1)} dB${e.meter.open ? "" : " (squelched)"}`;
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
        return `Receiving ${name}.`;
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
