// Geography helpers of the map: Maidenhead locator squares (the bounds of
// a 2, 4, 6 or 8 character locator; the grid overlay is MAP-005), great
// circle distances, and the text of positions and times.

const LOCATOR = /^[A-R]{2}(?:[0-9]{2}(?:[A-X]{2}(?:[0-9]{2})?)?)?$/;

/**
 * isLocator tells whether s is a Maidenhead locator (case-insensitive).
 * @param {string} s
 */
export function isLocator(s) {
  return LOCATOR.test(String(s ?? "").trim().toUpperCase());
}

/**
 * locatorBounds returns the square of a locator as [[south, west],
 * [north, east]], or null when it is not a locator.
 * @param {string} locator
 * @returns {[[number, number], [number, number]] | null}
 */
export function locatorBounds(locator) {
  const u = String(locator ?? "").trim().toUpperCase();
  if (!LOCATOR.test(u)) return null;
  const letter = (/** @type {number} */ i) => u.charCodeAt(i) - 65;
  let w = 20;
  let h = 10;
  let lon = -180 + letter(0) * w;
  let lat = -90 + letter(1) * h;
  if (u.length >= 4) {
    w = 2;
    h = 1;
    lon += Number(u[2]) * w;
    lat += Number(u[3]) * h;
  }
  if (u.length >= 6) {
    w /= 24;
    h /= 24;
    lon += letter(4) * w;
    lat += letter(5) * h;
  }
  if (u.length >= 8) {
    w /= 10;
    h /= 10;
    lon += Number(u[6]) * w;
    lat += Number(u[7]) * h;
  }
  return [
    [lat, lon],
    [lat + h, lon + w],
  ];
}

/**
 * distanceKm is the great circle distance between two positions.
 * @param {number} lat1 @param {number} lon1 @param {number} lat2 @param {number} lon2
 */
export function distanceKm(lat1, lon1, lat2, lon2) {
  const rad = Math.PI / 180;
  const dLat = (lat2 - lat1) * rad;
  const dLon = (lon2 - lon1) * rad;
  const a = Math.sin(dLat / 2) ** 2 + Math.cos(lat1 * rad) * Math.cos(lat2 * rad) * Math.sin(dLon / 2) ** 2;
  return 2 * 6371 * Math.asin(Math.min(1, Math.sqrt(a)));
}

/**
 * finite tells whether v is a finite number.
 * @param {unknown} v
 * @returns {v is number}
 */
export function finite(v) {
  return typeof v === "number" && Number.isFinite(v);
}

/**
 * latLonText is a position in decimal degrees with hemispheres.
 * @param {number} lat @param {number} lon
 */
export function latLonText(lat, lon) {
  const ns = lat >= 0 ? "N" : "S";
  const ew = lon >= 0 ? "E" : "W";
  return `${Math.abs(lat).toFixed(4)}° ${ns}, ${Math.abs(lon).toFixed(4)}° ${ew}`;
}

const pad = (/** @type {number} */ n) => String(n).padStart(2, "0");

/**
 * utcText is a time as "YYYY-MM-DD HH:MM:SS UTC".
 * @param {number} ms Unix milliseconds
 */
export function utcText(ms) {
  const d = new Date(ms);
  return `${d.getUTCFullYear()}-${pad(d.getUTCMonth() + 1)}-${pad(d.getUTCDate())} ${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}:${pad(d.getUTCSeconds())} UTC`;
}

/**
 * agoText is how long ago a time was, coarsely ("just now", "5 min ago").
 * @param {number} ms @param {number} now
 */
export function agoText(ms, now) {
  const s = Math.max(0, Math.round((now - ms) / 1000));
  if (s < 60) return "just now";
  const m = Math.round(s / 60);
  if (m < 60) return `${m} min ago`;
  const h = Math.floor(m / 60);
  if (h < 48) return `${h} h ${pad(m % 60)} min ago`;
  return `${Math.floor(h / 24)} days ago`;
}
