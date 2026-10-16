// Great circle lines of the map (MAP-011, ADR 0029: in-house, no plugin):
// the shortest path between two positions on the sphere, as polylines
// split where they cross the antimeridian, so a call between Europe and
// New Zealand is not drawn across the whole map.

const RAD = Math.PI / 180;
const STEP_DEG = 2; // one point every 2° of arc

/**
 * @param {number} lat @param {number} lon
 * @returns {[number, number, number]} the unit vector of a position
 */
function vector(lat, lon) {
  return [Math.cos(lat * RAD) * Math.cos(lon * RAD), Math.cos(lat * RAD) * Math.sin(lon * RAD), Math.sin(lat * RAD)];
}

/**
 * greatCircle returns the great circle path from a to b ([lat, lon] in
 * degrees) as one polyline, or two when it crosses the antimeridian.
 * @param {[number, number]} a @param {[number, number]} b
 * @returns {[number, number][][]}
 */
export function greatCircle(a, b) {
  const p = vector(a[0], a[1]);
  const q = vector(b[0], b[1]);
  const cross = Math.hypot(p[1] * q[2] - p[2] * q[1], p[2] * q[0] - p[0] * q[2], p[0] * q[1] - p[1] * q[0]);
  const d = Math.atan2(cross, p[0] * q[0] + p[1] * q[1] + p[2] * q[2]);
  // Same place, or antipodes (any great circle joins them): a straight line.
  if (d < 1e-9 || Math.PI - d < 1e-9) return [[a, b]];

  const n = Math.max(1, Math.ceil(d / RAD / STEP_DEG));
  /** @type {[number, number][]} */
  const points = [];
  for (let i = 0; i <= n; i++) {
    const f = i / n;
    const s = Math.sin((1 - f) * d) / Math.sin(d);
    const t = Math.sin(f * d) / Math.sin(d);
    const x = s * p[0] + t * q[0];
    const y = s * p[1] + t * q[1];
    const z = s * p[2] + t * q[2];
    points.push([Math.atan2(z, Math.hypot(x, y)) / RAD, Math.atan2(y, x) / RAD]);
  }
  points[0] = a;
  points[n] = b;
  return splitAntimeridian(points);
}

/**
 * splitAntimeridian cuts a path where a step jumps across ±180°, at the
 * latitude where it crosses it.
 * @param {[number, number][]} points
 * @returns {[number, number][][]}
 */
function splitAntimeridian(points) {
  /** @type {[number, number][][]} */
  const out = [[points[0]]];
  for (let i = 1; i < points.length; i++) {
    const [lat0, lon0] = points[i - 1];
    const [lat1, lon1] = points[i];
    if (Math.abs(lon1 - lon0) > 180) {
      const edge = lon0 > 0 ? 180 : -180;
      const unwrapped = lon1 + (edge > 0 ? 360 : -360);
      const lat = lat0 + ((edge - lon0) / (unwrapped - lon0)) * (lat1 - lat0);
      out[out.length - 1].push([lat, edge]);
      out.push([[lat, -edge]]);
    }
    out[out.length - 1].push(points[i]);
  }
  return out;
}
