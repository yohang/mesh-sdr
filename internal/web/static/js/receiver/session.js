// Per-session runtime state (rt) the browser remembers (FEATURE_SPEC
// §10.18): display toggles and saved pass bands. A convenience only: one
// namespaced localStorage key, never sent to the hub, and every value
// falls back to its default when storage is blocked or holds junk.

const KEY = "msdr.v1.session";

/** @returns {Record<string, any>} */
function load() {
  try {
    const v = JSON.parse(localStorage.getItem(KEY) ?? "{}");
    return v && typeof v === "object" && !Array.isArray(v) ? v : {};
  } catch {
    return {};
  }
}

/** @type {Record<string, any> | null} memory when storage is blocked */
let memory = null;

/**
 * getState returns a remembered value, or def when there is none or it
 * has another type than def.
 * @template T
 * @param {string} name @param {T} def
 * @returns {T}
 */
export function getState(name, def) {
  const v = (memory ?? load())[name];
  return v !== undefined && v !== null && typeof v === typeof def ? v : def;
}

/**
 * setState remembers a value; undefined forgets it.
 * @param {string} name @param {any} value
 */
export function setState(name, value) {
  const all = memory ?? load();
  if (value === undefined) delete all[name];
  else all[name] = value;
  try {
    localStorage.setItem(KEY, JSON.stringify(all));
    memory = null;
  } catch {
    // Storage blocked: remembered for this page only.
    memory = all;
  }
}
