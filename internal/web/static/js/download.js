// Downloads that change or reveal data (exports) are POST requests with the
// CSRF header (ADR 0003 §4C), which a plain link cannot send. A button with
// data-download="<url>" posts to it through apiFetch, then saves the
// response as data-download-name. Optional attributes:
//   data-download-form="<form id>"   sends that form's fields;
//   data-download-fields="k=v&k2=v2" adds fields;
//   data-download-status="<id>"      element (role=status) for the outcome.

import { apiFetch } from "./csrf.js";

function status(button, text) {
  const id = button.dataset.downloadStatus;
  const el = id ? document.getElementById(id) : null;
  if (el) {
    el.textContent = text;
  }
}

async function download(button) {
  const body = new URLSearchParams();
  const formID = button.dataset.downloadForm;
  const form = formID ? document.getElementById(formID) : null;
  if (form instanceof HTMLFormElement) {
    for (const [k, v] of new FormData(form)) {
      if (typeof v === "string") {
        body.append(k, v);
      }
    }
  }
  for (const [k, v] of new URLSearchParams(button.dataset.downloadFields ?? "")) {
    body.append(k, v);
  }

  button.disabled = true;
  status(button, "Preparing the download…");
  try {
    const res = await apiFetch(button.dataset.download, {
      method: "POST",
      headers: { "Content-Type": "application/x-www-form-urlencoded" },
      body,
    });
    if (!res.ok) {
      throw new Error(`HTTP ${res.status}`);
    }
    const url = URL.createObjectURL(await res.blob());
    const a = document.createElement("a");
    a.href = url;
    a.download = button.dataset.downloadName ?? "download";
    document.body.append(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 10_000);
    status(button, "Downloaded.");
  } catch (err) {
    status(button, `The download failed (${err.message}).`);
  } finally {
    button.disabled = false;
  }
}

document.addEventListener("click", (evt) => {
  const button = evt.target instanceof Element ? evt.target.closest("button[data-download]") : null;
  if (button) {
    evt.preventDefault();
    download(button);
  }
});
