// The keyboard shortcuts help (UI-015): built from the shortcuts registry
// (shortcuts.js) each time it opens, so every registered key, of this
// module or another, has its row; then the mouse and touch equivalents
// (FEATURE_SPEC §10.17) and the single-key shortcuts switch (WCAG 2.1.4,
// per-session state).

import { helpRows, setSingleKeys, singleKeys } from "../shortcuts.js";
import { el } from "./dom.js";

// Mouse and touch equivalents of the receiver.
const POINTER_ROWS = [
  ["Click or tap the waterfall, spectrum or scale", "Tune there"],
  ["Drag on the waterfall or spectrum", "Tune continuously"],
  ["Drag a filter edge on the scale", "Change the filter"],
  ["Wheel on the waterfall", "Zoom (tune with the Display menu's wheel swap)"],
  ["Shift + wheel on the waterfall", "Tune (zoom with the wheel swap)"],
  ["Wheel on a slider", "Move it one step"],
];

/**
 * section is a heading and a list of key and action pairs.
 * @param {string} title @param {[string, string, string?][]} rows keys, action, note
 * @param {boolean} [keys] the first column holds keys (else a gesture)
 */
function section(title, rows, keys = true) {
  const s = el("section", { class: "flex flex-col gap-1" });
  s.append(el("h3", { class: "font-semibold" }, title));
  const dl = el("dl", { class: "grid grid-cols-[minmax(0,10rem)_minmax(0,1fr)] gap-x-3 gap-y-2 text-sm" });
  for (const [what, action, note] of rows) {
    const dt = el("dt", { class: "flex min-w-0 flex-wrap items-start gap-1" });
    // One key per alternative ("M, Space").
    if (keys) for (const k of what.split(", ")) dt.append(el("kbd", { class: "kbd" }, k));
    else dt.textContent = what;
    const dd = el("dd", { class: "min-w-0 break-words" }, action);
    if (note) dd.append(el("span", { class: "block text-fg-muted" }, note));
    dl.append(dt, dd);
  }
  s.append(dl);
  return s;
}

/** @returns {HTMLElement} the help's content */
export function shortcutsHelp() {
  const root = el("div", { class: "flex flex-col gap-4" });
  root.append(el("p", { class: "text-sm text-fg-muted" }, "Shortcuts work anywhere on the page except while typing in a text field."));

  const box = /** @type {HTMLInputElement} */ (el("input", { class: "accent-accent" }));
  box.type = "checkbox";
  box.id = "rx-single-keys";
  box.checked = singleKeys();
  box.addEventListener("change", () => setSingleKeys(box.checked));
  const label = el("label", { class: "flex items-center gap-2 text-sm max-md:min-h-11 pointer-coarse:min-h-11" });
  label.htmlFor = box.id;
  label.append(box, document.createTextNode("Single-key shortcuts (letters, digits and symbols)"));
  root.append(label);

  /** @type {Map<string, [string, string, string?][]>} */
  const groups = new Map();
  for (const r of helpRows()) {
    if (!r.label) continue;
    if (!groups.has(r.group)) groups.set(r.group, []);
    groups.get(r.group)?.push([r.keys, r.label, r.note]);
  }
  for (const [title, rows] of groups) root.append(section(title, rows));
  root.append(section("Mouse and touch", /** @type {[string, string][]} */ (POINTER_ROWS), false));
  return root;
}
