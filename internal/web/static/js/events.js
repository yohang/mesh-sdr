// Hub events dispatcher (TECHNICAL_SPEC §6.6, ADR 0016, ADR 0018).
//
// One rx.v1 WebSocket per tab to /api/ws, owned by the shell (outside #main,
// so it survives boosted navigation). It turns each hub event into a DOM
// event on <body> named "msdr:<type>" (for example "msdr:node.status"), with
// detail {payload, ts}. Pages never read the socket: a live fragment
// declares the topics it needs and refetches itself over HTML:
//
//   <div id="nodes-table" data-msdr-topics="nodes" hx-get="/admin/nodes"
//        hx-trigger="msdr:node.status from:body delay:1s, msdr:resync from:body"
//        hx-swap="outerMorph">
//
// Topics come from the data-msdr-topics attributes present in the document;
// they are (un)subscribed after every swap, and the socket opens only when
// the document declares a topic. "msdr:resync" is fired after each
// subscription is acknowledged (first load, navigation, reconnection), since
// events may have been missed meanwhile. Events carry plain JSON and are
// never inserted as HTML here (§6.1).
//
// Requests triggered by these events are marked X-Msdr-Background: they are
// not activity of the user, so an open tab does not keep its session alive.
//
// Other events fired on <body>: "msdr:events.state" ({state, code?}),
// "msdr:events.error", "msdr:session.revoked" and "msdr:session.expired"
// (the shell then shows its "session ended" notice).

const PREFIX = "msdr:";
const PATH = "/api/ws";
const SUBPROTOCOL = "rx.v1";
const BACKGROUND_HEADER = "X-Msdr-Background";
const MIN_DELAY = 1000;
const MAX_DELAY = 30000;
// After this many connections in a row that never got a session.welcome,
// the dispatcher stops and the shell offers a manual reload.
const MAX_FAILED_HANDSHAKES = 5;

// Close codes after which reconnecting cannot help (§6.2 client behaviour).
const NO_RETRY = new Set([1000, 1003, 1008, 1009, 4400, 4403]);

let socket = null;
let ready = false; // session.welcome received
let subscribed = new Set();
let pendingSubs = new Map(); // sub request id → its topics, until acked
let denied = new Set(); // topics refused on this connection: not asked again
let attempt = 0;
let failedHandshakes = 0;
let retryAfter = 0;
let timer = null;
let stopped = false;
let seq = 0;

function fire(name, detail) {
  const type = PREFIX + name;
  if (window.htmx?.trigger) {
    window.htmx.trigger(document.body, type, detail);
  } else {
    document.body.dispatchEvent(new CustomEvent(type, { bubbles: true, detail }));
  }
}

// wanted returns the topics declared by the current document.
function wanted() {
  const topics = new Set();
  for (const el of document.querySelectorAll("[data-msdr-topics]")) {
    for (const t of el.dataset.msdrTopics.split(/\s+/)) {
      if (t) {
        topics.add(t);
      }
    }
  }
  return topics;
}

function send(type, payload, id) {
  const env = { v: 1, type, ts: Date.now(), payload };
  if (id) {
    env.id = id;
  }
  socket.send(JSON.stringify(env));
}

// sync (un)subscribes to match the document; it connects lazily on the first
// topic.
function sync() {
  const want = wanted();
  if (want.size > 0 && socket === null && timer === null && !stopped) {
    connect();
    return;
  }
  if (!ready) {
    return;
  }
  const add = [...want].filter((t) => !subscribed.has(t) && !denied.has(t));
  const drop = [...subscribed].filter((t) => !want.has(t));
  if (drop.length > 0) {
    send("unsub", { topics: drop });
    drop.forEach((t) => subscribed.delete(t));
  }
  if (add.length > 0) {
    const id = `s-${++seq}`;
    pendingSubs.set(id, add);
    send("sub", { topics: add }, id);
    add.forEach((t) => subscribed.add(t));
  }
}

function connect() {
  const url = new URL(PATH, location.href);
  url.protocol = location.protocol === "https:" ? "wss:" : "ws:";
  socket = new WebSocket(url, SUBPROTOCOL);
  fire("events.state", { state: "connecting" });

  socket.addEventListener("open", () => {
    send("session.hello", { client: { name: "meshsdr-web", version: "1" } }, `h-${++seq}`);
  });
  socket.addEventListener("message", (msg) => onMessage(msg.data));
  socket.addEventListener("close", (evt) => onClose(evt.code));
}

function onMessage(data) {
  let env;
  try {
    env = JSON.parse(data);
  } catch {
    return;
  }
  if (env?.v !== 1 || typeof env.type !== "string" || typeof env.payload !== "object" || env.payload === null) {
    return;
  }

  switch (env.type) {
    case "session.welcome":
      ready = true;
      attempt = 0;
      failedHandshakes = 0;
      subscribed = new Set();
      pendingSubs = new Map();
      denied = new Set();
      fire("events.state", { state: "open" });
      sync();
      return;
    case "ack":
      if (pendingSubs.delete(env.payload.re)) {
        // Events may have been missed before the subscription: refetch.
        fire("resync", {});
      }
      return;
    case "error": {
      // A refused sub subscribed none of its topics (all-or-nothing).
      const refused = pendingSubs.get(env.payload.re);
      if (refused) {
        pendingSubs.delete(env.payload.re);
        refused.forEach((t) => subscribed.delete(t));
        if (env.payload.code === "forbidden") {
          // The refused topic is not asked again on this connection; the
          // others are, without it.
          const bad = env.payload.details?.topic;
          for (const t of refused) {
            if (!bad || t === bad || refused.length === 1) {
              denied.add(t);
            }
          }
          queueMicrotask(sync);
        }
      }
      if (env.payload.code === "rate_limited") {
        retryAfter = env.payload.retry_after_ms ?? 0;
      }
      // Topics the hub dropped (listen policy change) are named.
      if (env.payload.code === "forbidden" && Array.isArray(env.payload.details?.topics)) {
        env.payload.details.topics.forEach((t) => {
          subscribed.delete(t);
          denied.add(t);
        });
      }
      fire("events.error", { payload: env.payload });
      return;
    }
    case "session.revoked":
      stopped = true;
      fire("session.revoked", { payload: env.payload });
      return;
    default:
      fire(env.type, { payload: env.payload, ts: env.ts });
  }
}

function onClose(code) {
  if (!ready) {
    failedHandshakes++;
  }
  socket = null;
  ready = false;
  subscribed = new Set();
  pendingSubs = new Map();
  fire("events.state", { state: "closed", code });

  if (code === 4401) {
    stopped = true;
    fire("session.expired", {});
    return;
  }
  if (code === 4426) {
    stopped = true; // protocol upgrade: the app must be reloaded
    return;
  }
  if (stopped || NO_RETRY.has(code)) {
    return;
  }
  if (failedHandshakes >= MAX_FAILED_HANDSHAKES) {
    stopped = true;
    document.getElementById("msdr-events-stopped")?.removeAttribute("hidden");
    fire("events.state", { state: "stopped", code });
    return;
  }

  // Exponential back-off with full jitter; 4429 waits at least retry_after_ms.
  const cap = Math.min(MAX_DELAY, MIN_DELAY * 2 ** attempt++);
  const delay = Math.max(retryAfter, Math.random() * cap);
  retryAfter = 0;
  timer = setTimeout(() => {
    timer = null;
    if (wanted().size > 0) {
      connect();
    }
  }, delay);
}

// showSessionEnded reveals the shell notice: reconnecting would silently
// continue as an anonymous visitor.
function showSessionEnded() {
  document.getElementById("msdr-session-ended")?.removeAttribute("hidden");
}

// installEvents watches swaps (navigation, fragments, history restores) to
// keep the subscriptions in line with the document, and marks the requests
// the hub events trigger as background requests.
export function installEvents() {
  document.addEventListener("htmx:after:swap", sync);
  window.addEventListener("pageshow", sync);
  document.addEventListener("htmx:config:request", (evt) => {
    const ctx = evt.detail?.ctx;
    if (ctx?.sourceEvent?.type?.startsWith(PREFIX) && ctx.request?.headers) {
      ctx.request.headers[BACKGROUND_HEADER] = "1";
    }
  });
  document.body.addEventListener(PREFIX + "session.revoked", showSessionEnded);
  document.body.addEventListener(PREFIX + "session.expired", showSessionEnded);
  sync();
}
