// Keyboard shortcuts (UI-014): one document keydown handler and the
// registry of the bindings, which the shortcuts help (UI-015) lists, so a
// binding and its help row come from the same entry.
//
// Two ways to bind a key:
//   - registerShortcuts([{keys, label, group, run, ...}]) from a module
//     (the receiver island, the audio dock); it returns the function that
//     removes them again (an island's disconnectedCallback);
//   - data-shortcut="<key>" on an element (the top bar's Help link): the key
//     activates (clicks) it, so the shortcut does exactly what the visible
//     control does. Its help row is data-shortcut-label, else aria-label,
//     else its text. No hx-on, no inline handlers (ADR 0003).
//
// Key syntax: a character ("r", "?", "[", "|"), "Space", a key name
// ("ArrowLeft", "PageUp", "Escape", "Enter"), or a physical key code
// ("Digit1", matched with event.code, so digits work on AZERTY too), each
// optionally prefixed by "Ctrl+", "Alt+" and "Shift+". A character binding
// ignores Shift, which typing the character may need ("?", "{"), and AltGr.
//
// Keys are ignored while typing (inputs, text areas, selects, editable
// content), during IME composition, on auto-repeat (unless the binding
// repeats, like a held button), with Meta, and when a modal dialog is
// open. Space and Enter are ignored while a button, link or other control
// has the focus (they activate it). Nothing traps the focus, and the keys
// screen readers use with modifiers are left alone.
//
// Single-key shortcuts (character keys, WCAG 2.1.4) are off when the admin
// turned them off (<body data-shortcuts="off">, ui.shortcut_set) unless the
// visitor turned them on in the help, and the other way round: the
// visitor's choice is per-session state the browser remembers
// (setSingleKeys). Named keys and combinations with Ctrl or Alt stay on.

const EDITABLE = "input, textarea, select, [contenteditable]:not([contenteditable='false'])";
const INTERACTIVE = "a[href], button, input, select, textarea, summary, [role='button'], [role='tab'], [role='link'], [role='checkbox'], [role='menuitem'], [tabindex]:not([tabindex='-1'])";
// The visitor's single-key shortcut choice, "on" or "off" (UI-014, rt).
const SINGLE_KEYS_KEY = "msdr.shortcuts.single";

/**
 * @typedef {object} Shortcut a binding
 * @property {string[]} keys the keys, in the syntax above; the first one is
 *   shown first in the help
 * @property {string} label what the key does, for the help
 * @property {string} [group] help section ("Tuning", "Audio"…)
 * @property {string} [display] help text of the keys, when the keys list
 *   would be too long ("1 … 9, 0")
 * @property {string} [note] help note ("Operators only")
 * @property {boolean} [repeat] handle auto-repeat (held keys)
 * @property {() => boolean} [enabled] false: the key does nothing and is
 *   left to the browser
 * @property {(event: KeyboardEvent) => boolean | void} run the action;
 *   false: not handled (the browser default applies)
 */

/**
 * @typedef {object} Binding a parsed key
 * @property {string} key a lower-case character, a key name or a code
 * @property {"char" | "name" | "code"} kind
 * @property {boolean} ctrl
 * @property {boolean} alt
 * @property {boolean} shift
 */

/** @type {Set<{shortcut: Shortcut, bindings: Binding[]}>} */
const registry = new Set();

/**
 * parseKey reads a key of the syntax above.
 * @param {string} spec
 * @returns {Binding}
 */
export function parseKey(spec) {
  const parts = spec.split("+");
  // "Shift++" would be "+": the last empty part is the key.
  let key = parts.pop() || "+";
  if (key === "+" && parts.at(-1) === "") parts.pop();
  const mods = new Set(parts.map((p) => p.toLowerCase()));
  /** @type {Binding["kind"]} */
  let kind = "name";
  if (key === "Space") {
    key = " ";
    kind = "char";
  } else if (key.length === 1) {
    key = key.toLowerCase();
    kind = "char";
  } else if (/^(Digit\d|Key[A-Z]|Numpad\d)$/.test(key)) {
    kind = "code";
  }
  return { key, kind, ctrl: mods.has("ctrl"), alt: mods.has("alt"), shift: mods.has("shift") };
}

/**
 * matches tells whether event presses binding.
 * @param {Binding} b @param {KeyboardEvent} ev
 */
function matches(b, ev) {
  const altGraph = typeof ev.getModifierState === "function" && ev.getModifierState("AltGraph");
  // AltGr reports Ctrl+Alt on some systems: it only selects the character.
  const ctrl = ev.ctrlKey && !altGraph;
  const alt = ev.altKey && !altGraph;
  if (b.kind === "char") {
    return !ctrl && !alt && b.ctrl === false && b.alt === false && ev.key.toLowerCase() === b.key;
  }
  if (b.kind === "code") {
    return !altGraph && ev.code === b.key && ctrl === b.ctrl && alt === b.alt && ev.shiftKey === b.shift;
  }
  return ev.key === b.key && ctrl === b.ctrl && alt === b.alt && ev.shiftKey === b.shift;
}

/** @param {Binding} b single-key (a character key without Ctrl or Alt) */
function isSingle(b) {
  return (b.kind === "char" || b.kind === "code") && !b.ctrl && !b.alt;
}

/**
 * registerShortcuts adds bindings; the returned function removes them.
 * @param {Shortcut[]} shortcuts
 * @returns {() => void}
 */
export function registerShortcuts(shortcuts) {
  const entries = shortcuts.map((shortcut) => ({ shortcut, bindings: shortcut.keys.map(parseKey) }));
  for (const e of entries) registry.add(e);
  return () => {
    for (const e of entries) registry.delete(e);
  };
}

/** @returns {boolean} whether single-key shortcuts are on */
export function singleKeys() {
  let mine = null;
  try {
    mine = localStorage.getItem(SINGLE_KEYS_KEY);
  } catch {
    // Storage blocked: the admin default applies.
  }
  if (mine === "on" || mine === "off") return mine === "on";
  return document.body.dataset.shortcuts !== "off";
}

/**
 * setSingleKeys turns single-key shortcuts on or off for this browser
 * (WCAG 2.1.4, per-session state).
 * @param {boolean} on
 */
export function setSingleKeys(on) {
  try {
    localStorage.setItem(SINGLE_KEYS_KEY, on ? "on" : "off");
  } catch {
    // Storage blocked: the choice lasts for this page.
  }
  document.body.dataset.shortcutsMine = on ? "on" : "off";
}

/** @returns {boolean} single-key shortcuts on, with this page's choice */
function singleOn() {
  const mine = document.body.dataset.shortcutsMine;
  if (mine === "on" || mine === "off") return mine === "on";
  return singleKeys();
}

/**
 * @typedef {object} HelpRow a row of the shortcuts help
 * @property {string} keys
 * @property {string} label
 * @property {string} group
 * @property {string} note
 */

/**
 * keyText is the help text of a key ("Shift + ←").
 * @param {string} spec
 */
export function keyText(spec) {
  const b = parseKey(spec);
  const names = /** @type {Record<string, string>} */ ({
    " ": "Space",
    ArrowLeft: "←",
    ArrowRight: "→",
    ArrowUp: "↑",
    ArrowDown: "↓",
    Escape: "Esc",
    PageUp: "Page Up",
    PageDown: "Page Down",
  });
  let k = names[b.key] ?? b.key;
  if (b.kind === "code") k = b.key.replace(/^(Digit|Key|Numpad)/, "");
  else if (b.kind === "char" && k.length === 1) k = k.toUpperCase();
  return [b.ctrl && "Ctrl", b.alt && "Alt", b.shift && "Shift", k].filter(Boolean).join(" + ");
}

/**
 * helpRows lists every shortcut for the help (UI-015): the registered ones
 * and the data-shortcut elements of the page, in registration then
 * document order.
 * @returns {HelpRow[]}
 */
export function helpRows() {
  /** @type {HelpRow[]} */
  const rows = [];
  for (const { shortcut: s } of registry) {
    rows.push({ keys: s.display ?? s.keys.map(keyText).join(", "), label: s.label, group: s.group ?? "General", note: s.note ?? "" });
  }
  for (const el of document.querySelectorAll("[data-shortcut]")) {
    if (!(el instanceof HTMLElement)) continue;
    const label = el.dataset.shortcutLabel || el.getAttribute("aria-label") || el.textContent?.trim() || "";
    rows.push({ keys: keyText(el.dataset.shortcut ?? ""), label, group: el.dataset.shortcutGroup || "General", note: "" });
  }
  return rows;
}

/** @param {KeyboardEvent} ev @param {Binding} b */
function blockedByFocus(ev, b) {
  const t = ev.target instanceof Element ? ev.target : null;
  if (!t) return false;
  if (t.closest(EDITABLE)) return true;
  // Space and Enter activate the focused control.
  return (b.key === " " || b.key === "Enter") && t !== document.body && t.closest(INTERACTIVE) !== null;
}

export function installShortcuts() {
  document.addEventListener("keydown", (event) => {
    if (event.defaultPrevented || event.isComposing || event.metaKey) return;
    if (document.querySelector("dialog[open]:modal")) return;
    const single = singleOn();

    for (const { shortcut: s, bindings } of registry) {
      const b = bindings.find((x) => matches(x, event));
      if (!b || (event.repeat && !s.repeat) || (isSingle(b) && !single) || blockedByFocus(event, b)) continue;
      if (s.enabled && !s.enabled()) continue;
      if (s.run(event) === false) continue;
      event.preventDefault();
      return;
    }

    // data-shortcut elements: one character, or a key name in lower case.
    if (event.ctrlKey || event.altKey || event.shiftKey || event.repeat) return;
    const named = event.key.length > 1;
    if ((!named && !single) || (event.target instanceof Element && event.target.closest(EDITABLE))) return;
    const target = document.querySelector(`[data-shortcut="${CSS.escape(event.key.toLowerCase())}"]`);
    if (!(target instanceof HTMLElement) || target.closest("[hidden]") || target.matches(":disabled")) return;
    event.preventDefault();
    target.click();
  });

  // The shortcuts help (UI-015): the Receiver page shows it in its side
  // panel (it cancels msdr:shortcuts-help); elsewhere the link opens the
  // Receiver with that view. "?" and "/" open it from every page.
  const openHelp = () => {
    const ev = new CustomEvent("msdr:shortcuts-help", { cancelable: true });
    document.dispatchEvent(ev);
    return ev.defaultPrevented;
  };
  // Capture phase: before htmx boosts the link into a navigation.
  document.addEventListener(
    "click",
    (event) => {
      const link = event.target instanceof Element ? event.target.closest("a[data-shortcuts-help]") : null;
      if (!link || event.button !== 0 || event.ctrlKey || event.metaKey || event.shiftKey || event.altKey) return;
      if (!openHelp()) return;
      event.preventDefault();
      event.stopPropagation();
      link.closest("[popover]")?.hidePopover?.();
    },
    true,
  );
  registerShortcuts([
    {
      keys: ["?", "/"],
      label: "Show the keyboard shortcuts",
      group: "General",
      run: () => {
        if (openHelp()) return;
        const link = document.querySelector("a[data-shortcuts-help]");
        if (!(link instanceof HTMLElement)) return false;
        link.click();
      },
    },
  ]);
}
