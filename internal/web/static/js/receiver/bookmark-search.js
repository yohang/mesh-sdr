// Bookmark search (BMK-004): the search field of the Bookmarks tab (key Y
// focuses it) finds pack and hub bookmarks by name and by frequency across
// every device the visitor may listen to: the devices of the grid state
// (GET /api/v1/features lists only those), each asked for its bookmarks
// with GET /api/v1/bookmarks?device_id= (the hub enforces the listen policy
// and the bookmark scopes). The lists are fetched when the search is first
// used and dropped on bookmark.changed and on a device list change.
//
// A result deep-links to the device and frequency in the RX-028 form,
// /receiver/{nodeId}/{deviceId}?f=<Hz>&m=<mode> (ADR 0027; BMK-004's
// /receiver/{deviceId}?freq=&mod= is a spec inconsistency, #541). A digital
// bookmark links its analog underlying mode as m and keeps its modulation
// as m2. A bookmark shown on several devices gets one link per device.

import { getJSON } from "../csrf.js";
import { formatMHz } from "./dom.js";
import { originText, tuneMode } from "./marks.js";

const MAX_RESULTS = 50;
const INPUT_DELAY_MS = 200;

/**
 * @typedef {import("./grid.js").Device} Device
 * @typedef {{id: string, name: string, frequency: number, modulation: string, underlying?: string, origin: string}} APIBookmark
 */

/**
 * matches reports whether a bookmark matches a query: its name contains
 * the words of the query, or its frequency (in MHz, kHz or Hz) starts with
 * the number typed.
 * @param {APIBookmark} b @param {string} q lower case, trimmed
 */
export function matches(b, q) {
  if (!q) return false;
  if (b.name.toLowerCase().includes(q)) return true;
  const n = q.replace(",", ".").replace(/\s*(mhz|khz|hz)$/, "");
  if (!/^\d+(\.\d*)?$/.test(n)) return false;
  const trim = (/** @type {number} */ v) => String(Number(v.toFixed(6)));
  return [trim(b.frequency / 1e6), trim(b.frequency / 1e3), String(b.frequency)].some((s) => s.startsWith(n));
}

/**
 * linkOf is the deep link of a bookmark on a device (RX-028).
 * @param {APIBookmark} b @param {Device} d
 */
export function linkOf(b, d) {
  const q = new URLSearchParams({ f: String(Math.round(b.frequency)) });
  const mode = tuneMode(b.modulation, b.underlying ?? "", d.modes ?? []);
  if (mode) q.set("m", mode);
  if (mode !== b.modulation) q.set("m2", b.modulation);
  return `/receiver/${encodeURIComponent(d.node_id)}/${encodeURIComponent(d.id)}?${q}`;
}

export class BookmarkSearch {
  /**
   * @param {ReturnType<typeof import("./grid.js").getGrid>} grid
   */
  constructor(grid) {
    this.grid = grid;
    /** @type {Map<string, APIBookmark[]> | null} per device id */
    this.lists = null;
    /** @type {Promise<void> | null} */
    this.loading = null;
    this.devicesKey = "";
    this.timer = 0;

    this.input = /** @type {HTMLInputElement} */ (document.createElement("input"));
    Object.assign(this.input, { type: "search", id: "rx-bmk-search", autocomplete: "off", spellcheck: false });
    this.input.className = "w-full min-w-0 rounded border px-2 py-1";
    this.input.setAttribute("aria-describedby", "rx-bmk-search-status");
    this.input.setAttribute("aria-controls", "rx-bmk-results");
    const label = document.createElement("label");
    label.htmlFor = "rx-bmk-search";
    label.className = "font-semibold";
    label.textContent = "Search bookmarks";
    const hint = document.createElement("p");
    hint.className = "text-sm text-fg-muted";
    hint.textContent = "By name or frequency (MHz), on every device you can listen to.";
    this.status = document.createElement("p");
    this.status.id = "rx-bmk-search-status";
    this.status.className = "text-sm";
    this.status.setAttribute("role", "status");
    this.results = document.createElement("ul");
    this.results.id = "rx-bmk-results";
    this.results.className = "flex max-h-64 flex-col gap-1 overflow-y-auto text-sm";
    this.el = document.createElement("div");
    this.el.className = "flex flex-col gap-1";
    this.el.setAttribute("role", "search");
    this.el.append(label, hint, this.input, this.status, this.results);

    this.input.addEventListener("focus", () => this.load());
    this.input.addEventListener("input", () => {
      clearTimeout(this.timer);
      this.timer = setTimeout(() => this.run(), INPUT_DELAY_MS);
    });
    this.input.addEventListener("keydown", (ev) => {
      if (ev.key === "Escape" && this.input.value) {
        ev.preventDefault();
        this.input.value = "";
        this.run();
      }
    });
    this.onChanged = () => this.invalidate();
    document.body.addEventListener("msdr:bookmark.changed", this.onChanged);
    document.body.addEventListener("msdr:resync", this.onChanged);
    this.onGrid = () => {
      const key = this.grid.devices.map((d) => `${d.node_id}/${d.id}`).join();
      if (key !== this.devicesKey) this.invalidate();
    };
    this.grid.addEventListener("change", this.onGrid);
  }

  destroy() {
    clearTimeout(this.timer);
    document.body.removeEventListener("msdr:bookmark.changed", this.onChanged);
    document.body.removeEventListener("msdr:resync", this.onChanged);
    this.grid.removeEventListener("change", this.onGrid);
  }

  focus() {
    this.input.focus();
    this.input.select();
  }

  invalidate() {
    this.lists = null;
    if (this.input.value.trim()) {
      clearTimeout(this.timer);
      this.timer = setTimeout(() => this.run(), INPUT_DELAY_MS);
    }
  }

  // load fetches the bookmarks of every listed device, once.
  load() {
    if (this.lists) return Promise.resolve();
    this.loading ??= (async () => {
      const devices = [...this.grid.devices];
      this.devicesKey = devices.map((d) => `${d.node_id}/${d.id}`).join();
      /** @type {Map<string, APIBookmark[]>} */
      const lists = new Map();
      await Promise.all(
        devices.map(async (d) => {
          try {
            const body = await getJSON(`/api/v1/bookmarks?device_id=${encodeURIComponent(d.id)}`);
            lists.set(d.id, Array.isArray(body?.bookmarks) ? body.bookmarks : []);
          } catch {
            // That device's bookmarks are left out.
          }
        }),
      );
      this.lists = lists;
      this.loading = null;
    })();
    return this.loading;
  }

  async run() {
    const q = this.input.value.trim().toLowerCase();
    if (!q) {
      this.results.replaceChildren();
      this.status.textContent = "";
      return;
    }
    if (!this.lists) {
      this.status.textContent = "Searching…";
      await this.load();
      if (q !== this.input.value.trim().toLowerCase()) return;
    }
    /** @type {Map<string, {b: APIBookmark, devices: Device[]}>} */
    const found = new Map();
    for (const d of this.grid.devices) {
      for (const b of this.lists?.get(d.id) ?? []) {
        if (!matches(b, q)) continue;
        const hit = found.get(b.id) ?? { b, devices: [] };
        hit.devices.push(d);
        found.set(b.id, hit);
      }
    }
    const all = [...found.values()].sort((x, y) => x.b.frequency - y.b.frequency || x.b.name.localeCompare(y.b.name));
    const shown = all.slice(0, MAX_RESULTS);
    const several = this.grid.devices.length > 1;
    this.results.replaceChildren(
      ...shown.map(({ b, devices }) => {
        const li = document.createElement("li");
        li.className = "flex flex-col";
        const origin = b.origin === "builtin" ? "pack" : "hub";
        for (const d of devices) {
          const a = document.createElement("a");
          a.href = linkOf(b, d);
          const shape = document.createElement("span");
          shape.className = `bmk-shape bmk-shape-${origin}`;
          a.append(shape, `${b.name} · ${formatMHz(b.frequency, { trim: true })} · ${(b.modulation || "").toUpperCase()}${several ? ` · ${d.name}` : ""}`);
          a.setAttribute("aria-label", `${b.name}, ${formatMHz(b.frequency, { trim: true })}, ${b.modulation.toUpperCase()}, ${originText(origin)}${several ? `, on ${d.name}` : ""}`);
          li.append(a);
        }
        return li;
      }),
    );
    const n = all.length;
    this.status.textContent =
      n === 0 ? "No bookmark found." : n > MAX_RESULTS ? `${n} bookmarks found, the first ${MAX_RESULTS} shown.` : `${n} bookmark${n === 1 ? "" : "s"} found.`;
  }
}
