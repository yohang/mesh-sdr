// Receiver island: reads its bootstrap from a templ.JSONScript element and renders it
// with textContent only (untrusted names are never parsed as HTML).
export class ReceiverIsland extends HTMLElement {
  connectedCallback() {
    const source = document.getElementById(this.dataset.bootstrap ?? "");
    if (!source) {
      return;
    }

    const data = JSON.parse(source.textContent ?? "{}");
    const list = document.createElement("ul");
    list.className = "flex flex-col gap-1";
    for (const device of data.devices ?? []) {
      const item = document.createElement("li");
      const mhz = (device.centerFrequencyHz / 1e6).toFixed(3);
      item.textContent = `${device.name} (${device.node}) · ${mhz} MHz`;
      list.append(item);
    }
    this.replaceChildren(list);
  }
}
