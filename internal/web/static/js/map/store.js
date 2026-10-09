// Feature store of the map island (MAP-002, MAP-012). It holds the
// features of GET /api/v1/map/features and the map.feature.upsert /
// map.feature.remove deltas, one per device (key <kind>:<subject>@<device>),
// and merges those of the same kind and subject into one entity: the
// newest report wins, with every device that heard it. Receivers (GET
// /api/v1/map/config) are entities too. Features are dropped at their
// expires_at, whether or not the hub's removal arrived.

import { finite, locatorBounds } from "./geo.js";

/**
 * @typedef {object} Feature a map feature (MapFeature of the API)
 * @property {string} key
 * @property {string} kind
 * @property {string} subject
 * @property {string} source
 * @property {string} [device_id]
 * @property {number} [lat]
 * @property {number} [lon]
 * @property {any} geometry
 * @property {Record<string, any>} details
 * @property {number} updated_at
 * @property {number} [expires_at]
 */

/**
 * @typedef {object} Receiver a receiver marker (MapReceiver of the API)
 * @property {string} key
 * @property {string} kind
 * @property {string} name
 * @property {number} lat
 * @property {number} lon
 * @property {string} locator
 * @property {boolean} precise
 * @property {{id: string, name: string}[]} devices
 */

/**
 * @typedef {object} Entity what the map shows once: the features of one
 * kind and subject merged, or a receiver
 * @property {string} id kind:subject, or the receiver key
 * @property {string} kind
 * @property {string} subject
 * @property {Feature | null} feature the newest report (null: receiver)
 * @property {Receiver | null} receiver
 * @property {string[]} devices the devices that report it, newest first
 * @property {number} updated_at
 * @property {number} [expires_at] the latest expiry of its reports
 * @property {[number, number] | null} at its position (lat, lon), or the
 *   middle of a call line
 */

/**
 * valid tells whether a payload has the shape of a feature.
 * @param {any} f
 * @returns {f is Feature}
 */
function valid(f) {
  return (
    f !== null &&
    typeof f === "object" &&
    typeof f.key === "string" &&
    typeof f.kind === "string" &&
    typeof f.subject === "string" &&
    finite(f.updated_at) &&
    f.geometry !== null &&
    typeof f.geometry === "object" &&
    (f.details === undefined || (f.details !== null && typeof f.details === "object"))
  );
}

/**
 * position returns where a feature is drawn: its point, the centre of its
 * locator square, or the middle of its line.
 * @param {Feature} f
 * @returns {[number, number] | null}
 */
function position(f) {
  const g = f.geometry;
  if (g.type === "line") {
    const a = g.from;
    const b = g.to;
    if (!a || !b || ![a.lat, a.lon, b.lat, b.lon].every(finite)) return null;
    return [(a.lat + b.lat) / 2, (a.lon + b.lon) / 2];
  }
  if (finite(f.lat) && finite(f.lon)) return [f.lat, f.lon];
  if (finite(g.lat) && finite(g.lon)) return [g.lat, g.lon];
  if (g.type === "locator") {
    const b = locatorBounds(g.locator);
    if (b) return [(b[0][0] + b[1][0]) / 2, (b[0][1] + b[1][1]) / 2];
  }
  return null;
}

export class Store {
  constructor() {
    /** @type {Map<string, Feature>} features by key */
    this.raw = new Map();
    /** @type {Map<string, Set<string>>} feature keys by entity id */
    this.groups = new Map();
    /** @type {Map<string, Receiver>} receivers by key */
    this.receivers = new Map();
    /** @type {Set<string>} entity ids changed since the last take */
    this.dirty = new Set();
  }

  /**
   * replace sets every feature and receiver (a load).
   * @param {unknown[]} features @param {Receiver[]} receivers
   */
  replace(features, receivers) {
    for (const id of this.ids()) this.dirty.add(id);
    this.raw.clear();
    this.groups.clear();
    this.receivers.clear();
    for (const f of features) this.upsert(f);
    for (const r of receivers) {
      if (r && typeof r.key === "string" && finite(r.lat) && finite(r.lon)) {
        this.receivers.set(r.key, r);
        this.dirty.add(r.key);
      }
    }
  }

  /**
   * upsert adds or updates a feature; an older report than the one held
   * is ignored (a replayed delta).
   * @param {unknown} f
   */
  upsert(f) {
    if (!valid(f)) return;
    const old = this.raw.get(f.key);
    if (old && old.updated_at > f.updated_at) return;
    if (!f.details) f.details = {};
    this.raw.set(f.key, f);
    const id = `${f.kind}:${f.subject}`;
    let g = this.groups.get(id);
    if (!g) {
      g = new Set();
      this.groups.set(id, g);
    }
    g.add(f.key);
    this.dirty.add(id);
  }

  /**
   * remove drops a feature.
   * @param {unknown} key
   */
  remove(key) {
    if (typeof key !== "string") return;
    const f = this.raw.get(key);
    if (!f) return;
    this.raw.delete(key);
    const id = `${f.kind}:${f.subject}`;
    const g = this.groups.get(id);
    g?.delete(key);
    if (g?.size === 0) this.groups.delete(id);
    this.dirty.add(id);
  }

  /**
   * expire drops the features whose expiry passed (MAP-012).
   * @param {number} now Unix milliseconds
   */
  expire(now) {
    for (const f of [...this.raw.values()]) {
      if (finite(f.expires_at) && f.expires_at <= now) this.remove(f.key);
    }
  }

  /** @returns {number | null} the earliest expiry of a feature */
  nextExpiry() {
    let next = null;
    for (const f of this.raw.values()) {
      if (finite(f.expires_at) && (next === null || f.expires_at < next)) next = f.expires_at;
    }
    return next;
  }

  /** @returns {string[]} the ids of every entity */
  ids() {
    return [...this.groups.keys(), ...this.receivers.keys()];
  }

  /** @returns {Set<string>} the changed entity ids, cleared */
  take() {
    const d = this.dirty;
    this.dirty = new Set();
    return d;
  }

  /**
   * entity returns the entity of an id, or null when it is gone.
   * @param {string} id
   * @returns {Entity | null}
   */
  entity(id) {
    const r = this.receivers.get(id);
    if (r) {
      return {
        id, kind: "receiver", subject: r.name, feature: null, receiver: r,
        devices: r.devices.map((d) => d.id), updated_at: 0, at: [r.lat, r.lon],
      };
    }
    const keys = this.groups.get(id);
    if (!keys || keys.size === 0) return null;
    const reports = [...keys].map((k) => /** @type {Feature} */ (this.raw.get(k))).sort((a, b) => b.updated_at - a.updated_at);
    const f = reports[0];
    const expiries = reports.map((x) => x.expires_at);
    /** @type {string[]} */
    const devices = [];
    for (const x of reports) {
      if (x.device_id && !devices.includes(x.device_id)) devices.push(x.device_id);
    }
    return {
      id, kind: f.kind, subject: f.subject, feature: f, receiver: null, devices, updated_at: f.updated_at,
      expires_at: expiries.every(finite) ? Math.max(...expiries) : undefined, at: position(f),
    };
  }
}
