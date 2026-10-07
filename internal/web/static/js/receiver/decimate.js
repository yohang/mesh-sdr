// Peak-preserving decimation (ADR 0015 decision 2): a window of FFT bins is
// reduced to at most one value per pixel column, keeping the maximum of each
// group so that narrow carriers survive.

/**
 * decimate writes into out (length = columns) the max of bins[start,
 * start+count) per column. With fewer bins than columns, a bin spans several
 * columns.
 * @param {Uint8Array} bins @param {number} start @param {number} count
 * @param {Uint8Array | Uint32Array} out @param {Uint32Array} [lut] optional
 *   colour lookup applied to each value
 */
export function decimate(bins, start, count, out, lut) {
  const cols = out.length;
  const end = Math.min(bins.length, start + count);
  for (let c = 0; c < cols; c++) {
    const a = start + Math.floor((c * count) / cols);
    const b = Math.min(end, Math.max(a + 1, start + Math.floor(((c + 1) * count) / cols)));
    let m = 0;
    for (let i = a; i < b; i++) {
      if (bins[i] > m) m = bins[i];
    }
    out[c] = lut ? lut[m] : m;
  }
}
