// Tune-by-squelch (RX-025): "[" and "]" walk the peak-hold FFT from the
// tuned frequency to the next signal above the squelch level minus
// SEEK_MARGIN_DB, and tune to its strongest bin.

export const SEEK_MARGIN_DB = 13;

/**
 * seekBin returns the strongest bin of the next run of bins above
 * threshold, walking from bin `from` in direction dir, or -1. The run the
 * walk starts in (the signal tuned now) is skipped first.
 * @param {ArrayLike<number>} peak peak-hold levels, one per bin
 * @param {number} from start bin
 * @param {1 | -1} dir
 * @param {number} threshold level, in the units of peak
 * @returns {number}
 */
export function seekBin(peak, from, dir, threshold) {
  const n = peak.length;
  let i = Math.min(n - 1, Math.max(0, Math.round(from)));
  // Leave the signal tuned now.
  while (i >= 0 && i < n && peak[i] > threshold) i += dir;
  // Find the next one.
  while (i >= 0 && i < n && peak[i] <= threshold) i += dir;
  if (i < 0 || i >= n) return -1;
  let best = i;
  while (i >= 0 && i < n && peak[i] > threshold) {
    if (peak[i] > peak[best]) best = i;
    i += dir;
  }
  return best;
}
