// Single-key shortcuts (UI-002; the full set comes with UI-014). An element
// opts in with data-shortcut="<key>": the key activates (clicks) it, so a
// shortcut does exactly what the visible control does, and a link opening a
// new tab counts as a user action. No hx-on, no inline handlers (ADR 0003).
//
// Keys are ignored while typing (inputs, text areas, selects, editable
// content), with Ctrl, Alt, Meta or Shift, on auto-repeat and during IME
// composition, and when the admin turned shortcuts off
// (<body data-shortcuts="off">, ui.shortcut_set).

const EDITABLE = "input, textarea, select, [contenteditable]:not([contenteditable='false'])";

export function installShortcuts() {
  document.addEventListener("keydown", (event) => {
    if (event.defaultPrevented || event.repeat || event.isComposing) return;
    if (event.ctrlKey || event.altKey || event.metaKey || event.shiftKey) return;
    if (document.body.dataset.shortcuts === "off") return;
    if (event.target instanceof Element && event.target.closest(EDITABLE)) return;
    if (event.key.length !== 1) return;

    const target = document.querySelector(`[data-shortcut="${CSS.escape(event.key.toLowerCase())}"]`);
    if (!target) return;

    event.preventDefault();
    target.click();
  });
}
