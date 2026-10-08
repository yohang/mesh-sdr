// Saved pass bands per modulation (RX-021): the last pass band the
// listener set for a mode is restored for each new demodulator in that
// mode; "|" clears them all. Per-session state the browser remembers
// (session.js); the node clamps whatever it receives (DEM-007).

import { getState, setState } from "./session.js";

const NAME = "bandpasses";

/**
 * @typedef {{low_hz: number, high_hz: number}} Bandpass relative to the
 *   tuned frequency, as demod.set sends it
 */

/** @returns {Record<string, Bandpass>} */
function all() {
  return getState(NAME, {});
}

/**
 * savedBandpass returns the pass band saved for mode, or null.
 * @param {string} mode
 * @returns {Bandpass | null}
 */
export function savedBandpass(mode) {
  const b = all()[mode];
  if (!b || !Number.isFinite(b.low_hz) || !Number.isFinite(b.high_hz) || b.low_hz >= b.high_hz) return null;
  return { low_hz: Math.round(b.low_hz), high_hz: Math.round(b.high_hz) };
}

/**
 * saveBandpass remembers the pass band of mode.
 * @param {string} mode @param {number} lowHz @param {number} highHz
 */
export function saveBandpass(mode, lowHz, highHz) {
  if (!mode) return;
  setState(NAME, { ...all(), [mode]: { low_hz: Math.round(lowHz), high_hz: Math.round(highHz) } });
}

/** clearBandpasses forgets every saved pass band. @returns {number} how many */
export function clearBandpasses() {
  const n = Object.keys(all()).length;
  setState(NAME, undefined);
  return n;
}
