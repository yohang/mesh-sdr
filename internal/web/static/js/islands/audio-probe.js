// SPIKE: audio continuity probe. Stands in for the receiver audio engine, which must
// live outside #main so that client-side navigation never interrupts it (UI-006).
export class AudioProbe extends HTMLElement {
  #ctx = null;
  #started = 0;
  #timer = 0;
  #button = null;
  #status = null;

  connectedCallback() {
    this.#button = document.createElement("button");
    this.#button.type = "button";
    this.#button.className = "rounded border border-border px-2 py-0.5 text-sm";
    this.#button.textContent = "Test tone";
    this.#button.setAttribute("aria-pressed", "false");
    this.#button.addEventListener("click", () => this.#toggle());

    this.#status = document.createElement("span");
    this.#status.className = "ml-2 text-sm text-fg-muted tabular-nums";

    this.replaceChildren(this.#button, this.#status);
  }

  disconnectedCallback() {
    this.#stop();
  }

  async #toggle() {
    if (this.#ctx) {
      this.#stop();
      return;
    }

    this.#ctx = new AudioContext();
    const osc = this.#ctx.createOscillator();
    const gain = this.#ctx.createGain();
    gain.gain.value = 0.02;
    osc.frequency.value = 440;
    osc.connect(gain).connect(this.#ctx.destination);
    osc.start();
    this.#started = performance.now();
    this.#timer = window.setInterval(() => this.#render(), 500);
    this.#button.setAttribute("aria-pressed", "true");
    this.#render();
  }

  #stop() {
    window.clearInterval(this.#timer);
    this.#ctx?.close();
    this.#ctx = null;
    this.#button?.setAttribute("aria-pressed", "false");
    if (this.#status) {
      this.#status.textContent = "";
    }
  }

  #render() {
    const seconds = Math.floor((performance.now() - this.#started) / 1000);
    this.#status.textContent = `${this.#ctx?.state ?? "stopped"} ${seconds}s`;
  }
}
