// Leaflet loader (ADR 0029): the vendored Leaflet 1.9.4 is loaded once,
// by the first map island, never by the pages without a map. Its
// stylesheet is appended to <head> (allowed by style-src 'self'); its
// script is a UMD build that sets window.L when imported as a module, and
// the import inherits the shell's nonce (ADR 0003). Leaflet changes styles
// through the CSSOM only, which the CSP allows.

const CSS_URL = "/static/vendor/leaflet/leaflet.css";

/** @type {Promise<any> | null} */
let pending = null;

/** stylesheet resolves once Leaflet's stylesheet is applied. */
function stylesheet() {
  return new Promise((resolve, reject) => {
    const link = document.createElement("link");
    link.rel = "stylesheet";
    link.href = CSS_URL;
    link.addEventListener("load", () => resolve(undefined), { once: true });
    link.addEventListener(
      "error",
      () => {
        link.remove();
        reject(new Error("the Leaflet stylesheet did not load"));
      },
      { once: true },
    );
    document.head.append(link);
  });
}

/**
 * loadLeaflet resolves to the Leaflet namespace; a failure is retried by
 * the next call.
 * @returns {Promise<any>}
 */
export function loadLeaflet() {
  if (pending === null) {
    pending = Promise.all([stylesheet(), import("../../vendor/leaflet/leaflet.js")]).then(() => {
      const L = /** @type {any} */ (window).L;
      if (!L?.map) throw new Error("Leaflet did not load");
      return L;
    });
    pending.catch(() => {
      pending = null;
      document.querySelector(`link[href="${CSS_URL}"]`)?.remove();
    });
  }
  return pending;
}
