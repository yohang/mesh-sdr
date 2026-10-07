// Waterfall palettes (UI-013) as 256-entry RGBA tables. M1a ships two
// schemes; device.config names the one in use (waterfall.scheme), unknown
// names fall back to "default".

/** @type {Record<string, number[][]>} */
const STOPS = {
  default: [
    [0, 0, 0x20],
    [0, 0, 0x80],
    [0x40, 0, 0xa0],
    [0xc0, 0x20, 0x40],
    [0xff, 0xa0, 0],
    [0xff, 0xff, 0xa0],
  ],
  turbo: [
    [0x30, 0x12, 0x3b],
    [0x46, 0x86, 0xfb],
    [0x1a, 0xe4, 0xb6],
    [0xa2, 0xfc, 0x3c],
    [0xfa, 0xba, 0x39],
    [0xe4, 0x46, 0x0b],
    [0x7a, 0x04, 0x03],
  ],
};

/**
 * paletteRGBA returns 256 × RGBA bytes.
 * @param {string} name
 */
export function paletteRGBA(name) {
  const stops = STOPS[name] ?? STOPS.default;
  const out = new Uint8Array(256 * 4);
  for (let i = 0; i < 256; i++) {
    const t = (i / 255) * (stops.length - 1);
    const k = Math.min(Math.floor(t), stops.length - 2);
    const f = t - k;
    for (let c = 0; c < 3; c++) {
      out[i * 4 + c] = Math.round(stops[k][c] + (stops[k + 1][c] - stops[k][c]) * f);
    }
    out[i * 4 + 3] = 255;
  }
  return out;
}

/**
 * levelLUT maps a quantised value q (u8 dB) to a palette colour for the
 * levels [minDb, maxDb], packed as little-endian ABGR for a Uint32Array view
 * of ImageData.
 * @param {Uint8Array} rgba @param {number} dbMin @param {number} dbStep
 * @param {number} minDb @param {number} maxDb
 */
export function levelLUT(rgba, dbMin, dbStep, minDb, maxDb) {
  const lut = new Uint32Array(256);
  const range = Math.max(1, maxDb - minDb);
  for (let q = 0; q < 256; q++) {
    const db = dbMin + q * dbStep;
    const t = Math.min(1, Math.max(0, (db - minDb) / range));
    const i = Math.round(t * 255) * 4;
    lut[q] = ((rgba[i + 3] << 24) | (rgba[i + 2] << 16) | (rgba[i + 1] << 8) | rgba[i]) >>> 0;
  }
  return lut;
}
