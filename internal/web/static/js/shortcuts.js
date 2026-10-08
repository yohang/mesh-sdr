// Single-key shortcuts (UI-002; the full set comes with UI-014). An element
// opts in with data-shortcut="<key>": the key activates (clicks) it, so a
// shortcut does exactly what the visible control does, and a link opening a
// new tab counts as a user action. No hx-on, no inline handlers (ADR 0003).
// Keys are one character, or one of NAMED (lower case, such as
// data-shortcut="arrowleft" for the receiver's tune step, RX-009).
//
// Keys are ignored while typing (inputs, text areas, selects, editable
// content), with Ctrl, Alt, Meta or Shift, on auto-repeat (except NAMED
// keys, which repeat like a held button) and during IME composition, and
// when the admin turned shortcuts off (<body data-shortcuts="off">,
// ui.shortcut_set). A hidden or disabled control is not activated.

const EDITABLE = "input, textarea, select, [contenteditable]:not([contenteditable='false'])";
const NAMED = new Set(["ArrowLeft", "ArrowRight"]);

export function installShortcuts() {
  document.addEventListener("keydown", (event) => {
    const named = NAMED.has(event.key);
    if (event.defaultPrevented || (event.repeat && !named) || event.isComposing) return;
    if (event.ctrlKey || event.altKey || event.metaKey || event.shiftKey) return;
    if (document.body.dataset.shortcuts === "off") return;
    if (event.target instanceof Element && event.target.closest(EDITABLE)) return;
    if (event.key.length !== 1 && !named) return;

    const target = document.querySelector(`[data-shortcut="${CSS.escape(event.key.toLowerCase())}"]`);
    if (!(target instanceof HTMLElement) || target.closest("[hidden]") || target.matches(":disabled")) return;

    event.preventDefault();
    target.click();
  });
}
