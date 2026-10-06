// Admin settings forms (FEATURE_SPEC §10.13 "Save model"): a form marked
// data-msdr-form shows its "Unsaved changes" indicator once edited, and
// leaving the page (boosted navigation or unload) with an unsaved form asks
// for confirmation. A saved form is replaced by the server's fragment,
// which starts clean.

const FORM = "form[data-msdr-form]";

function dirtyForms() {
  return document.querySelectorAll(`${FORM}[data-dirty]`);
}

function markDirty(evt) {
  const form = evt.target.closest?.(FORM);
  if (!form || form.hasAttribute("data-dirty")) {
    return;
  }
  form.setAttribute("data-dirty", "");
  const indicator = form.querySelector("[data-dirty-indicator]");
  if (indicator) {
    indicator.hidden = false;
  }
}

export function installAdminForms() {
  document.addEventListener("input", markDirty);
  document.addEventListener("change", markDirty);

  window.addEventListener("beforeunload", (evt) => {
    if (dirtyForms().length > 0) {
      evt.preventDefault();
    }
  });

  // Boosted navigation away from a page with unsaved changes. The form's
  // own submission is not a navigation.
  document.addEventListener("htmx:before:request", (evt) => {
    const req = evt.detail?.ctx?.request;
    const source = evt.detail?.ctx?.sourceElement ?? evt.target;
    if (!req || (req.method ?? "GET").toUpperCase() !== "GET" || source?.closest?.(FORM)) {
      return;
    }
    if (dirtyForms().length > 0 && !window.confirm("You have unsaved changes. Leave this page?")) {
      evt.preventDefault();
    }
  });

  // After a swap, focus the error summary of a rejected form so keyboard and
  // screen-reader users land on it.
  // The swap replaces the form (outerHTML): find the new one by its id.
  document.addEventListener("htmx:after:swap", (evt) => {
    const target = evt.detail?.ctx?.target ?? evt.target;
    if (!target?.matches?.(FORM) || !target.id) {
      return;
    }
    document.getElementById(target.id)?.querySelector('[role="alert"]')?.focus();
  });
}
