// Confirmation dialog (UI-012): the one modal of the app, for consequential
// actions only: switching a shared preset or moving the centre while others
// listen, and destructive admin actions. The <dialog id="msdr-confirm"> is
// in the shell, outside #main. showModal() makes the rest of the page inert
// (the focus stays in the dialog), Esc cancels, and the focus returns to
// the control that opened it.
//
// Admin forms keep htmx's hx-confirm="<question>" (window.confirm when this
// module is missing, progressive behaviour); the htmx:confirm hook shows
// this dialog instead, with data-confirm-action as the confirm button text
// when the element has it. No hx-on (ADR 0003).

/** @type {Promise<boolean> | null} */
let open = null;

/**
 * confirmDialog asks a question; it resolves true when confirmed, false
 * when cancelled (Cancel, Esc). Without the dialog element it falls back to
 * window.confirm.
 * @param {{title?: string, message: string, confirmLabel?: string, danger?: boolean}} o
 * @returns {Promise<boolean>}
 */
export function confirmDialog({ title = "Please confirm", message, confirmLabel = "Confirm", danger = false }) {
  const dialog = /** @type {HTMLDialogElement | null} */ (document.getElementById("msdr-confirm"));
  if (!dialog || typeof dialog.showModal !== "function") return Promise.resolve(window.confirm(message));
  if (open) return Promise.resolve(false);

  const titleEl = dialog.querySelector("[data-confirm-title]");
  const messageEl = dialog.querySelector("[data-confirm-message]");
  const ok = /** @type {HTMLButtonElement | null} */ (dialog.querySelector("[data-confirm-ok]"));
  const cancel = /** @type {HTMLButtonElement | null} */ (dialog.querySelector("[data-confirm-cancel]"));
  if (titleEl) titleEl.textContent = title;
  if (messageEl) messageEl.textContent = message;
  if (ok) {
    ok.textContent = confirmLabel;
    ok.classList.toggle("button-danger", danger);
    ok.classList.toggle("button-primary", !danger);
  }

  const origin = document.activeElement instanceof HTMLElement ? document.activeElement : null;
  open = new Promise((resolve) => {
    /** @param {boolean} v */
    const done = (v) => {
      ok?.removeEventListener("click", onOK);
      cancel?.removeEventListener("click", onCancel);
      dialog.removeEventListener("close", onClose);
      if (dialog.open) dialog.close();
      open = null;
      // Back to the control that asked (UI-012).
      if (origin?.isConnected) origin.focus();
      resolve(v);
    };
    const onOK = () => done(true);
    const onCancel = () => done(false);
    // Esc closes the dialog (cancel, then close).
    const onClose = () => done(false);
    ok?.addEventListener("click", onOK);
    cancel?.addEventListener("click", onCancel);
    dialog.addEventListener("close", onClose);
    dialog.showModal();
    // The safe choice has the focus.
    cancel?.focus();
  });
  return open;
}

// installConfirm routes htmx's hx-confirm through the dialog.
export function installConfirm() {
  document.addEventListener("htmx:confirm", (evt) => {
    const e = /** @type {CustomEvent} */ (evt);
    const d = e.detail;
    const question = d?.ctx?.confirm;
    if (!question || typeof d.issueRequest !== "function") return;
    e.preventDefault();
    const src = d.ctx.sourceElement instanceof Element ? d.ctx.sourceElement : null;
    const action = src?.closest("[data-confirm-action]")?.getAttribute("data-confirm-action") || "Confirm";
    confirmDialog({ title: "Please confirm", message: question, confirmLabel: action, danger: true }).then((yes) =>
      yes ? d.issueRequest() : d.dropRequest(),
    );
  });
}
