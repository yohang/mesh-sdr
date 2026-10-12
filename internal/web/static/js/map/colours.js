// Colour mode of the map (MAP-009, MAP-010): locator squares and call lines
// are coloured by band (the amateur bands of the band plan, GET
// /api/v1/bandplan; by mode while it is not loaded) or by mode, or all in
// their type's color (Off). The legend lists the bands or modes on the map,
// each with its swatch, its name and its count of stations (locator
// reports, not squares) and calls; selecting one isolates it (aria-pressed, a check mark
// and a sentence say which), selecting it again shows them all. The colors
// are the eight --msdr-color-series-* tokens: the name in the legend, the
// detail panel and the list view is the cue, never the color alone.

import { el } from "../receiver/dom.js";
import { finite } from "./geo.js";

export const SERIES = 8;

// Modes in a fixed order, so each keeps its color as others come and go.
const MODES = ["ft8", "ft4", "wspr", "js8", "jt65", "jt9", "fst4", "q65", "msk144", "fst4w", "aprs"];

/**
 * @typedef {object} Band an amateur band of the band plan
 * @property {string} name
 * @property {number} low
 * @property {number} high
 */

/**
 * @typedef {object} Key what a feature is coloured by
 * @property {string} id
 * @property {string} label
 * @property {number} series 0 to SERIES - 1, or -1 (unknown: the type color)
 * @property {number} rank the legend order (band plan order, then modes)
 */

/**
 * colourKey returns the band or mode of a feature's details; null when the
 * colour mode is off.
 * @param {string} mode "band", "mode" or "off"
 * @param {Record<string, any>} details @param {Band[]} bands
 * @returns {Key | null}
 */
export function colourKey(mode, details, bands) {
  if (mode === "band") {
    const hz = details.freq_hz;
    const i = finite(hz) ? bands.findIndex((b) => hz >= b.low && hz <= b.high) : -1;
    return i < 0
      ? { id: "band:", label: "Other band", series: -1, rank: Infinity }
      : { id: `band:${bands[i].name}`, label: bands[i].name, series: i % SERIES, rank: i };
  }
  if (mode === "mode") {
    const m = typeof details.mode === "string" ? details.mode.toLowerCase() : "";
    if (!m) return { id: "mode:", label: "Unknown mode", series: -1, rank: Infinity };
    const known = MODES.indexOf(m);
    const i = known >= 0 ? known : [...m].reduce((sum, c) => sum + c.charCodeAt(0), 0);
    return { id: `mode:${m}`, label: m.toUpperCase(), series: i % SERIES, rank: known >= 0 ? known : MODES.length };
  }
  return null;
}

/**
 * hamBands keeps the amateur bands of a GET /api/v1/bandplan answer.
 * @param {any} plan
 * @returns {Band[]}
 */
export function hamBands(plan) {
  const bands = Array.isArray(plan?.bands) ? plan.bands : [];
  return bands.filter(
    (/** @type {any} */ b) => typeof b?.name === "string" && finite(b.low) && finite(b.high) && Array.isArray(b.tags) && b.tags.includes("hamradio"),
  );
}

/**
 * swatch is the legend symbol of a key.
 * @param {Key} k @param {string} fallback the symbol class of the type
 */
function swatch(k, fallback) {
  return el("span", { class: k.series < 0 ? `map-sym ${fallback}` : `map-sym map-swatch map-series-${k.series + 1}` });
}

/**
 * ColourLegend is the band or mode legend, updated in place so the focus
 * stays on its buttons.
 */
export class ColourLegend {
  /**
   * @param {HTMLElement} root @param {(id: string) => void} isolate
   */
  constructor(root, isolate) {
    this.root = root;
    this.heading = el("h3", { class: "mt-3 text-sm font-medium" });
    this.list = el("ul", { class: "mt-1 flex flex-col gap-1" });
    this.note = el("p", { class: "mt-1 text-sm" });
    this.root.append(this.heading, this.note, this.list);
    /** @type {Map<string, {li: HTMLElement, button: HTMLElement, count: HTMLElement, check: HTMLElement}>} */
    this.items = new Map();
    this.isolate = isolate;
  }

  /**
   * render shows the keys on the map with their counts.
   * @param {string} mode @param {{key: Key, count: number}[]} entries in band or mode order
   * @param {string} isolated the isolated key id ("" for none)
   */
  render(mode, entries, isolated) {
    this.root.hidden = mode === "off";
    this.heading.textContent = mode === "band" ? "Bands" : "Modes";
    const label = entries.find((x) => x.key.id === isolated)?.key.label;
    this.note.textContent = label
      ? `Only ${label} is shown; select it again to show every ${mode}.`
      : entries.length > 0
        ? `Select a ${mode} to show it alone.`
        : `No locator or call is on the map.`;
    const ids = new Set(entries.map((x) => x.key.id));
    for (const [id, item] of this.items) {
      if (!ids.has(id)) {
        item.li.remove();
        this.items.delete(id);
      }
    }
    entries.forEach(({ key, count }, i) => {
      let item = this.items.get(key.id);
      if (!item) {
        const li = el("li");
        const button = el("button", { type: "button", class: "map-key flex w-full items-center gap-2 rounded px-1 text-left text-sm max-md:min-h-11" });
        const check = el("span", { class: "w-4", "aria-hidden": "true" });
        const count = el("span", { class: "ml-auto tabular-nums text-fg-muted" });
        button.append(check, swatch(key, "map-sym-locator"), el("span", {}, key.label), count);
        button.addEventListener("click", () => this.isolate(key.id));
        li.append(button);
        item = { li, button, count, check };
        this.items.set(key.id, item);
      }
      const on = key.id === isolated;
      item.button.setAttribute("aria-pressed", String(on));
      item.check.textContent = on ? "✓" : "";
      if (item.count.textContent !== String(count)) item.count.textContent = String(count);
      // Moved only when out of place: moving the focused button blurs it.
      if (this.list.children[i] !== item.li) this.list.insertBefore(item.li, this.list.children[i] ?? null);
    });
  }
}
