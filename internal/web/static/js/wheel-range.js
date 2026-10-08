// Range sliders step with the mouse wheel (RX-033): one listener on a
// container serves every <input type="range"> inside it, present or added
// later. Wheel up or right steps up. The slider then fires "input" and
// "change" as if it had been dragged, so its owner needs no other code.

/**
 * wheelRanges makes the range sliders inside root follow the wheel; the
 * returned function removes the listener.
 * @param {HTMLElement} root
 * @returns {() => void}
 */
export function wheelRanges(root) {
  /** @param {WheelEvent} ev */
  const onWheel = (ev) => {
    const input = ev.target instanceof Element ? ev.target.closest('input[type="range"]') : null;
    if (!(input instanceof HTMLInputElement) || input.disabled || !root.contains(input)) return;
    const delta = Math.abs(ev.deltaY) >= Math.abs(ev.deltaX) ? -ev.deltaY : ev.deltaX;
    if (delta === 0) return;
    ev.preventDefault();
    const before = input.value;
    if (delta > 0) input.stepUp();
    else input.stepDown();
    if (input.value === before) return;
    input.dispatchEvent(new Event("input", { bubbles: true }));
    input.dispatchEvent(new Event("change", { bubbles: true }));
  };
  root.addEventListener("wheel", onWheel, { passive: false });
  return () => root.removeEventListener("wheel", onWheel);
}
