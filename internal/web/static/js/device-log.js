// Device log island (SRC-005): <msdr-device-log data-device="<id>"> wraps
// the server-rendered records of Admin › Devices › {id} › Log and appends
// the live ones, received on the hub events socket as "msdr:device.log"
// body events (topic admin.device_log:device=<id>, declared by the list). Records
// are plain text: they are inserted with textContent only, never as HTML.
// New records are announced in the polite status region at most once per
// ANNOUNCE_EVERY, as a count, so a chatty connector does not flood screen
// readers.

const EVENT = "msdr:device.log";
// Records kept on the page, like the node and the hub.
const MAX_RECORDS = 200;
const ANNOUNCE_EVERY = 10_000;

class DeviceLog extends HTMLElement {
  connectedCallback() {
    this.pending = 0;
    this.timer = null;
    this.lastAnnounce = 0;
    this.onRecords = (evt) => this.receive(evt.detail?.payload);
    document.body.addEventListener(EVENT, this.onRecords);
  }

  disconnectedCallback() {
    document.body.removeEventListener(EVENT, this.onRecords);
    clearTimeout(this.timer);
  }

  // receive appends the records of a device.log event for this device; a
  // reset (the node reconnected and sent its backlog) replaces them.
  receive(payload) {
    if (!payload || payload.device_id !== this.dataset.device || !Array.isArray(payload.records)) {
      return;
    }
    const list = this.querySelector("[data-device-log-records]");
    if (!list) {
      return;
    }
    if (payload.reset) {
      list.replaceChildren();
    }
    for (const rec of payload.records) {
      list.append(recordItem(rec));
    }
    while (list.children.length > MAX_RECORDS) {
      list.firstElementChild.remove();
    }
    const empty = this.querySelector("[data-device-log-empty]");
    if (empty) {
      empty.hidden = list.children.length > 0;
    }
    this.announce(payload.records.length);
  }

  // announce counts the new records and tells them at most once per
  // ANNOUNCE_EVERY.
  announce(count) {
    this.pending += count;
    if (this.pending === 0 || this.timer !== null) {
      return;
    }
    const wait = Math.max(0, this.lastAnnounce + ANNOUNCE_EVERY - Date.now());
    this.timer = setTimeout(() => {
      this.timer = null;
      this.lastAnnounce = Date.now();
      const status = this.querySelector("[data-device-log-status]");
      if (status && this.pending > 0) {
        status.textContent = this.pending === 1 ? "1 new log record." : `${this.pending} new log records.`;
      }
      this.pending = 0;
    }, wait);
  }
}

// recordItem renders one record like the server does: time, [origin], text.
function recordItem(rec) {
  const li = document.createElement("li");
  li.className = "whitespace-pre-wrap break-words";

  const time = document.createElement("time");
  const t = Number(rec.t);
  if (Number.isFinite(t)) {
    time.dateTime = new Date(t).toISOString();
  }
  time.textContent = String(rec.time ?? "");

  const origin = document.createElement("span");
  origin.className = "text-fg-muted";
  const source = String(rec.source ?? "");
  origin.textContent = `[${source === "device" || !rec.class ? source : `${source} ${rec.class}`}]`;

  li.append(time, " ", origin, " ", String(rec.text ?? ""));
  return li;
}

if (!customElements.get("msdr-device-log")) {
  customElements.define("msdr-device-log", DeviceLog);
}
