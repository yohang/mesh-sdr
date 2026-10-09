// Map island (MAP-001, MAP-002, MAP-004 to MAP-006, MAP-008 to MAP-012,
// MAP-014 to MAP-016, MAP-018, FEATURE_SPEC §10.10, ADR 0029): <msdr-map>
// draws the map features on the vendored Leaflet.
//
// Feed: it loads GET /api/v1/map/config (layers, receivers),
// /api/v1/map/features and /api/v1/features (the names and nodes of the
// devices) and /api/v1/bandplan (the amateur bands), then applies the map.feature.upsert and map.feature.remove
// deltas of the map topic (data-msdr-topics="map", static/js/events.js).
// It loads again on every msdr:resync, which the dispatcher fires after the
// subscription is acknowledged: on the first load (so no event is missed),
// after navigation and after a reconnection; deltas that arrive during a
// load are replayed on top of it. A failed load is retried with the events
// back-off. While the socket is down the map keeps its last state, marked
// stale.
//
// Drawing: the features of one kind and subject heard by several devices
// are one entity (store.js). Changes are applied at most four times a
// second; above 1000 entities the vectors move to Leaflet's canvas
// renderer. Every type has a shape and a color (kinds.js): APRS stations
// are dots with their track and a heading tick when the course is known,
// receivers triangles. The stations of one Maidenhead square are one
// square (MAP-009) and calls are dashed great circle lines (MAP-011); both
// are coloured by band, by mode or by type (the colour mode, MAP-010,
// colours.js), and the band or mode legend isolates one. Features fade over
// the second half of their life and are dropped at expires_at.
//
// Panels: Layers (base layer and overlays: day and night, the Maidenhead
// grid and the tile overlays of the config; remembered by this browser with
// savePref, never on the account), the Features legend (per-type visibility, counts,
// a labelled collapse button; per-session state), the detail panel
// (detail.js) and the list view (list.js), the accessible equivalent of the
// map. Tile attribution is plain text.
//
// URL (MAP-016): ?callsign=, ?locator=, ?icao=, ?mmsi= centre the map on
// the item and open its detail when it arrives; ?layers= (comma-separated
// feature types, a base layer id and/or overlay ids) sets what is shown; ?node= shows
// the features heard by the devices of one node only. They combine.

import { getJSON } from "../csrf.js";
import { backoff, eventsState } from "../events.js";
import { getState, loadPref, savePref, setState } from "../receiver/session.js";
import { el } from "../receiver/dom.js";
import { onThemeChange, readToken } from "../tokens.js";
import { ColourLegend, colourKey, hamBands, SERIES } from "./colours.js";
import { renderDetail, renderSquare } from "./detail.js";
import { finite, isLocator, locatorBounds } from "./geo.js";
import { greatCircle } from "./greatcircle.js";
import { maidenheadGrid } from "./grid.js";
import { isType, TYPES, typeOf } from "./kinds.js";
import { loadLeaflet } from "./leaflet.js";
import { ListView } from "./list.js";
import { Store } from "./store.js";
import { nightPolygon } from "./terminator.js";

const CONFIG_URL = "/api/v1/map/config";
const FEATURES_URL = "/api/v1/map/features";
const DEVICES_URL = "/api/v1/features";
const BANDPLAN_URL = "/api/v1/bandplan";
const LAYER_PREF = "msdr.map.layer";
const OVERLAY_PREF = "msdr.map.overlays";
const NIGHT_EVERY_MS = 60000; // the terminator moves 0.25° a minute
const COLOURED = new Set(["locator", "call"]);
const COLOUR_MODES = ["band", "mode", "off"];
const MIN_FLUSH_MS = 250; // at most 4 redraws per second
const CANVAS_ABOVE = 1000;
const FADE_TICK_MS = 15000;
const LIST_EVERY_MS = 2000;
const STATUS_EVERY_MS = 2000;
const FIRST_LOAD_WAIT_MS = 1500;
const HEADING_PX = 18;
const FOCUS_ZOOM = 9;

/** @typedef {import("./store.js").Entity} Entity */

/**
 * opacity is the opacity of an entity: full over the first half of its
 * life, then fading to 0.15 at its expiry (MAP-012).
 * @param {Entity} e @param {number} now
 */
function opacity(e, now) {
  if (!finite(e.expires_at) || e.expires_at <= e.updated_at) return 1;
  const half = (e.expires_at - e.updated_at) / 2;
  const age = now - e.updated_at;
  if (age <= half) return 1;
  return Math.max(0.15, 1 - ((age - half) / half) * 0.85);
}

/**
 * param reads a URL parameter, trimmed and at most 64 characters.
 * @param {URLSearchParams} q @param {string} name
 */
function param(q, name) {
  return (q.get(name) ?? "").trim().slice(0, 64);
}

class MapIsland extends HTMLElement {
  connectedCallback() {
    this.store = new Store();
    /** @type {Map<string, any>} Leaflet layers by entity id */
    this.layers = new Map();
    /** @type {Map<string, import("./detail.js").DeviceInfo>} */
    this.devices = new Map();
    /** @type {Map<string, string>} node names by id */
    this.nodes = new Map();
    /** @type {any[] | null} deltas received during a load */
    this.buffer = null;
    this.loading = false;
    this.again = false;
    this.loaded = false;
    this.attempt = 0;
    this.lastFlush = 0;
    this.lastList = 0;
    this.selected = "";
    this.rendererKind = "";
    this.colorVersion = 0;
    this.config = /** @type {any} */ (null);
    /** @type {import("./colours.js").Band[]} */
    this.bands = [];
    /** @type {Map<string, string>} the square of each locator entity */
    this.squareOf = new Map();
    /** @type {Map<string, Set<string>>} the locator entities of each square */
    this.squares = new Map();
    /** @type {Map<string, any>} the overlays shown, by id */
    this.overlays = new Map();
    const colourMode = getState("map.colour", "band");
    this.colourMode = COLOUR_MODES.includes(colourMode) ? colourMode : "band";
    this.isolated = this.colourMode === "off" ? "" : getState("map.isolate", "");
    this.status = this.querySelector("[data-map-status]");
    this.notice = /** @type {HTMLElement} */ (this.querySelector("[data-map-notice]"));
    this.signedIn = false;
    try {
      const c = JSON.parse(this.querySelector("#msdr-map-config")?.textContent ?? "{}");
      this.signedIn = c.signed_in === true;
      this.loginURL = typeof c.login_url === "string" ? c.login_url : "/login";
    } catch {
      this.loginURL = "/login";
    }
    this.readURL();
    this.abort = new AbortController();
    const on = (/** @type {string} */ type, /** @type {(e: any) => void} */ fn) =>
      document.body.addEventListener(type, fn, { signal: this.abort.signal });
    on("msdr:map.feature.upsert", (ev) => this.delta("upsert", ev.detail?.payload));
    on("msdr:map.feature.remove", (ev) => this.delta("remove", ev.detail?.payload?.key));
    on("msdr:resync", () => this.load());
    on("msdr:events.state", () => this.showNotices());
    this.stopTheme = onThemeChange(() => {
      this.readColors();
      this.redrawAll();
    });
    // The dispatcher fires msdr:resync once subscribed; without a socket,
    // load anyway.
    this.firstLoad = setTimeout(() => this.load(), FIRST_LOAD_WAIT_MS);
    this.fade = setInterval(() => this.redrawAll(), FADE_TICK_MS);
    this.start().catch(() => this.say("The map could not be loaded. Reload the page to try again."));
  }

  disconnectedCallback() {
    this.abort?.abort();
    this.stopTheme?.();
    clearTimeout(this.firstLoad);
    clearTimeout(this.retry);
    clearTimeout(this.timer);
    clearTimeout(this.statusTimer);
    clearTimeout(this.expiry);
    clearInterval(this.fade);
    clearInterval(this.nightTimer);
    this.map?.remove();
    this.map = null;
  }

  // readURL reads the cross-section link parameters (MAP-016).
  readURL() {
    const q = new URLSearchParams(location.search);
    /** @type {{field: string, value: string}[]} */
    this.pending = [];
    for (const field of ["callsign", "icao", "mmsi", "locator"]) {
      const value = param(q, field);
      if (value) this.pending.push({ field, value: value.toUpperCase() });
    }
    this.focusLocator = isLocator(param(q, "locator")) ? param(q, "locator") : "";
    this.node = param(q, "node");
    /** @type {Set<string> | null} types asked for by ?layers= */
    this.urlTypes = null;
    /** @type {string[]} base layer and overlay ids asked for by ?layers= */
    this.urlLayers = [];
    const layers = param(q, "layers")
      .split(",")
      .map((s) => s.trim())
      .filter(Boolean);
    for (const id of layers) {
      if (isType(id)) (this.urlTypes ??= new Set()).add(id);
      else if (/^[a-z0-9_]{1,64}$/.test(id)) this.urlLayers.push(id);
    }
  }

  // start loads Leaflet and builds the map and its panels.
  async start() {
    const L = await loadLeaflet();
    if (!this.isConnected) return;
    this.L = L;
    this.querySelector("[data-map-fallback]")?.remove();
    const stage = /** @type {HTMLElement} */ (this.querySelector("[data-map-stage]"));
    stage.hidden = false;
    for (const b of this.querySelectorAll("[data-map-toggle], [data-map-list-toggle], [data-map-search], [data-map-colour-label]")) {
      /** @type {HTMLElement} */ (b).hidden = false;
    }

    const canvas = this.querySelector("[data-map-canvas]");
    this.map = L.map(canvas, { attributionControl: false, zoomControl: false, worldCopyJump: true, minZoom: 2 });
    this.map.setView([30, 0], 2);
    // Night shading under the features (overlayPane is at 400).
    this.map.createPane("night").style.zIndex = "350";
    const Credit = L.Control.extend({
      options: { position: "bottomright" },
      onAdd: () => {
        this.credit = L.DomUtil.create("p", "map-credit");
        L.DomEvent.disableClickPropagation(this.credit);
        return this.credit;
      },
    });
    new Credit().addTo(this.map);
    L.control.zoom({ position: "bottomright" }).addTo(this.map);
    this.map.on("zoomend", () => this.redrawAll());
    this.readColors();

    this.list = new ListView(/** @type {HTMLElement} */ (this.querySelector("[data-map-list]")), (e, opener) => this.select(e.id, { opener }));
    this.setupToggles();
    this.setupLegend();
    this.setupColours();
    this.setupSearch();
    this.setupDetail();
    if (this.focusLocator) {
      const b = locatorBounds(this.focusLocator);
      if (b) this.map.fitBounds(b, { maxZoom: FOCUS_ZOOM });
    }
    if (this.config) this.applyConfig();
    if (this.loaded) {
      this.showNotices();
      this.fitFirst();
    }
    this.schedule();
  }

  // readColors reads the marker colors from the design tokens.
  readColors() {
    /** @type {Record<string, string>} */
    this.colors = {};
    for (const t of TYPES) this.colors[t.color] = readToken(t.color) || "#0b5cad";
    this.colors.surface = readToken("color-surface") || "#ffffff";
    this.colors.focus = readToken("color-focus") || "#9a3412";
    this.colors.ink = readToken("color-map-ink") || "#1b1f24";
    for (let i = 1; i <= SERIES; i++) this.colors[`series-${i}`] = readToken(`color-series-${i}`) || "#0b5cad";
    this.colorVersion++;
  }

  /**
   * say sets the status region, at most every 2 s (the last text wins).
   * @param {string} text
   */
  say(text) {
    this.statusText = text;
    if (this.statusTimer) return;
    const flush = () => {
      if (this.status && this.status.textContent !== this.statusText) this.status.textContent = this.statusText ?? "";
      this.statusTimer = setTimeout(() => {
        this.statusTimer = 0;
        if (this.status?.textContent !== this.statusText) this.say(this.statusText ?? "");
      }, STATUS_EVERY_MS);
    };
    flush();
  }

  /**
   * delta applies a map.feature.* event, or keeps it for after the load.
   * @param {"upsert" | "remove"} op @param {any} value
   */
  delta(op, value) {
    if (this.buffer) this.buffer.push([op, value]);
    if (op === "upsert") this.store.upsert(value);
    else this.store.remove(value);
    this.schedule();
  }

  // load (re)loads the configuration and the features.
  async load() {
    clearTimeout(this.firstLoad);
    clearTimeout(this.retry);
    if (this.loading) {
      this.again = true;
      return;
    }
    this.loading = true;
    this.buffer = [];
    try {
      const [config, features, devices, plan] = await Promise.all([
        getJSON(CONFIG_URL),
        getJSON(FEATURES_URL),
        getJSON(DEVICES_URL),
        // Without the band plan, squares and calls are coloured by mode only.
        getJSON(BANDPLAN_URL).catch(() => null),
      ]);
      if (!this.isConnected) return;
      if (plan) {
        this.bands = hamBands(plan);
        this.colorVersion++;
      }
      this.attempt = 0;
      this.devices = new Map();
      for (const d of Array.isArray(devices?.devices) ? devices.devices : []) {
        if (typeof d?.id === "string" && typeof d.node_id === "string") {
          this.devices.set(d.id, { id: d.id, name: String(d.name ?? ""), node_id: d.node_id });
        }
      }
      this.nodes = new Map();
      for (const n of Array.isArray(devices?.nodes) ? devices.nodes : []) {
        if (typeof n?.id === "string") this.nodes.set(n.id, String(n.name ?? n.id));
      }
      this.truncated = features?.truncated === true;
      this.store.replace(Array.isArray(features?.features) ? features.features : [], Array.isArray(config?.receivers) ? config.receivers : []);
      for (const [op, value] of this.buffer) {
        if (op === "upsert") this.store.upsert(value);
        else this.store.remove(value);
      }
      const first = !this.loaded;
      this.loaded = true;
      this.config = config;
      if (this.map) this.applyConfig();
      this.showNotices();
      this.schedule();
      if (first) this.say("");
      if (first && this.map) this.fitFirst();
    } catch {
      if (!this.isConnected) return;
      const delay = backoff(this.attempt++, 2000, 60000);
      this.say("The map features could not be loaded; retrying.");
      this.retry = setTimeout(() => this.load(), delay);
    } finally {
      this.buffer = null;
      this.loading = false;
      if (this.again && this.isConnected) {
        this.again = false;
        this.load();
      }
    }
  }

  // fitFirst frames the features on the first load, unless a link asked
  // for a place.
  fitFirst() {
    if (this.pending.length > 0 || this.focusLocator) return;
    /** @type {[number, number][]} */
    const points = [];
    for (const id of this.store.ids()) {
      const e = this.store.entity(id);
      if (e?.at && this.shown(e)) points.push(e.at);
    }
    if (points.length === 0) {
      for (const r of this.store.receivers.values()) points.push([r.lat, r.lon]);
    }
    if (points.length === 1) this.map.setView(points[0], 8);
    else if (points.length > 1) this.map.fitBounds(points, { maxZoom: 8, padding: [24, 24] });
  }

  // applyConfig sets the base layers (when they changed).
  applyConfig() {
    const layers = Array.isArray(this.config?.base_layers) ? this.config.base_layers : [];
    this.applyOverlays();
    const sig = JSON.stringify(layers.map((/** @type {any} */ l) => l.id));
    if (sig === this.layersSig) return;
    this.layersSig = sig;
    this.baseLayers = layers;
    const box = /** @type {HTMLElement} */ (this.querySelector("[data-map-bases]"));
    box.replaceChildren();
    const ids = layers.map((/** @type {any} */ l) => l.id);
    const saved = loadPref(LAYER_PREF) ?? "";
    const current = [...this.urlLayers, saved, this.config?.default_base_layer].find((id) => id && ids.includes(id)) ?? ids[0] ?? "";
    if (layers.length === 0) box.append(el("p", { class: "text-sm text-fg-muted" }, "No base layer is offered."));
    for (const l of layers) {
      const id = `map-base-${l.id}`;
      const label = el("label", { class: "flex items-center gap-2 text-sm max-md:min-h-11", for: id });
      const input = /** @type {HTMLInputElement} */ (el("input", { type: "radio", name: "map-base", id, value: l.id }));
      input.checked = l.id === current;
      input.addEventListener("change", () => {
        if (!input.checked) return;
        savePref(LAYER_PREF, l.id);
        this.setBase(l.id);
      });
      label.append(input, document.createTextNode(String(l.name ?? l.id)));
      box.append(label);
    }
    this.setBase(current);
  }

  /**
   * setBase shows a base layer and its attribution.
   * @param {string} id
   */
  setBase(id) {
    const l = (this.baseLayers ?? []).find((/** @type {any} */ x) => x.id === id);
    this.tiles?.remove();
    this.tiles = l ? this.tileLayer(l, 1) : null;
    this.tiles?.addTo(this.map).bringToBack();
    this.showCredit();
  }

  /**
   * tileLayer returns the tile layer of a base layer or overlay of the
   * config; null unless its URL is https.
   * @param {any} l @param {number} zIndex
   */
  tileLayer(l, zIndex) {
    if (typeof l.url !== "string" || !l.url.startsWith("https://")) return null;
    const layer = this.L.tileLayer(l.url, {
      subdomains: typeof l.subdomains === "string" && l.subdomains ? l.subdomains : "abc",
      maxZoom: finite(l.max_zoom) ? l.max_zoom : 18,
      attribution: "",
      detectRetina: false,
      zIndex,
      // The page's Referrer-Policy (same-origin) sends no Referer to the
      // tile servers, and the OSM tile usage policy
      // (https://operations.osmfoundation.org/policies/tiles/) requires a
      // valid one: tile images send the hub's origin only, nothing else.
      referrerPolicy: "strict-origin",
    });
    layer.credit = String(l.attribution ?? "");
    return layer;
  }

  // showCredit sets the attribution of the tiles shown.
  showCredit() {
    if (!this.credit) return;
    const parts = [this.tiles, ...this.overlays.values()].map((l) => l?.credit).filter(Boolean);
    this.credit.textContent = parts.join(" | ");
  }

  // applyOverlays lists the overlays (when they changed): day and night
  // (MAP-006, on by default), the Maidenhead grid and the tile overlays of
  // the config (MAP-005).
  applyOverlays() {
    const tiles = (Array.isArray(this.config?.overlays) ? this.config.overlays : []).filter(
      (/** @type {any} */ l) => typeof l?.id === "string" && typeof l.url === "string",
    );
    const all = [{ id: "night", name: "Day and night (shaded night side)" }, { id: "grid", name: "Maidenhead grid" }, ...tiles];
    const sig = JSON.stringify(all.map((o) => o.id));
    if (sig === this.overlaysSig) return;
    this.overlaysSig = sig;
    const saved = loadPref(OVERLAY_PREF);
    const on = new Set(saved === null ? ["night"] : saved.split(","));
    for (const id of this.urlLayers) on.add(id);
    const box = /** @type {HTMLElement} */ (this.querySelector("[data-map-overlays]"));
    box.replaceChildren();
    for (const o of all) {
      const id = `map-overlay-${o.id}`;
      const label = el("label", { class: "flex items-center gap-2 text-sm max-md:min-h-11", for: id });
      const input = /** @type {HTMLInputElement} */ (el("input", { type: "checkbox", id }));
      input.checked = on.has(o.id);
      input.addEventListener("change", () => {
        this.setOverlay(o, input.checked);
        savePref(OVERLAY_PREF, [...this.overlays.keys()].join(","));
      });
      label.append(input, document.createTextNode(String(o.name ?? o.id)));
      box.append(label);
      this.setOverlay(o, input.checked);
    }
  }

  /**
   * setOverlay shows or hides an overlay.
   * @param {any} o @param {boolean} on
   */
  setOverlay(o, on) {
    this.overlays.get(o.id)?.remove();
    this.overlays.delete(o.id);
    if (o.id === "night") clearInterval(this.nightTimer);
    const layer = on ? this.overlayLayer(o) : null;
    if (layer) {
      layer.addTo(this.map);
      this.overlays.set(o.id, layer);
    }
    this.showCredit();
  }

  /**
   * overlayLayer builds the layer of an overlay.
   * @param {any} o
   */
  overlayLayer(o) {
    if (o.id === "grid") return maidenheadGrid(this.L, () => this.colors.ink);
    if (o.id !== "night") return this.tileLayer(o, 2);
    const night = this.L.polygon(nightPolygon(Date.now()), {
      pane: "night", interactive: false, stroke: false, fillColor: this.colors.ink, fillOpacity: 0.25,
    });
    this.nightTimer = setInterval(() => night.setLatLngs(nightPolygon(Date.now())), NIGHT_EVERY_MS);
    return night;
  }

  // showNotices shows the non-live notices: stale map, truncated list,
  // node filter, sign-in hint.
  showNotices() {
    if (!this.notice) return;
    /** @type {(string | Node)[]} */
    const parts = [];
    const state = eventsState();
    if (this.loaded && (state === "closed" || state === "stopped")) {
      parts.push("Live updates are paused: the map shows the last known state. ");
    }
    if (this.truncated) parts.push("Only the newest 5000 features are shown. ");
    if (this.node) {
      const q = new URLSearchParams(location.search);
      q.delete("node");
      const s = q.toString();
      parts.push(`Only the features heard by the node ${this.nodes.get(this.node) ?? this.node} are shown. `);
      parts.push(el("a", { href: `/map${s ? `?${s}` : ""}`, class: "underline" }, "Show every node"));
      parts.push(" ");
    }
    if (this.loaded && this.devices.size === 0 && !this.signedIn) {
      parts.push("No receiver is open to visitors. ");
      parts.push(el("a", { href: this.loginURL, class: "underline" }, "Sign in"));
      parts.push(" to see what the receivers hear.");
    }
    this.notice.replaceChildren(...parts);
    this.notice.hidden = parts.length === 0;
    window.htmx?.process?.(this.notice);
  }

  // setupToggles wires the Layers, Legend and list view buttons.
  setupToggles() {
    const legendOpen = getState("map.legend", matchMedia("(width >= 48rem)").matches);
    for (const b of this.querySelectorAll("[data-map-toggle]")) {
      const btn = /** @type {HTMLButtonElement} */ (b);
      const panel = /** @type {HTMLElement | null} */ (document.getElementById(btn.getAttribute("aria-controls") ?? ""));
      if (!panel) continue;
      const set = (/** @type {boolean} */ open) => {
        panel.hidden = !open;
        btn.setAttribute("aria-expanded", String(open));
        if (btn.dataset.mapToggle === "legend") setState("map.legend", open);
      };
      set(btn.dataset.mapToggle === "legend" ? legendOpen : false);
      btn.addEventListener("click", () => set(panel.hidden));
      panel.addEventListener("keydown", (ev) => {
        if (ev.key !== "Escape") return;
        ev.stopPropagation();
        set(false);
        btn.focus();
      });
    }

    const toggle = /** @type {HTMLButtonElement} */ (this.querySelector("[data-map-list-toggle]"));
    const stage = /** @type {HTMLElement} */ (this.querySelector("[data-map-stage]"));
    const list = /** @type {HTMLElement} */ (this.querySelector("[data-map-list]"));
    toggle.addEventListener("click", () => {
      const listing = toggle.getAttribute("aria-pressed") !== "true";
      toggle.setAttribute("aria-pressed", String(listing));
      stage.hidden = listing;
      list.hidden = !listing;
      for (const b of this.querySelectorAll("[data-map-toggle]")) /** @type {HTMLElement} */ (b).hidden = listing;
      if (listing) this.renderList(true);
      else this.map.invalidateSize();
    });
    list.addEventListener("focusout", () => setTimeout(() => this.renderList(false), 0));
  }

  // setupLegend builds the Features legend: one checkbox per type, with its
  // symbol and count (MAP-008).
  setupLegend() {
    const list = /** @type {HTMLElement} */ (this.querySelector("[data-map-types]"));
    /** @type {Record<string, boolean>} */
    const saved = getState("map.types", {});
    /** @type {Map<string, boolean>} */
    this.visible = new Map();
    /** @type {Map<string, {li: HTMLElement, count: HTMLElement, input: HTMLInputElement}>} */
    this.legend = new Map();
    for (const t of TYPES) {
      const on = this.urlTypes ? this.urlTypes.has(t.id) : typeof saved[t.id] === "boolean" ? saved[t.id] : t.shown;
      this.visible.set(t.id, on);
      const id = `map-type-${t.id}`;
      const li = el("li");
      const label = el("label", { for: id, class: "flex items-center gap-2 text-sm max-md:min-h-11" });
      const input = /** @type {HTMLInputElement} */ (el("input", { type: "checkbox", id }));
      input.checked = on;
      input.addEventListener("change", () => {
        this.visible.set(t.id, input.checked);
        /** @type {Record<string, boolean>} */
        const all = {};
        for (const [k, v] of this.visible) all[k] = v;
        setState("map.types", all);
        this.redrawAll();
      });
      const count = el("span", { class: "ml-auto tabular-nums text-fg-muted" }, "0");
      label.append(input, el("span", { class: `map-sym map-sym-${t.symbol}` }), el("span", {}, t.label), count);
      li.append(label);
      li.hidden = t.optional;
      list.append(li);
      this.legend.set(t.id, { li, count, input });
    }
  }

  // setupColours wires the colour mode (per-session state) and the band or
  // mode legend (MAP-010).
  setupColours() {
    const select = /** @type {HTMLSelectElement} */ (this.querySelector("[data-map-colour]"));
    select.value = this.colourMode;
    select.addEventListener("change", () => {
      this.colourMode = COLOUR_MODES.includes(select.value) ? select.value : "band";
      setState("map.colour", this.colourMode);
      this.isolate("");
    });
    this.keys = new ColourLegend(/** @type {HTMLElement} */ (this.querySelector("[data-map-keys]")), (id) =>
      this.isolate(this.isolated === id ? "" : id),
    );
  }

  /**
   * isolate shows the squares and calls of one band or mode only ("": all).
   * @param {string} id
   */
  isolate(id) {
    this.isolated = id;
    setState("map.isolate", id || undefined);
    this.colorVersion++;
    this.redrawAll();
  }

  /**
   * keyOf returns the band or mode an entity is coloured by; null for the
   * types that keep their color, and when the colour mode is off.
   * @param {Entity} e
   */
  keyOf(e) {
    return e.feature && COLOURED.has(e.kind) ? colourKey(this.colourMode, e.feature.details, this.bands) : null;
  }

  /**
   * colourOf returns the color of an entity: its band's or mode's, else
   * its type's.
   * @param {Entity} e
   */
  colourOf(e) {
    const k = this.keyOf(e);
    return k && k.series >= 0 ? this.colors[`series-${k.series + 1}`] : this.colors[typeOf(e.kind).color];
  }

  // setupSearch wires the toolbar search (FEATURE_SPEC §10.10).
  setupSearch() {
    const form = /** @type {HTMLFormElement} */ (this.querySelector("[data-map-search]"));
    form.addEventListener("submit", (ev) => {
      ev.preventDefault();
      const input = /** @type {HTMLInputElement} */ (form.querySelector("input"));
      const q = input.value.trim().slice(0, 64);
      if (!q) return;
      const found = this.find(q.toUpperCase(), true);
      if (found) {
        this.select(found.id, { centre: true, opener: input });
        this.say(`Found ${found.subject}.`);
        return;
      }
      const b = isLocator(q) ? locatorBounds(q) : null;
      if (b) {
        this.map.fitBounds(b, { maxZoom: FOCUS_ZOOM });
        this.say(`Centred on the square ${q.toUpperCase()}; nothing is heard there.`);
        return;
      }
      this.say(`Nothing matches ${q}.`);
    });
  }

  // setupDetail wires the detail panel's close button and Escape.
  setupDetail() {
    const panel = /** @type {HTMLElement} */ (this.querySelector("#map-detail"));
    this.querySelector("[data-map-detail-close]")?.addEventListener("click", () => this.closeDetail());
    panel.addEventListener("keydown", (ev) => {
      if (ev.key === "Escape") {
        ev.stopPropagation();
        this.closeDetail();
      }
    });
    panel.addEventListener("focusout", () => setTimeout(() => this.detailStale && this.renderDetail(), 0));
  }

  /**
   * find returns the entity that matches a value: an exact callsign,
   * subject, locator, ICAO address or MMSI, else (loose) one whose name
   * contains it.
   * @param {string} value upper case @param {boolean} loose
   * @param {string} [field] the URL parameter it comes from
   */
  find(value, loose, field = "") {
    if ((field === "locator" || field === "") && isLocator(value) && value.length >= 4) {
      const square = this.entityOf(`square:${value.slice(0, 4)}`);
      if (square) return square;
    }
    /** @type {Entity | null} */
    let partial = null;
    for (const id of this.store.ids()) {
      const e = this.store.entity(id);
      if (!e || !this.byNode(e)) continue;
      const d = e.feature?.details ?? {};
      const g = e.feature?.geometry ?? {};
      const subject = e.subject.toUpperCase();
      /** @type {Record<string, unknown[]>} */
      const fields = {
        callsign: e.kind === "call" ? [] : [subject, d.callsign, d.source],
        locator: [g.locator, d.locator, e.receiver?.locator],
        icao: e.kind === "aircraft" ? [subject, d.icao] : [d.icao],
        mmsi: e.kind === "vessel" ? [subject, d.mmsi] : [d.mmsi],
      };
      const values = field ? fields[field] ?? [] : Object.values(fields).flat();
      if (values.some((v) => typeof v === "string" && v.toUpperCase() === value)) {
        // A locator square wins over a station that names it.
        if (field !== "locator" || e.kind === "locator") return e;
        partial ??= e;
      }
      if (loose && !partial && (subject.includes(value) || String(d.label ?? "").toUpperCase().includes(value))) partial = e;
    }
    return partial;
  }

  /**
   * byNode tells whether an entity passes the ?node= filter.
   * @param {Entity} e
   */
  byNode(e) {
    if (!this.node) return true;
    if (e.receiver) return e.receiver.devices.some((d) => this.devices.get(d.id)?.node_id === this.node) || e.id === `receiver:node:${this.node}`;
    return e.devices.some((d) => this.devices.get(d)?.node_id === this.node);
  }

  /**
   * shown tells whether an entity is drawn: its type is shown, it passes
   * the node filter and, for a square or call, the isolated band or mode.
   * @param {Entity} e
   */
  shown(e) {
    const k = this.isolated ? this.keyOf(e) : null;
    if (k && k.id !== this.isolated) return false;
    return this.visible?.get(typeOf(e.kind).id) !== false && this.byNode(e) && e.at !== null;
  }

  // schedule applies the changes, at most every MIN_FLUSH_MS.
  schedule() {
    if (this.timer || !this.map) return;
    const wait = Math.max(0, this.lastFlush + MIN_FLUSH_MS - performance.now());
    this.timer = setTimeout(() => {
      this.timer = 0;
      this.flush();
    }, wait);
  }

  redrawAll() {
    for (const id of this.store.ids()) this.store.dirty.add(id);
    for (const id of this.layers.keys()) this.store.dirty.add(id);
    this.schedule();
  }

  // flush applies the pending changes to the map.
  flush() {
    if (!this.map) return;
    this.lastFlush = performance.now();
    this.now = Date.now();
    this.store.expire(this.now);
    const dirty = this.store.take();

    const count = this.store.ids().length;
    const kind = count > CANVAS_ABOVE ? "canvas" : "svg";
    const previous = kind !== this.rendererKind ? this.renderer : null;
    if (kind !== this.rendererKind) {
      this.rendererKind = kind;
      this.renderer = kind === "canvas" ? this.L.canvas({ padding: 0.3 }) : this.L.svg({ padding: 0.3 });
      for (const id of this.layers.keys()) dirty.add(id);
      for (const id of this.store.ids()) dirty.add(id);
    }
    /** @type {Set<string>} the squares to redraw */
    const squares = new Set();
    for (const id of dirty) {
      if (id.startsWith("square:")) squares.add(id);
      else if (id.startsWith("locator:")) this.regroup(id, squares);
      else this.draw(id);
    }
    for (const id of squares) this.drawSquare(id);
    if (previous) this.map.removeLayer(previous);
    // Drop the next feature at its expiry, not at the next fade tick.
    clearTimeout(this.expiry);
    const next = this.store.nextExpiry();
    if (next !== null) this.expiry = setTimeout(() => this.schedule(), Math.min(FADE_TICK_MS, Math.max(0, next - Date.now())) + 50);
    this.updateCounts();
    this.focusPending();
    if (dirty.has(this.selected) || squares.has(this.selected)) this.renderDetail();
    this.renderList(false);
  }

  /**
   * regroup files a locator entity under its square (MAP-009) and adds
   * the squares it left and joined to squares.
   * @param {string} id @param {Set<string>} squares
   */
  regroup(id, squares) {
    const g = this.store.entity(id)?.feature?.geometry;
    const loc = typeof g?.locator === "string" && isLocator(g.locator) && g.locator.length >= 4 ? g.locator.slice(0, 4).toUpperCase() : "";
    const old = this.squareOf.get(id);
    if (old && old !== loc) {
      const members = this.squares.get(old);
      members?.delete(id);
      if (members?.size === 0) this.squares.delete(old);
    }
    if (old) squares.add(`square:${old}`);
    if (!loc) {
      this.squareOf.delete(id);
      return;
    }
    this.squareOf.set(id, loc);
    let members = this.squares.get(loc);
    if (!members) {
      members = new Set();
      this.squares.set(loc, members);
    }
    members.add(id);
    squares.add(`square:${loc}`);
  }

  /**
   * stationsIn returns the stations of a square that pass a test.
   * @param {string} square @param {(e: Entity) => boolean} test
   */
  stationsIn(square, test) {
    /** @type {Entity[]} */
    const out = [];
    for (const id of this.squares.get(square) ?? []) {
      const e = this.store.entity(id);
      if (e && test(e)) out.push(e);
    }
    return out;
  }

  /**
   * entityOf returns the entity of an id, a square included, or null.
   * @param {string} id
   * @returns {Entity | null}
   */
  entityOf(id) {
    if (!id.startsWith("square:")) return this.store.entity(id);
    const square = id.slice(7);
    const stations = this.stationsIn(square, (e) => this.byNode(e));
    const b = locatorBounds(square);
    if (stations.length === 0 || !b) return null;
    return {
      id, kind: "locator", subject: square, feature: null, receiver: null, devices: [],
      updated_at: Math.max(...stations.map((e) => e.updated_at)), at: [(b[0][0] + b[1][0]) / 2, (b[0][1] + b[1][1]) / 2],
    };
  }

  /**
   * drawSquare draws a locator square in the color of its newest shown
   * station, fading with it (MAP-009).
   * @param {string} id square:<locator>
   */
  drawSquare(id) {
    const stations = this.stationsIn(id.slice(7), (e) => this.shown(e));
    const old = this.layers.get(id);
    const b = locatorBounds(id.slice(7));
    if (stations.length === 0 || !b) {
      old?.group.remove();
      this.layers.delete(id);
      return;
    }
    const newest = stations.reduce((a, x) => (x.updated_at > a.updated_at ? x : a));
    const selected = id === this.selected || stations.some((e) => e.id === this.selected);
    const op = opacity(newest, this.now);
    const sig = [newest.feature?.key, newest.updated_at, stations.length, this.rendererKind, selected, this.colorVersion].join("|");
    if (old && old.sig === sig) {
      old.style(op);
      return;
    }
    old?.group.remove();
    const color = this.colourOf(newest);
    const rect = this.L.rectangle(b, {
      renderer: this.renderer, color: selected ? this.colors.focus : color, weight: selected ? 3 : 1, fillColor: color,
    });
    const group = this.L.featureGroup([rect]);
    group.on("click", () => this.select(id));
    const layer = { group, sig, style: (/** @type {number} */ o) => rect.setStyle({ opacity: o, fillOpacity: 0.3 * o }) };
    layer.style(op);
    // Under the stations and lines it holds.
    group.addTo(this.map).bringToBack();
    this.layers.set(id, layer);
  }

  /**
   * draw creates, updates or removes the layers of an entity.
   * @param {string} id
   */
  draw(id) {
    const e = this.store.entity(id);
    const old = this.layers.get(id);
    if (!e || !this.shown(e)) {
      if (old) {
        old.group.remove();
        this.layers.delete(id);
      }
      return;
    }
    const op = opacity(e, this.now);
    // The heading tick is sized in pixels: redrawn at every zoom.
    const zoom = finite(e.feature?.details.course) ? this.map.getZoom() : "";
    // Receivers have no report time: their place and name are compared.
    const sig = [e.feature?.key, e.updated_at, e.at, e.subject, this.rendererKind, zoom, id === this.selected, this.colorVersion].join("|");
    if (old && old.sig === sig) {
      old.style(op);
      return;
    }
    old?.group.remove();
    const layer = this.build(e);
    if (!layer) {
      this.layers.delete(id);
      return;
    }
    layer.sig = sig;
    layer.style(op);
    layer.group.addTo(this.map);
    this.layers.set(id, layer);
  }

  /**
   * build returns the Leaflet layers of an entity and how to restyle them.
   * @param {Entity} e
   */
  build(e) {
    const L = this.L;
    const color = this.colourOf(e);
    const selected = e.id === this.selected;
    const stroke = selected ? this.colors.focus : this.colors.surface;
    const renderer = this.renderer;
    const group = L.featureGroup();
    const open = () => this.select(e.id);
    group.on("click", open);
    /** @type {(op: number) => void} */
    let style = () => {};

    if (e.receiver) {
      const marker = L.marker(e.at, {
        icon: L.divIcon({ className: "map-receiver", iconSize: [24, 24], iconAnchor: [12, 14] }),
        title: `Receiver ${e.subject}`,
        keyboard: true,
        riseOnHover: true,
      });
      group.addLayer(marker);
      marker.on("add", () => {
        const icon = marker.getElement();
        icon?.setAttribute("aria-label", `Receiver ${e.subject}`);
        if (selected) icon?.setAttribute("data-selected", "");
      });
      style = (op) => marker.setOpacity(op);
      return { group, style };
    }

    const f = /** @type {import("./store.js").Feature} */ (e.feature);
    const g = f.geometry;
    if (g.type === "line") {
      if (![g.from?.lat, g.from?.lon, g.to?.lat, g.to?.lon].every(finite)) return null;
      const line = L.polyline(greatCircle([g.from.lat, g.from.lon], [g.to.lat, g.to.lon]), {
        renderer, color, weight: selected ? 4 : 2, dashArray: "6 6",
      });
      group.addLayer(line);
      style = (op) => line.setStyle({ opacity: op });
    } else {
      const at = /** @type {[number, number]} */ (e.at);
      /** @type {any[]} */
      const lines = [];
      if (Array.isArray(g.track) && g.track.length > 0) {
        const points = g.track.filter((/** @type {any} */ p) => finite(p?.lat) && finite(p?.lon)).map((/** @type {any} */ p) => [p.lat, p.lon]);
        lines.push(L.polyline([...points, at], { renderer, color, weight: 2 }));
      }
      const course = f.details.course;
      if (finite(course)) {
        const zoom = this.map.getZoom();
        const p = this.map.project(at, zoom);
        const rad = (course * Math.PI) / 180;
        const end = this.map.unproject([p.x + Math.sin(rad) * HEADING_PX, p.y - Math.cos(rad) * HEADING_PX], zoom);
        lines.push(L.polyline([at, end], { renderer, color, weight: 3 }));
      }
      const dot = L.circleMarker(at, {
        renderer, radius: selected ? 9 : 7, color: stroke, weight: 2, fillColor: color,
      });
      for (const x of lines) group.addLayer(x);
      group.addLayer(dot);
      style = (op) => {
        for (const x of lines) x.setStyle({ opacity: 0.7 * op });
        dot.setStyle({ opacity: op, fillOpacity: op });
      };
    }
    return { group, style };
  }

  // updateCounts sets the legend counts: entities per type, and squares
  // and calls per band or mode (whatever is isolated).
  updateCounts() {
    /** @type {Map<string, number>} */
    const counts = new Map();
    /** @type {Map<string, {key: import("./colours.js").Key, count: number}>} */
    const keys = new Map();
    for (const id of this.store.ids()) {
      const e = this.store.entity(id);
      if (!e || !this.byNode(e)) continue;
      const t = typeOf(e.kind).id;
      counts.set(t, (counts.get(t) ?? 0) + 1);
      const k = this.visible?.get(t) !== false ? this.keyOf(e) : null;
      if (k) {
        const x = keys.get(k.id) ?? { key: k, count: 0 };
        x.count++;
        keys.set(k.id, x);
      }
    }
    const entries = [...keys.values()].sort((a, b) => a.key.rank - b.key.rank || a.key.label.localeCompare(b.key.label));
    if (this.isolated && this.loaded && !keys.has(this.isolated)) {
      // What was isolated is gone: show everything again.
      this.isolate("");
    }
    this.keys?.render(this.colourMode, entries, this.isolated);
    for (const t of TYPES) {
      const item = this.legend?.get(t.id);
      if (!item) continue;
      const n = counts.get(t.id) ?? 0;
      if (item.count.textContent !== String(n)) item.count.textContent = String(n);
      item.li.hidden = t.optional && n === 0;
    }
  }

  // focusPending opens the items the URL asked for once they arrive.
  focusPending() {
    if (!this.loaded || this.pending.length === 0) return;
    /** @type {{field: string, e: Entity} | null} */
    let chosen = null;
    for (const p of [...this.pending]) {
      const e = this.find(p.value, false, p.field);
      if (!e) continue;
      this.pending.splice(this.pending.indexOf(p), 1);
      // A station (callsign, ICAO, MMSI) wins over a locator square.
      if (!chosen || chosen.field === "locator") chosen = { field: p.field, e };
    }
    if (chosen) this.select(chosen.e.id, { centre: true, focus: false });
  }

  /**
   * select opens the detail panel of an entity.
   * @param {string} id
   * @param {{centre?: boolean, focus?: boolean, opener?: HTMLElement}} [opts]
   */
  select(id, { centre = false, focus = true, opener } = {}) {
    const previous = this.selected;
    this.selected = id;
    this.returnFocus = opener ?? (document.activeElement instanceof HTMLElement ? document.activeElement : null);
    if (previous && previous !== id) this.store.dirty.add(previous);
    this.store.dirty.add(id);
    const e = this.entityOf(id);
    const panel = /** @type {HTMLElement} */ (this.querySelector("#map-detail"));
    const wasHidden = panel.hidden;
    panel.hidden = false;
    this.renderDetail(true);
    if (wasHidden) this.map?.invalidateSize();
    if (centre && e?.at && this.map) {
      const g = e.feature?.geometry;
      const b = id.startsWith("square:") ? locatorBounds(e.subject) : g?.type === "locator" ? locatorBounds(g.locator) : null;
      if (b) this.map.fitBounds(b, { maxZoom: FOCUS_ZOOM });
      else this.map.setView(e.at, Math.max(this.map.getZoom(), FOCUS_ZOOM));
    }
    if (focus) /** @type {HTMLElement | null} */ (this.querySelector("[data-map-detail-title]"))?.focus();
    this.schedule();
  }

  closeDetail() {
    const panel = /** @type {HTMLElement} */ (this.querySelector("#map-detail"));
    panel.hidden = true;
    if (this.selected) this.store.dirty.add(this.selected);
    this.selected = "";
    this.map?.invalidateSize();
    this.schedule();
    const back = this.returnFocus;
    this.returnFocus = null;
    if (back?.isConnected && back !== document.body) back.focus();
    else /** @type {HTMLElement | null} */ (this.querySelector("[data-map-canvas]"))?.focus();
  }

  /**
   * renderDetail (re)fills the detail panel of the selected entity; while
   * the focus is in it, the refresh waits.
   * @param {boolean} [force]
   */
  renderDetail(force = false) {
    if (!this.selected) return;
    const panel = /** @type {HTMLElement} */ (this.querySelector("#map-detail"));
    if (!force && panel.contains(document.activeElement) && document.activeElement !== panel.querySelector("[data-map-detail-title]")) {
      this.detailStale = true;
      return;
    }
    this.detailStale = false;
    const title = /** @type {HTMLElement} */ (panel.querySelector("[data-map-detail-title]"));
    const body = /** @type {HTMLElement} */ (panel.querySelector("[data-map-detail-body]"));
    const e = this.entityOf(this.selected);
    if (!e) {
      body.replaceChildren(el("p", { class: "mt-2 text-sm" }, "This feature is no longer on the map: it expired or was removed."));
      return;
    }
    if (e.id.startsWith("square:")) {
      title.textContent = `Square ${e.subject}`;
      const stations = this.stationsIn(e.subject, (x) => this.byNode(x));
      body.replaceChildren(renderSquare(stations, this.context(), (x, opener) => this.select(x.id, { opener })));
    } else {
      title.textContent = e.subject || "(unnamed)";
      body.replaceChildren(renderDetail(e, this.context()));
    }
    if (e.at && !this.querySelector("[data-map-stage]")?.hidden) {
      const b = el("button", { type: "button", class: "button mt-3 max-md:min-h-11" }, "Centre the map on it");
      b.addEventListener("click", () => this.select(e.id, { centre: true, focus: false }));
      body.append(b);
    }
    // Links stay in the app shell: boosted like the server-rendered ones.
    window.htmx?.process?.(body);
  }

  /** @returns {import("./detail.js").DetailContext} */
  context() {
    const links = this.config?.links && typeof this.config.links === "object" ? this.config.links : {};
    return { devices: this.devices, receivers: [...this.store.receivers.values()], bands: this.bands, links, now: Date.now() };
  }

  /**
   * renderList refreshes the list view while shown, at most every 2 s and
   * never under the keyboard focus.
   * @param {boolean} now
   */
  renderList(now) {
    const root = this.querySelector("[data-map-list]");
    if (!this.list || !root || /** @type {HTMLElement} */ (root).hidden) return;
    if (!now && (performance.now() - this.lastList < LIST_EVERY_MS || this.list.focused())) return;
    this.lastList = performance.now();
    /** @type {Entity[]} */
    const rows = [];
    for (const id of this.store.ids()) {
      const e = this.store.entity(id);
      if (e && this.shown(e)) rows.push(e);
    }
    this.list.render(rows, this.context());
  }
}

if (!customElements.get("msdr-map")) {
  customElements.define("msdr-map", MapIsland);
}
