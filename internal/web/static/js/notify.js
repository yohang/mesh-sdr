// Notifications (UI-011, RX-038): transient toasts and the top bar list of
// the session's notifications. Shell-level, outside #main: toasts survive
// boosted navigation, and the list lives in sessionStorage (memory when
// storage is blocked), so it lasts for the browser session.
//
// Toasts sit top-right on desktop and at the bottom on mobile (CSS
// .toast-region). Errors go to an assertive live region and stay until
// dismissed; info and warnings go to a polite one and leave after
// INFO_MS, a delay paused while the pointer or the focus is on the toast.
// Each toast names its level in text, so color is never the only cue. Text
// is always set with textContent (§6.1: server messages are not HTML).
//
// API: notify({level, text}) adds a notification; notifications() lists
// them, newest first; notices fires "change" when the list changed.
// Sources: the receiver (shared preset or centre changed by an operator,
// device or node offline and back, node messages), the session ending (hub
// events), and anything else that calls notify.

const STORAGE_KEY = "msdr.notifications";
const MAX_KEPT = 50;
const MAX_TOASTS = 4;
// Info toasts stay at least 5 s (UI-011).
const INFO_MS = 6000;
// The same text within this delay is one notification.
const DEDUPE_MS = 3000;
const LABELS = { error: "Error", warning: "Warning", info: "Info" };

/**
 * @typedef {object} Notice
 * @property {number} id
 * @property {"error" | "warning" | "info"} level
 * @property {string} text
 * @property {number} at ms since the epoch
 */

/** notices fires "change" when the list of notifications changed. */
export const notices = new EventTarget();

/** @type {Notice[]} */
let memory = [];
let unread = 0;
let seq = 0;

/** @returns {Notice[]} */
function load() {
  try {
    const raw = sessionStorage.getItem(STORAGE_KEY);
    const list = raw ? JSON.parse(raw) : [];
    return Array.isArray(list) ? list : [];
  } catch {
    return memory;
  }
}

/** @param {Notice[]} list */
function save(list) {
  memory = list;
  try {
    sessionStorage.setItem(STORAGE_KEY, JSON.stringify(list));
  } catch {
    // Storage blocked: the list lives in memory for this page.
  }
}

/** notifications returns the session's notifications, newest first. */
export function notifications() {
  return load();
}

/**
 * notify adds a notification and shows it as a toast.
 * @param {{level?: "error" | "warning" | "info", text: string}} n
 */
export function notify({ level = "info", text }) {
  if (!text) return;
  const list = load();
  const now = Date.now();
  if (list[0] && list[0].text === text && list[0].level === level && now - list[0].at < DEDUPE_MS) return;
  const id = now * 1000 + (seq++ % 1000);
  list.unshift({ id, level, text, at: now });
  save(list.slice(0, MAX_KEPT));
  unread++;
  toast(level, text);
  renderList();
  notices.dispatchEvent(new Event("change"));
}

/**
 * el creates an element with a class and optional text.
 * @param {string} tag @param {string} cls @param {string} [text]
 */
function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

/** @param {string} level @param {string} text */
function toast(level, text) {
  const region = document.getElementById(level === "error" ? "msdr-toasts-alert" : "msdr-toasts-polite");
  if (!region) return;

  const box = el("div", `toast toast-${level}`);
  const p = el("p", "min-w-0 flex-1 break-words");
  p.append(el("strong", "", `${LABELS[level] ?? "Info"}: `), document.createTextNode(text));
  const close = el("button", "toast-close", "×");
  close.type = "button";
  close.setAttribute("aria-label", "Dismiss notification");
  box.append(p, close);

  /** @type {number} */
  let timer = 0;
  const dismiss = () => {
    clearTimeout(timer);
    box.remove();
  };
  close.addEventListener("click", dismiss);
  if (level !== "error") {
    const arm = () => {
      clearTimeout(timer);
      timer = setTimeout(dismiss, INFO_MS);
    };
    const pause = () => clearTimeout(timer);
    box.addEventListener("mouseenter", pause);
    box.addEventListener("focusin", pause);
    box.addEventListener("mouseleave", () => {
      if (!box.contains(document.activeElement)) arm();
    });
    box.addEventListener("focusout", (e) => {
      if (!box.contains(/** @type {Node | null} */ (e.relatedTarget)) && !box.matches(":hover")) arm();
    });
    arm();
  }

  region.append(box);
  // Too many toasts: the oldest leave first, errors last (they stay listed
  // under the bell).
  for (;;) {
    const all = [...document.querySelectorAll("#msdr-toasts .toast")].filter((t) => t !== box);
    if (all.length < MAX_TOASTS) break;
    (all.find((t) => !t.classList.contains("toast-error")) ?? all[0]).remove();
  }
}

/** @param {number} at */
function clock(at) {
  return new Date(at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
}

// renderList fills the top bar list and the bell's unread count.
function renderList() {
  const list = load();
  const ol = document.getElementById("msdr-notifications-list");
  const empty = document.getElementById("msdr-notifications-empty");
  const clear = document.getElementById("msdr-notifications-clear");
  if (ol) {
    ol.replaceChildren(
      ...list.map((n) => {
        const li = el("li", `notice notice-${n.level}`);
        const time = el("time", "block text-xs text-fg-muted", clock(n.at));
        time.setAttribute("datetime", new Date(n.at).toISOString());
        const p = el("p", "break-words");
        p.append(el("strong", "", `${LABELS[n.level] ?? "Info"}: `), document.createTextNode(n.text));
        li.append(time, p);
        return li;
      }),
    );
  }
  if (empty) empty.hidden = list.length > 0;
  if (clear) clear.hidden = list.length === 0;

  const bell = document.getElementById("msdr-bell");
  const count = document.getElementById("msdr-bell-count");
  if (bell) bell.setAttribute("aria-label", unread > 0 ? `Notifications, ${unread} unread` : "Notifications");
  if (count) {
    count.textContent = unread > 9 ? "9+" : String(unread);
    count.hidden = unread === 0;
  }
}

// installNotifications wires the bell list and the session notices.
export function installNotifications() {
  const popover = document.getElementById("msdr-notifications");
  popover?.addEventListener("toggle", (e) => {
    if (/** @type {ToggleEvent} */ (e).newState === "open") {
      unread = 0;
      renderList();
    }
  });
  document.getElementById("msdr-notifications-clear")?.addEventListener("click", () => {
    save([]);
    unread = 0;
    renderList();
    notices.dispatchEvent(new Event("change"));
  });
  const ended = () => notify({ level: "error", text: "Your session has ended. Reload the page to continue." });
  document.body.addEventListener("msdr:session.revoked", ended);
  document.body.addEventListener("msdr:session.expired", ended);
  document.body.addEventListener("msdr:events.state", (e) => {
    if (/** @type {CustomEvent} */ (e).detail?.state === "stopped") {
      notify({ level: "error", text: "Live updates stopped: the hub could not be reached." });
    }
  });
  renderList();
}
