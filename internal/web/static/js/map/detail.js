// Detail panel of the map (MAP-014, FEATURE_SPEC §10.10): the fields of
// the selected entity, its distance from the receiver that heard it, when
// it was last heard, its path and the actions "Tune receiver" (deep link
// to the Receiver, RX-028, MAP-016) and "Show decodes". Every value comes
// from radio data: it is set with textContent, never as HTML, and every
// value put in a link is URL-encoded.

import { el } from "../receiver/dom.js";
import { agoText, distanceKm, finite, latLonText, utcText } from "./geo.js";
import { typeOf } from "./kinds.js";

/**
 * @typedef {object} DeviceInfo a device the visitor may listen to
 * @property {string} id
 * @property {string} name
 * @property {string} node_id
 */

/**
 * @typedef {object} DetailContext
 * @property {Map<string, DeviceInfo>} devices
 * @property {import("./store.js").Receiver[]} receivers
 * @property {number} now
 */

const LINK = "underline";

// Formatted detail fields, in order; label, mode, freq_hz, hops and weather
// have their own rows.
/** @type {[string, string, ((v: any) => string)?][]} */
const FIELDS = [
  ["callsign", "Callsign"],
  ["source", "Source"],
  ["aprs_type", "Report"],
  ["locator", "Locator"],
  ["symbol", "Symbol"],
  ["comment", "Comment"],
  ["course", "Course", (v) => `${Math.round(v)}°`],
  ["speed_kmh", "Speed", (v) => `${Math.round(v * 10) / 10} km/h`],
  ["altitude_m", "Altitude", (v) => `${Math.round(v)} m`],
  ["db", "SNR", (v) => `${v} dB`],
  ["dbm", "Power", (v) => `${v} dBm`],
  ["ambiguity", "Position ambiguity", (v) => `${v} digit${v === 1 ? "" : "s"}`],
];
const OWN = new Set(["label", "mode", "freq_hz", "hops", "weather", ...FIELDS.map((f) => f[0])]);

/** @param {string} key */
function human(key) {
  const s = key.replace(/_/g, " ");
  return s.charAt(0).toUpperCase() + s.slice(1);
}

/** @param {unknown} v @returns {string | null} a scalar as text */
function scalar(v) {
  if (typeof v === "string") return v;
  if (finite(v)) return String(Math.round(v * 1000) / 1000);
  if (typeof v === "boolean") return v ? "Yes" : "No";
  return null;
}

/**
 * freqText is a frequency in MHz.
 * @param {number} hz
 */
export function freqText(hz) {
  return `${(hz / 1e6).toFixed(6).replace(/0{1,3}$/, "")} MHz`;
}

/**
 * deviceName names a device: its name, else its id.
 * @param {DetailContext} ctx @param {string} id
 */
export function deviceName(ctx, id) {
  return ctx.devices.get(id)?.name || id;
}

/**
 * receiverLink is the deep link that tunes a device to a report (RX-028):
 * /receiver/{node}/{device}?f=<Hz>&m2=<mode>; null when the node is not
 * known.
 * @param {DetailContext} ctx @param {string} device @param {Record<string, any>} details
 */
export function receiverLink(ctx, device, details) {
  const d = ctx.devices.get(device);
  if (!d) return null;
  const q = new URLSearchParams();
  if (finite(details.freq_hz) && details.freq_hz > 0) q.set("f", String(Math.round(details.freq_hz)));
  if (typeof details.mode === "string" && /^[a-z0-9_-]{1,24}$/.test(details.mode)) q.set("m2", details.mode);
  const s = q.toString();
  return `/receiver/${encodeURIComponent(d.node_id)}/${encodeURIComponent(d.id)}${s ? `?${s}` : ""}`;
}

/**
 * decodesLink is the Decodes page filtered by a device and a mode.
 * @param {string} device @param {Record<string, any>} details
 */
export function decodesLink(device, details) {
  const q = new URLSearchParams({ device });
  if (typeof details.mode === "string" && details.mode) q.set("mode", details.mode);
  return `/decodes?${q}`;
}

/**
 * row appends a dt/dd pair.
 * @param {HTMLElement} dl @param {string} term @param {string | Node} value
 */
function row(dl, term, value) {
  dl.append(el("dt", { class: "text-fg-muted" }, term));
  const dd = el("dd", { class: "min-w-0" });
  if (typeof value === "string") dd.textContent = value;
  else dd.append(value);
  dl.append(dd);
}

/**
 * receiverOf returns the receiver marker holding a device.
 * @param {DetailContext} ctx @param {string} device
 */
function receiverOf(ctx, device) {
  return ctx.receivers.find((r) => r.devices.some((d) => d.id === device)) ?? null;
}

/**
 * distanceText is the distance of a position from the receiver of a
 * device ("" when either is unknown).
 * @param {DetailContext} ctx @param {string} device @param {[number, number] | null} at
 */
function distanceText(ctx, device, at) {
  const r = receiverOf(ctx, device);
  if (!r || !at) return "";
  const km = distanceKm(r.lat, r.lon, at[0], at[1]);
  return `${km < 10 ? km.toFixed(1) : Math.round(km)} km from ${r.name}${r.precise ? "" : " (approximately)"}`;
}

/**
 * actions lists the reporting devices with their links.
 * @param {DetailContext} ctx @param {import("./store.js").Entity} e
 */
function actions(ctx, e) {
  const details = e.feature?.details ?? {};
  const list = el("ul", { class: "mt-3 flex flex-col gap-2" });
  for (const id of e.devices) {
    const li = el("li", { class: "flex flex-col gap-1" });
    li.append(el("span", { class: "font-medium" }, deviceName(ctx, id)));
    const links = el("span", { class: "flex flex-wrap gap-3 text-sm" });
    const tune = receiverLink(ctx, id, details);
    if (tune) links.append(el("a", { href: tune, class: LINK }, "Tune receiver"));
    links.append(el("a", { href: decodesLink(id, details), class: LINK }, "Show decodes"));
    li.append(links);
    list.append(li);
  }
  return list;
}

/**
 * endpointText is one end of a call line.
 * @param {any} p
 */
function endpointText(p) {
  if (!p) return "";
  const call = typeof p.callsign === "string" ? p.callsign : "";
  const loc = typeof p.locator === "string" && p.locator ? ` (${p.locator})` : "";
  return `${call}${loc}`;
}

/**
 * renderDetail returns the body of the detail panel of an entity.
 * @param {import("./store.js").Entity} e @param {DetailContext} ctx
 * @returns {DocumentFragment}
 */
export function renderDetail(e, ctx) {
  const out = document.createDocumentFragment();
  const dl = el("dl", { class: "mt-2 grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-sm" });
  out.append(dl);
  row(dl, "Type", typeOf(e.kind).one);

  if (e.receiver) {
    const r = e.receiver;
    row(dl, "Locator", r.locator);
    row(dl, "Position", r.precise ? latLonText(r.lat, r.lon) : `Centre of ${r.locator} (approximate)`);
    out.append(el("h3", { class: "mt-3 font-semibold" }, r.devices.length === 1 ? "Device" : "Devices"));
    const list = el("ul", { class: "flex flex-col gap-1 text-sm" });
    for (const d of r.devices) {
      const li = el("li");
      const info = ctx.devices.get(d.id);
      if (info) {
        li.append(el("a", { href: `/receiver/${encodeURIComponent(info.node_id)}/${encodeURIComponent(d.id)}`, class: LINK }, d.name || d.id));
      } else {
        li.textContent = d.name || d.id;
      }
      list.append(li);
    }
    out.append(list);
    return out;
  }

  const f = /** @type {import("./store.js").Feature} */ (e.feature);
  const d = f.details;
  const g = f.geometry;
  if (g.type === "line") {
    row(dl, "From", endpointText(g.from));
    row(dl, "To", endpointText(g.to));
    if ([g.from?.lat, g.from?.lon, g.to?.lat, g.to?.lon].every(finite)) {
      row(dl, "Distance", `${Math.round(distanceKm(g.from.lat, g.from.lon, g.to.lat, g.to.lon))} km`);
    }
  } else if (g.type === "locator") {
    row(dl, "Square", String(g.locator ?? ""));
  } else if (e.at) {
    row(dl, "Position", latLonText(e.at[0], e.at[1]));
  }
  if (e.devices[0]) {
    const dist = g.type === "line" ? "" : distanceText(ctx, e.devices[0], e.at);
    if (dist) row(dl, "Distance", dist);
  }
  row(dl, "Last heard", `${utcText(f.updated_at)} (${agoText(f.updated_at, ctx.now)})`);
  if (finite(e.expires_at)) row(dl, "Shown until", utcText(e.expires_at));
  if (typeof d.mode === "string" && d.mode) row(dl, "Mode", d.mode);
  if (finite(d.freq_hz) && d.freq_hz > 0) row(dl, "Frequency", freqText(d.freq_hz));
  for (const [key, label, format] of FIELDS) {
    const v = d[key];
    if (v === undefined || v === null || v === "") continue;
    if (key === "callsign" && v === e.subject) continue;
    const text = format && finite(v) ? format(v) : scalar(v);
    if (text) row(dl, label, text);
  }
  if (Array.isArray(d.hops) && d.hops.length > 0) {
    row(dl, "Via", d.hops.filter((h) => typeof h === "string").join(" › "));
  }
  if (d.weather && typeof d.weather === "object") {
    const parts = Object.entries(d.weather)
      .map(([k, v]) => (scalar(v) ? `${human(k)} ${scalar(v)}` : ""))
      .filter(Boolean);
    if (parts.length > 0) row(dl, "Weather", parts.join(", "));
  }
  for (const [k, v] of Object.entries(d)) {
    if (OWN.has(k)) continue;
    const text = scalar(v);
    if (text) row(dl, human(k), text);
  }
  if (Array.isArray(g.track) && g.track.length > 0) row(dl, "Track", `${g.track.length + 1} positions`);
  if (e.devices.length > 0) {
    out.append(el("h3", { class: "mt-3 font-semibold" }, "Heard by"));
    out.append(actions(ctx, e));
  }
  return out;
}

/**
 * placeText is the place of an entity in the list view: its
 * locator or position.
 * @param {import("./store.js").Entity} e
 */
export function placeText(e) {
  if (e.receiver) return e.receiver.locator;
  const g = e.feature?.geometry;
  if (g?.type === "locator") return String(g.locator ?? "");
  if (g?.type === "line") return `${endpointText(g.from)} – ${endpointText(g.to)}`;
  if (e.feature?.details?.locator) return String(e.feature.details.locator);
  return e.at ? latLonText(e.at[0], e.at[1]) : "";
}
