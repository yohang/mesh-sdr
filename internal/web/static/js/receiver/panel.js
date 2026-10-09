// The receiver's side panel (UI-019) and, below 768 px, its bottom sheet
// (UI-020, UI-007). One element, two presentations:
//   - docked: a column on the right of the waterfall (from 768 px), opened
//     and closed by the island (closed by default below 1200 px);
//   - sheet: fixed at the bottom of the screen, over the bottom tab bar,
//     with three snap points: peek (the head and the tab bar), half (about
//     half the viewport) and full (up to the top bar, which keeps the
//     audio dock visible). Its head always shows a summary the island
//     fills (frequency and mode), so the sheet never hides them entirely.
//     The handle is dragged with pointer events; the Shrink and Enlarge
//     buttons do the same from the keyboard; a tab of a peeking sheet
//     opens it to half; Esc shrinks it back to peek. It is not a modal:
//     the page behind stays live and reachable, and nothing traps the
//     focus. With reduced motion, snaps are instant.
//
// Tabs (addTab) follow the WAI-ARIA tab pattern (roving tab index, arrow
// keys, Home and End). Every module adds its own tab: Info today, others
// later; the panel does not know them. A view (showView) replaces the
// active tab until it is closed (its Close button or Esc): the keyboard
// shortcuts help (UI-015) and, in the sheet, the control bar groups that
// do not fit the compact bar (More).

import { BUTTON, el } from "./dom.js";

/** @typedef {"peek" | "half" | "full"} Snap */

const SNAPS = /** @type {Snap[]} */ (["peek", "half", "full"]);
// A pointer that moved less than this (CSS px) on the handle is a tap.
const TAP_PX = 6;
const SHEET_BUTTON =
  "inline-flex min-h-11 min-w-11 items-center justify-center rounded border border-border text-lg leading-none disabled:opacity-50";

/**
 * @typedef {object} Tab
 * @property {string} id
 * @property {HTMLButtonElement} button
 * @property {HTMLElement} panel
 */

/**
 * @typedef {object} View
 * @property {string} id
 * @property {string} title
 * @property {HTMLElement} content
 * @property {(() => void) | undefined} onClose
 */

export class SidePanel {
  /**
   * @param {object} o
   * @param {string} o.id element id (the toggles' aria-controls)
   * @param {() => void} o.onChange called when the tab, the view or the
   *   snap point changed
   */
  constructor({ id, onChange }) {
    this.onChange = onChange;
    this.sheet = false;
    /** @type {Snap} */
    this.snapPoint = "peek";
    /** @type {Tab[]} */
    this.tabs = [];
    this.active = "";
    /** @type {View | null} */
    this.view = null;
    /** @type {Element | null} focus to restore when the view closes */
    this.returnFocus = null;

    this.el = el("aside", {
      id,
      "aria-label": "Side panel",
      class:
        "flex min-w-0 flex-col overflow-hidden rounded border border-border bg-surface md:self-start " +
        "max-md:fixed max-md:inset-x-0 max-md:bottom-16 max-md:z-20 max-md:rounded-b-none max-md:border-x-0 max-md:border-b-0 " +
        "max-md:shadow-[0_-8px_24px_rgb(0_0_0/0.25)] motion-safe:max-md:transition-[height] data-dragging:transition-none",
    });

    // Sheet head: drag handle, summary, Shrink and Enlarge.
    this.summary = el("p", { class: "min-w-0 flex-1 truncate font-mono text-sm tabular-nums" });
    this.shrinkBtn = /** @type {HTMLButtonElement} */ (el("button", { type: "button", class: SHEET_BUTTON, "aria-label": "Shrink the panel" }, "▾"));
    this.growBtn = /** @type {HTMLButtonElement} */ (el("button", { type: "button", class: SHEET_BUTTON, "aria-label": "Enlarge the panel" }, "▴"));
    this.shrinkBtn.addEventListener("click", () => this.step(-1));
    this.growBtn.addEventListener("click", () => this.step(1));
    const grip = el("span", { class: "mx-auto block h-1.5 w-12 rounded-full bg-border", "aria-hidden": "true" });
    const row = el("div", { class: "flex items-center gap-2 px-3 pb-1" });
    row.append(this.summary, this.shrinkBtn, this.growBtn);
    this.head = el("div", { class: "flex shrink-0 cursor-grab touch-none select-none flex-col gap-1 pt-2 md:hidden" });
    this.head.append(grip, row);
    this.bindDrag();

    this.tablist = el("div", { role: "tablist", "aria-label": "Side panel", class: "flex shrink-0 flex-wrap gap-1 border-b border-border px-2 pt-2" });
    this.tablist.addEventListener("keydown", (ev) => this.onTabKey(ev));

    // View header: title and Close.
    this.viewTitle = el("h2", { class: "min-w-0 flex-1 text-lg font-semibold focus:outline-none", tabindex: "-1" });
    this.viewClose = el("button", { type: "button", class: BUTTON, "aria-keyshortcuts": "Escape" }, "Close");
    this.viewClose.addEventListener("click", () => this.closeView());
    this.viewHead = el("div", { class: "flex items-center gap-2 border-b border-border p-3" });
    this.viewHead.append(this.viewTitle, this.viewClose);
    this.viewHead.hidden = true;
    this.viewBody = el("div", { class: "flex flex-col gap-3" });

    this.body = el("div", { class: "flex min-h-0 flex-1 flex-col gap-3 overflow-y-auto p-3" });
    this.body.append(this.viewBody);
    this.el.append(this.head, this.tablist, this.viewHead, this.body);

    // Esc inside the panel: close the view, else shrink the sheet.
    this.el.addEventListener("keydown", (ev) => {
      if (ev.key !== "Escape" || ev.defaultPrevented) return;
      if (this.escape()) {
        ev.preventDefault();
        ev.stopPropagation();
      }
    });
  }

  /**
   * addTab adds a tab; its content goes into a tab panel. The first tab
   * is the active one.
   * @param {{id: string, label: string, content: HTMLElement}} t
   * @returns {HTMLElement} the tab panel
   */
  addTab({ id, label, content }) {
    const button = /** @type {HTMLButtonElement} */ (
      el(
        "button",
        {
          type: "button",
          role: "tab",
          id: `rx-tab-${id}`,
          "aria-controls": `rx-tabpanel-${id}`,
          "aria-selected": "false",
          tabindex: "-1",
          class: "rx-tab",
        },
        label,
      )
    );
    button.addEventListener("click", () => this.select(id, { open: true }));
    const panel = el("div", { role: "tabpanel", id: `rx-tabpanel-${id}`, "aria-labelledby": `rx-tab-${id}`, tabindex: "0", class: "flex min-w-0 flex-col gap-3" });
    panel.append(content);
    this.tablist.append(button);
    this.body.insertBefore(panel, this.viewBody);
    this.tabs.push({ id, button, panel });
    if (!this.active) this.active = id;
    this.render();
    return panel;
  }

  /**
   * select shows a tab, closing any view. open: a peeking sheet opens to
   * half.
   * @param {string} id @param {{open?: boolean, focus?: boolean}} [o]
   */
  select(id, { open = false, focus = false } = {}) {
    const t = this.tabs.find((x) => x.id === id);
    if (!t) return;
    this.active = id;
    if (this.view) this.closeView({ restore: false });
    if (open && this.sheet && this.snapPoint === "peek") this.snapPoint = "half";
    this.render();
    if (focus) t.button.focus();
    this.onChange();
  }

  /**
   * showView replaces the active tab with a view until closeView. The
   * focus moves to its title, and returns where it was on close.
   * @param {{id: string, title: string, content: HTMLElement, onClose?: () => void}} v
   */
  showView({ id, title, content, onClose }) {
    if (this.view && this.view.id !== id) this.closeView({ restore: false });
    if (!this.view) this.returnFocus = document.activeElement;
    this.view = { id, title, content, onClose };
    this.viewTitle.textContent = title;
    this.viewBody.replaceChildren(content);
    if (this.sheet && this.snapPoint === "peek") this.snapPoint = "half";
    this.render();
    // The island may unhide the panel first.
    this.onChange();
    this.viewTitle.focus({ preventScroll: true });
  }

  /**
   * closeView goes back to the active tab.
   * @param {{restore?: boolean}} [o] restore: focus where it was before
   */
  closeView({ restore = true } = {}) {
    const v = this.view;
    if (!v) return;
    this.view = null;
    this.viewBody.replaceChildren();
    v.onClose?.();
    this.render();
    const back = this.returnFocus;
    this.returnFocus = null;
    if (restore && back instanceof HTMLElement && back.isConnected && !back.closest("[hidden]")) back.focus();
    this.onChange();
  }

  /** @returns {boolean} whether Esc did something: closed the view or shrank the sheet */
  escape() {
    if (this.view) {
      this.closeView();
      return true;
    }
    if (this.sheet && this.snapPoint !== "peek") {
      this.snap("peek");
      return true;
    }
    return false;
  }

  /**
   * setSheet switches between the docked panel and the bottom sheet.
   * @param {boolean} sheet
   */
  setSheet(sheet) {
    if (sheet === this.sheet) return;
    this.sheet = sheet;
    this.render();
  }

  /** @param {Snap} s */
  snap(s) {
    if (this.snapPoint === s) return;
    this.snapPoint = s;
    this.render();
    this.onChange();
  }

  /** @param {number} dir +1 enlarge, −1 shrink */
  step(dir) {
    const i = SNAPS.indexOf(this.snapPoint) + dir;
    if (i < 0 || i >= SNAPS.length) return;
    const focused = document.activeElement;
    this.snap(SNAPS[i]);
    // A button that got disabled hands the focus to the other one.
    if (focused instanceof HTMLButtonElement && focused.disabled) (focused === this.growBtn ? this.shrinkBtn : this.growBtn).focus();
  }

  /** @returns {Record<Snap, number>} the snap heights (CSS px) */
  heights() {
    const head = this.head.offsetHeight + this.tablist.offsetHeight + 1;
    const bottom = parseFloat(getComputedStyle(this.el).bottom) || 0;
    const top = document.querySelector("body > header")?.getBoundingClientRect().bottom ?? 0;
    const room = Math.max(head, window.innerHeight - bottom - Math.max(0, top));
    return { peek: head, half: Math.max(head, Math.min(room, Math.round(window.innerHeight * 0.5))), full: room };
  }

  // render applies the tab, the view and the sheet height.
  render() {
    for (const t of this.tabs) {
      const on = t.id === this.active && !this.view;
      t.button.setAttribute("aria-selected", String(t.id === this.active && !this.view));
      t.button.tabIndex = t.id === this.active ? 0 : -1;
      t.panel.hidden = !on;
    }
    this.viewHead.hidden = !this.view;
    this.viewBody.hidden = !this.view;
    if (!this.sheet) {
      this.el.style.removeProperty("height");
      this.body.inert = false;
      document.body.classList.remove("rx-sheet");
      return;
    }
    const h = this.heights();
    this.el.style.height = `${h[this.snapPoint]}px`;
    document.body.classList.add("rx-sheet");
    document.body.style.setProperty("--rx-sheet-peek", `${h.peek}px`);
    // A peeking sheet shows its tabs only: what it hides is not reachable.
    this.body.inert = this.snapPoint === "peek";
    this.shrinkBtn.disabled = this.snapPoint === "peek";
    this.growBtn.disabled = this.snapPoint === "full";
    this.el.dataset.snap = this.snapPoint;
  }

  // detach undoes what the panel did outside itself.
  detach() {
    document.body.classList.remove("rx-sheet");
    document.body.style.removeProperty("--rx-sheet-peek");
  }

  /** @param {KeyboardEvent} ev */
  onTabKey(ev) {
    const i = this.tabs.findIndex((t) => t.button === document.activeElement);
    if (i < 0) return;
    const n = this.tabs.length;
    const next = { ArrowRight: (i + 1) % n, ArrowLeft: (i - 1 + n) % n, Home: 0, End: n - 1 }[ev.key];
    if (next === undefined) return;
    ev.preventDefault();
    this.select(this.tabs[next].id, { focus: true });
  }

  // bindDrag drags the sheet by its head; a tap toggles peek and half.
  bindDrag() {
    /** @type {{id: number, y: number, h: number, moved: boolean} | null} */
    let drag = null;
    this.head.addEventListener("pointerdown", (ev) => {
      if (!this.sheet || ev.button !== 0 || (ev.target instanceof Element && ev.target.closest("button"))) return;
      this.head.setPointerCapture(ev.pointerId);
      drag = { id: ev.pointerId, y: ev.clientY, h: this.el.getBoundingClientRect().height, moved: false };
      this.el.dataset.dragging = "";
    });
    this.head.addEventListener("pointermove", (ev) => {
      if (!drag || ev.pointerId !== drag.id) return;
      const dy = drag.y - ev.clientY;
      if (Math.abs(dy) >= TAP_PX) drag.moved = true;
      if (!drag.moved) return;
      const h = this.heights();
      this.el.style.height = `${Math.min(h.full, Math.max(h.peek, drag.h + dy))}px`;
    });
    /** @param {PointerEvent} ev */
    const end = (ev) => {
      if (!drag || ev.pointerId !== drag.id) return;
      const moved = drag.moved;
      drag = null;
      delete this.el.dataset.dragging;
      if (!moved) {
        this.snap(this.snapPoint === "peek" ? "half" : "peek");
        return;
      }
      // Snap to the nearest point.
      const now = this.el.getBoundingClientRect().height;
      const h = this.heights();
      const best = SNAPS.reduce((a, b) => (Math.abs(h[b] - now) < Math.abs(h[a] - now) ? b : a));
      this.snapPoint = best;
      this.render();
      this.onChange();
    };
    this.head.addEventListener("pointerup", end);
    this.head.addEventListener("pointercancel", end);
  }
}
