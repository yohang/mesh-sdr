// Grid state of the receiver (RX-040, UI-021, GRID-021, RX-035): the
// devices the visitor may listen to and their nodes, from GET
// /api/v1/features, kept live by the hub events the shell dispatcher fires
// on <body> (events.js): device.status (state, listeners, active preset),
// node.status (online or not, public telemetry) and resync (refetch after a
// (re)subscription). A shell-level singleton, like the engine: the island
// and the notices read it; whoever needs it live declares the "nodes
// devices" topics (the island, the audio dock while listening).
//
// installReceiverNotices turns what happens to the device being listened
// to into notifications (UI-011): node or device offline and back, a
// preset switch or centre move by an operator, node messages, a refused
// token refresh. When the node comes back, the engine reconnects at once.

import { getJSON } from "../csrf.js";
import { notify } from "../notify.js";
import { formatMHz } from "./dom.js";
import { getEngine } from "./engine.js";

const FEATURES_URL = "/api/v1/features";
// Events often come in bursts (a node's devices): one refetch.
const REFRESH_DELAY_MS = 500;
// Device states in which a device cannot be listened to.
const DOWN_STATES = new Set(["failed", "disabled", "unavailable"]);

/**
 * @typedef {object} Device a device of the features summary
 * @property {string} id
 * @property {string} node_id
 * @property {string} name
 * @property {boolean} online
 * @property {boolean} node_online
 * @property {string} state
 * @property {string[]} modes
 * @property {boolean} login_required
 * @property {number} listeners
 * @property {{id: string, name: string}} [active_preset]
 */

/**
 * @typedef {object} Node a node of the features summary
 * @property {string} id
 * @property {string} name
 * @property {boolean} online
 * @property {number} [cpu] busy ratio 0..1
 * @property {number} [temp_c]
 */

/** @param {string} status a node.status status */
function isUp(status) {
  return status === "online" || status === "degraded";
}

/**
 * unavailable returns why a device cannot be listened to, "" when it can.
 * @param {Device} d
 */
export function unavailable(d) {
  if (!d.node_online) return "node offline";
  if (DOWN_STATES.has(d.state)) return `device ${d.state}`;
  return "";
}

class Grid extends EventTarget {
  constructor() {
    super();
    /** @type {Device[]} */
    this.devices = [];
    /** @type {Map<string, Node>} */
    this.nodes = new Map();
    this.loaded = false;
    this.error = false;
    /** @type {Promise<void> | null} */
    this.loading = null;
    this.timer = 0;
    const on = (/** @type {string} */ type, /** @type {(p: any) => void} */ f) =>
      document.body.addEventListener(`msdr:${type}`, (e) => f(/** @type {CustomEvent} */ (e).detail?.payload ?? {}));
    on("device.status", (p) => this.onDevice(p));
    on("node.status", (p) => this.onNode(p));
    document.body.addEventListener("msdr:resync", () => this.refresh());
  }

  emit() {
    this.dispatchEvent(new Event("change"));
  }

  /** load fetches the summary (one request at a time). */
  load() {
    this.loading ??= (async () => {
      try {
        const body = await getJSON(FEATURES_URL);
        this.devices = Array.isArray(body?.devices) ? body.devices : [];
        this.nodes = new Map((Array.isArray(body?.nodes) ? body.nodes : []).map((/** @type {Node} */ n) => [n.id, n]));
        this.error = false;
      } catch {
        this.error = !this.loaded || this.error;
      } finally {
        this.loaded = true;
        this.loading = null;
        this.emit();
      }
    })();
    return this.loading;
  }

  // refresh refetches the summary soon (coalesced).
  refresh() {
    if (!this.loaded) return;
    clearTimeout(this.timer);
    this.timer = setTimeout(() => this.load(), REFRESH_DELAY_MS);
  }

  /** @param {string} nodeId @param {string} id @returns {Device | undefined} */
  device(nodeId, id) {
    return this.devices.find((d) => d.node_id === nodeId && d.id === id);
  }

  /** @param {string} id @returns {Node | undefined} */
  node(id) {
    return this.nodes.get(id);
  }

  /** @param {any} p device.status */
  onDevice(p) {
    const d = this.device(p.node_id, p.device_id);
    // A device this summary does not have (new, or now listenable), or a
    // preset to name: the summary says.
    if (!d || (p.active_preset_id ?? "") !== (d.active_preset?.id ?? "")) {
      this.refresh();
      if (!d) return;
    }
    if (typeof p.state === "string") d.state = p.state;
    if (typeof p.listeners === "number") d.listeners = p.listeners;
    this.emit();
  }

  /** @param {any} p node.status */
  onNode(p) {
    const n = this.nodes.get(p.node_id);
    if (!n) {
      this.refresh();
      return;
    }
    n.online = isUp(p.status);
    for (const k of /** @type {const} */ (["cpu", "temp_c"])) {
      if (n.online && typeof p[k] === "number") n[k] = p[k];
      else delete n[k];
    }
    for (const d of this.devices) {
      if (d.node_id === n.id) d.node_online = n.online;
    }
    // Back online: the device states come with device.status.
    this.emit();
  }
}

/** @type {Grid | null} */
let grid = null;

/** getGrid returns the grid state, created on first use. */
export function getGrid() {
  grid ??= new Grid();
  return grid;
}

// installReceiverNotices notifies what happens to the device listened to.
export function installReceiverNotices() {
  const g = getGrid();
  const e = getEngine();
  /** @type {{key: string, nodeUp: boolean, why: string} | null} */
  let last = null;

  g.addEventListener("change", () => {
    const t = e.target;
    const d = t ? g.device(t.node_id, t.device_id) : undefined;
    if (!t || !d) {
      last = null;
      return;
    }
    const key = `${t.node_id}/${t.device_id}`;
    const node = g.node(t.node_id)?.name || t.node_id;
    const why = unavailable(d);
    if (last && last.key === key) {
      if (last.nodeUp && !d.node_online) {
        notify({ level: "warning", text: `Station ${node} is offline. Reconnecting when it is back.` });
      } else if (!last.nodeUp && d.node_online) {
        notify({ level: "info", text: `Station ${node} is back online.` });
        e.retryNow();
      } else if (d.node_online && why !== last.why) {
        if (why) notify({ level: "warning", text: `${d.name} is unavailable (${why}).` });
        else if (last.why) notify({ level: "info", text: `${d.name} is available again.` });
      }
    }
    last = { key, nodeUp: d.node_online, why };
  });

  e.addEventListener("shared", (ev) => {
    const s = /** @type {CustomEvent} */ (ev).detail;
    const name = e.target?.name ?? "the device";
    if (s.kind === "preset") {
      notify({ level: "info", text: `An operator switched ${name} to the preset ${s.preset || "(unnamed)"}.` });
    } else {
      notify({ level: "info", text: `An operator moved the centre of ${name} to ${formatMHz(s.centerHz)}.` });
    }
  });
  e.addEventListener("message", (ev) => {
    notify({ level: "warning", text: `Node message: ${/** @type {CustomEvent} */ (ev).detail.text}` });
  });
  e.addEventListener("session", () => {
    notify({ level: "error", text: "Your session has ended. Reload the page to continue." });
  });
}
