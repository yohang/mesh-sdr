// UTC clock in the top bar (UI-006). Not a live region: it would spam screen readers.
export class UtcClock extends HTMLElement {
  #timer = 0;

  connectedCallback() {
    this.#tick();
    this.#timer = window.setInterval(() => this.#tick(), 1000);
  }

  disconnectedCallback() {
    window.clearInterval(this.#timer);
  }

  #tick() {
    const now = new Date();
    const time = this.querySelector("time") ?? this.appendChild(document.createElement("time"));
    time.dateTime = now.toISOString();
    time.textContent = `${now.toISOString().slice(11, 19)} UTC`;
  }
}
