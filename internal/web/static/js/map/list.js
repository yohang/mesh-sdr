// List view of the map (FEATURE_SPEC §10.10 "Accessibility"): the shown
// features as a table, sortable by its column header buttons (aria-sort)
// and keyboard-navigable; each name is a button that opens the same
// detail panel as the map. Text is set with textContent only.

import { el } from "../receiver/dom.js";
import { deviceName, placeText } from "./detail.js";
import { utcText } from "./geo.js";
import { typeOf } from "./kinds.js";

/** @typedef {import("./store.js").Entity} Entity */

/**
 * @typedef {object} Column
 * @property {string} id
 * @property {string} label
 * @property {(e: Entity, ctx: import("./detail.js").DetailContext) => string | number} key sort key
 */

/** @type {Column[]} */
const COLUMNS = [
  { id: "name", label: "Name", key: (e) => e.subject.toLowerCase() },
  { id: "type", label: "Type", key: (e) => typeOf(e.kind).one },
  { id: "place", label: "Locator or position", key: (e) => placeText(e) },
  { id: "heard", label: "Last heard (UTC)", key: (e) => e.updated_at },
  { id: "by", label: "Heard by", key: (e, ctx) => e.devices.map((d) => deviceName(ctx, d)).join(", ") },
];

export class ListView {
  /**
   * @param {HTMLElement} root the list container
   * @param {(e: Entity, opener: HTMLElement) => void} open opens the detail
   */
  constructor(root, open) {
    this.root = root;
    this.open = open;
    this.sort = "heard";
    this.desc = true;
    /** @type {Entity[]} */
    this.rows = [];
    /** @type {import("./detail.js").DetailContext | null} */
    this.ctx = null;
    this.caption = el("p", { class: "mb-2 text-sm text-fg-muted" });
    this.root.append(this.caption);
  }

  /** @returns {boolean} whether the focus is in the table (no re-render then) */
  focused() {
    return this.root.contains(document.activeElement) && document.activeElement !== this.root;
  }

  /**
   * render shows rows, sorted.
   * @param {Entity[]} rows @param {import("./detail.js").DetailContext} ctx
   */
  render(rows, ctx) {
    this.rows = rows;
    this.ctx = ctx;
    const col = COLUMNS.find((c) => c.id === this.sort) ?? COLUMNS[0];
    const sorted = [...rows].sort((a, b) => {
      const x = col.key(a, ctx);
      const y = col.key(b, ctx);
      const c = x < y ? -1 : x > y ? 1 : 0;
      return this.desc ? -c : c;
    });

    this.caption.textContent =
      rows.length === 0 ? "No feature is shown." : `${rows.length} feature${rows.length === 1 ? "" : "s"} shown, as on the map.`;
    this.root.querySelector("[data-map-table]")?.remove();
    if (rows.length === 0) return;

    const scroll = el("div", { class: "overflow-x-auto", "data-map-table": "" });
    const table = el("table", { class: "w-full text-left text-sm" });
    table.append(el("caption", { class: "sr-only" }, "Features on the map; the column buttons sort it"));
    const head = el("tr", { class: "border-b border-border" });
    for (const c of COLUMNS) {
      const th = el("th", { scope: "col", class: "py-1 pr-4" });
      if (c.id === this.sort) th.setAttribute("aria-sort", this.desc ? "descending" : "ascending");
      const b = el("button", { type: "button", class: "font-semibold underline-offset-2 hover:underline", "data-sort": c.id }, c.label);
      if (c.id === this.sort) b.append(el("span", { "aria-hidden": "true" }, this.desc ? " ▼" : " ▲"));
      b.addEventListener("click", () => this.sortBy(c.id));
      th.append(b);
      head.append(th);
    }
    const thead = el("thead");
    thead.append(head);
    table.append(thead);
    const body = el("tbody");
    for (const e of sorted) {
      const tr = el("tr", { class: "border-b border-border align-top" });
      const name = el("td", { class: "py-1 pr-4" });
      const b = el("button", { type: "button", class: "text-left font-medium text-accent underline" }, e.subject || "(unnamed)");
      b.addEventListener("click", () => this.open(e, b));
      name.append(b);
      tr.append(name);
      tr.append(el("td", { class: "py-1 pr-4" }, typeOf(e.kind).one));
      tr.append(el("td", { class: "py-1 pr-4" }, placeText(e)));
      tr.append(el("td", { class: "whitespace-nowrap py-1 pr-4 font-mono tabular-nums" }, e.updated_at ? utcText(e.updated_at).slice(0, 19) : "—"));
      tr.append(el("td", { class: "py-1 pr-4" }, e.devices.map((d) => deviceName(ctx, d)).join(", ")));
      body.append(tr);
    }
    table.append(body);
    scroll.append(table);
    this.root.append(scroll);
  }

  /**
   * sortBy sorts by a column (again: the other way round) and keeps the
   * focus on its header button.
   * @param {string} id
   */
  sortBy(id) {
    if (this.sort === id) this.desc = !this.desc;
    else {
      this.sort = id;
      this.desc = id === "heard";
    }
    if (this.ctx) this.render(this.rows, this.ctx);
    /** @type {HTMLElement | null} */ (this.root.querySelector(`[data-sort="${id}"]`))?.focus();
  }
}
