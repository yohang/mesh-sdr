// Day/night terminator of the map (MAP-006, ADR 0029: in-house, no
// plugin): the position of the Sun from the low-precision formulas of the
// Astronomical Almanac (about 0.01° until 2050, far below what a map shows),
// then the line where it sets, closed over the pole in darkness.

const RAD = Math.PI / 180;
const STEP_DEG = 2;

/**
 * sun returns the declination of the Sun and the longitude where it is
 * overhead, in degrees.
 * @param {number} ms Unix milliseconds
 */
function sun(ms) {
  const n = ms / 86400000 - 10957.5; // days since J2000.0
  const L = 280.46 + 0.9856474 * n; // mean longitude
  const g = (357.528 + 0.9856003 * n) * RAD; // mean anomaly
  const lambda = (L + 1.915 * Math.sin(g) + 0.02 * Math.sin(2 * g)) * RAD; // ecliptic longitude
  const epsilon = (23.439 - 0.0000004 * n) * RAD; // obliquity
  const dec = Math.asin(Math.sin(epsilon) * Math.sin(lambda));
  const ra = Math.atan2(Math.cos(epsilon) * Math.sin(lambda), Math.cos(lambda));
  const gmst = 280.46061837 + 360.98564736629 * n; // degrees
  const lon = ((((ra / RAD - gmst) % 360) + 540) % 360) - 180;
  return { dec: dec / RAD, lon };
}

/**
 * nightPolygon returns the night side of the Earth at a time as one ring of
 * [lat, lon], over two turns of longitude (-360° to 360°) so it also covers
 * the world copies of a zoomed-out map.
 * @param {number} ms Unix milliseconds
 * @returns {[number, number][]}
 */
export function nightPolygon(ms) {
  const s = sun(ms);
  // At an equinox tan(dec) is 0: a tiny declination keeps the line finite.
  const tanDec = Math.tan((Math.abs(s.dec) < 1e-6 ? 1e-6 : s.dec) * RAD);
  /** @type {[number, number][]} */
  const ring = [];
  for (let lon = -360; lon <= 360; lon += STEP_DEG) {
    // The Sun is on the horizon where tan(lat) = -cos(hour angle) / tan(dec).
    ring.push([Math.atan(-Math.cos((lon - s.lon) * RAD) / tanDec) / RAD, lon]);
  }
  // The pole away from the Sun is in darkness.
  const pole = s.dec > 0 ? -90 : 90;
  ring.push([pole, 360], [pole, -360]);
  return ring;
}
