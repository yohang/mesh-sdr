// Lookup links of the map (MAP-015): the links.*_url templates of GET
// /api/v1/map/config with their {} replaced by the URL-encoded value. Only
// http and https links are made; they open in a new tab with
// rel="noopener noreferrer".

import { el } from "../receiver/dom.js";

const CALLSIGN = /^[A-Z0-9/]{3,16}$/;

/**
 * webURL parses an http or https URL; null for anything else.
 * @param {string} href
 */
export function webURL(href) {
  try {
    const u = new URL(href);
    return u.protocol === "https:" || u.protocol === "http:" ? u : null;
  } catch {
    return null;
  }
}

/**
 * newTabLink is a link to an http or https URL (webURL), in a new tab.
 * @param {URL} u @param {string} text
 */
export function newTabLink(u, text) {
  const a = el("a", { href: u.href, target: "_blank", rel: "noopener noreferrer", class: "underline" }, text);
  a.append(el("span", { class: "sr-only" }, " (opens in a new tab)"));
  return a;
}

/**
 * lookups returns the lookups of a feature, as [template, value] pairs:
 * the callsigns of a station, locator or call (without the APRS SSID), the
 * MMSI of a vessel, the ICAO address and flight of an aircraft, the serial
 * of a sonde.
 * @param {import("./store.js").Feature} f
 * @returns {[string, string][]}
 */
function lookups(f) {
  const d = f.details;
  const text = (/** @type {unknown} */ v) => (typeof v === "string" ? v.trim() : "");
  switch (f.kind) {
    case "aprs":
    case "locator": {
      const call = (text(d.source) || f.subject).toUpperCase().replace(/-[0-9A-Z]{1,2}$/, "");
      return CALLSIGN.test(call) ? [["callsign_url", call]] : [];
    }
    case "call":
      return [text(d.from), text(d.to)].filter((c) => CALLSIGN.test(c)).map((c) => ["callsign_url", c]);
    case "vessel":
      return [["vessel_url", f.subject]];
    case "aircraft":
      return [
        ["modes_url", f.subject],
        ["flight_url", text(d.flight)],
      ];
    case "sonde":
      return [["sonde_url", f.subject]];
    default:
      return [];
  }
}

/**
 * lookupLinks returns the lookup links of a feature (possibly none).
 * @param {import("./store.js").Feature} f @param {Record<string, unknown>} templates
 * @returns {HTMLElement[]}
 */
export function lookupLinks(f, templates) {
  /** @type {HTMLElement[]} */
  const out = [];
  for (const [name, value] of lookups(f)) {
    const template = templates[name];
    if (typeof template !== "string" || !template.includes("{}") || !value) continue;
    const u = webURL(template.replace("{}", encodeURIComponent(value)));
    if (u) out.push(newTabLink(u, `${value} on ${u.host}`));
  }
  return out;
}
