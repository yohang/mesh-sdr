// <msdr-audio-dock>: the shell-level audio controls, a row of the top bar
// outside #main (ADR 0015 decision 9). It stays in the DOM across boosted
// navigation, so the listener can start or stop the audio (RX-004), mute it
// (shortcuts M and Space) and set the volume (RX-026; Alt or Ctrl + ↑ ↓,
// and the wheel on the slider, RX-033) from any page. It shows only while
// the engine listens to a device. Not a live region: the receiver page
// announces the connection states.

import { syncTopics } from "../events.js";
import { registerShortcuts } from "../shortcuts.js";
import { wheelRanges } from "../wheel-range.js";
import { BUTTON } from "./dom.js";
import { getEngine } from "./engine.js";

// Volume step of the keyboard (0..1).
const VOLUME_STEP = 0.05;

class MsdrAudioDock extends HTMLElement {
  connectedCallback() {
    const e = getEngine();
    this.name = document.createElement("span");
    this.name.className = "min-w-0 truncate text-fg-muted";

    this.play = document.createElement("button");
    this.play.type = "button";
    this.play.className = BUTTON;
    this.play.addEventListener("click", () => (e.audio.running ? e.stopAudio() : e.startAudio()));

    this.mute = document.createElement("button");
    this.mute.type = "button";
    this.mute.className = BUTTON;
    this.mute.textContent = "Mute";
    this.mute.setAttribute("aria-keyshortcuts", "M Space");
    this.mute.addEventListener("click", () => e.setMuted(!e.audio.muted));

    const label = document.createElement("label");
    label.className = "flex items-center gap-2";
    const text = document.createElement("span");
    text.textContent = "Volume";
    this.volume = document.createElement("input");
    this.volume.type = "range";
    this.volume.id = "audio-dock-volume";
    this.volume.name = "volume";
    this.volume.min = "0";
    this.volume.max = "100";
    this.volume.className = "w-24 accent-accent max-md:min-h-11 pointer-coarse:min-h-11";
    this.volume.addEventListener("input", () => e.setVolume(Number(this.volume?.value) / 100));
    label.append(text, this.volume);

    const row = document.createElement("div");
    row.className = "mx-auto flex max-w-screen-2xl flex-wrap items-center gap-x-3 gap-y-1 px-4 py-1 text-sm";
    row.append(this.play, this.mute, label, this.name);
    this.replaceChildren(row);
    this.stopWheel = wheelRanges(row);

    // Shortcuts on every page while listening (UI-014).
    const listening = () => !!e.target;
    /** @param {number} dir */
    const volume = (dir) => e.setVolume(Math.round((e.audio.volume + dir * VOLUME_STEP) * 100) / 100);
    this.stopKeys = registerShortcuts([
      { keys: ["m", "Space"], label: "Mute or unmute", group: "Audio", enabled: listening, run: () => e.setMuted(!e.audio.muted) },
      { keys: ["Alt+ArrowUp", "Ctrl+ArrowUp"], label: "Volume up", group: "Audio", repeat: true, enabled: listening, run: () => volume(1) },
      { keys: ["Alt+ArrowDown", "Ctrl+ArrowDown"], label: "Volume down", group: "Audio", repeat: true, enabled: listening, run: () => volume(-1) },
    ]);

    this.onChange = () => this.render();
    e.addEventListener("change", this.onChange);
    this.render();
  }

  disconnectedCallback() {
    if (this.onChange) getEngine().removeEventListener("change", this.onChange);
    this.stopWheel?.();
    this.stopKeys?.();
  }

  render() {
    const e = getEngine();
    const a = e.audio;
    this.hidden = !e.target;
    // While listening, the grid state (receiver/grid.js) stays live on
    // every page: node offline and back (GRID-021), notices (UI-011).
    const topics = e.target ? "nodes devices" : "";
    if ((this.dataset.msdrTopics ?? "") !== topics) {
      if (topics) this.dataset.msdrTopics = topics;
      else delete this.dataset.msdrTopics;
      syncTopics();
    }
    if (!this.play || !this.mute || !this.volume || !this.name) return;
    this.play.textContent = a.running ? "Stop audio" : "Start audio";
    this.mute.setAttribute("aria-pressed", String(a.muted));
    this.volume.value = String(Math.round(a.volume * 100));
    this.volume.setAttribute("aria-valuetext", `${this.volume.value} %`);
    this.name.textContent = e.target ? e.target.name : "";
  }
}

if (!customElements.get("msdr-audio-dock")) {
  customElements.define("msdr-audio-dock", MsdrAudioDock);
}
