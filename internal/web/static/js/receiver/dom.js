// DOM helpers of the receiver modules: element creation, the control
// classes and the frequency text.

// Controls are 44 px touch targets on touch screens and below 768 px
// (UI-007).
export const TOUCH = "max-md:min-h-11 max-md:min-w-11 pointer-coarse:min-h-11 pointer-coarse:min-w-11";
export const BUTTON = `rounded border border-border px-3 py-1 text-sm ${TOUCH}`;
export const SMALL_BUTTON = `rounded border border-border px-2 py-1 text-sm ${TOUCH}`;
export const FIELD = `rounded border px-2 py-1 ${TOUCH}`;

/**
 * el creates an element with attributes and optional text content.
 * @param {string} tag @param {Record<string, string>} [attrs] @param {string} [text]
 */
export function el(tag, attrs = {}, text) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, v);
  if (text !== undefined) e.textContent = text;
  return e;
}

/**
 * formatMHz is a frequency in MHz with six decimals; trim drops up to three
 * trailing zeros, unit: false leaves out " MHz".
 * @param {number} hz @param {{trim?: boolean, unit?: boolean}} [opts]
 */
export function formatMHz(hz, { trim = false, unit = true } = {}) {
  let s = (hz / 1e6).toFixed(6);
  if (trim) s = s.replace(/0{1,3}$/, "");
  return unit ? `${s} MHz` : s;
}
