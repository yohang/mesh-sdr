// UTC clock of the top bar (UI-006, RX-034): <msdr-utc-clock> wraps the
// server-rendered <time>, so the clock shows without JS, and updates it on
// every minute boundary. It is deliberately not a live region (ADR 0003):
// a ticking clock would be announced every minute.

const pad = (n) => String(n).padStart(2, "0");

class UTCClock extends HTMLElement {
  connectedCallback() {
    this.tick();
  }

  disconnectedCallback() {
    clearTimeout(this.timer);
  }

  // tick renders the current time and schedules the next minute boundary.
  tick() {
    const now = new Date();
    const time = this.querySelector("time");
    if (time) {
      const hm = `${pad(now.getUTCHours())}:${pad(now.getUTCMinutes())}`;
      time.dateTime = `${now.toISOString().slice(0, 16)}Z`;
      time.textContent = hm;
    }
    clearTimeout(this.timer);
    this.timer = setTimeout(() => this.tick(), 60_000 - (now.getTime() % 60_000) + 50);
  }
}

if (!customElements.get("msdr-utc-clock")) {
  customElements.define("msdr-utc-clock", UTCClock);
}
