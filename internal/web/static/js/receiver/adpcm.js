// IMA ADPCM decoder (§6.7 codec 0x01): state prefix, low nibble first,
// standard tables. Each frame is self-contained.

const STEPS = new Int32Array([
  7, 8, 9, 10, 11, 12, 13, 14, 16, 17, 19, 21, 23, 25, 28, 31, 34, 37, 41, 45, 50, 55, 60, 66, 73, 80, 88, 97, 107,
  118, 130, 143, 157, 173, 190, 209, 230, 253, 279, 307, 337, 371, 408, 449, 494, 544, 598, 658, 724, 796, 876, 963,
  1060, 1166, 1282, 1411, 1552, 1707, 1878, 2066, 2272, 2499, 2749, 3024, 3327, 3660, 4026, 4428, 4871, 5358, 5894,
  6484, 7132, 7845, 8630, 9493, 10442, 11487, 12635, 13899, 15289, 16818, 18500, 20350, 22385, 24623, 27086, 29794,
  32767,
]);
const INDEX = new Int8Array([-1, -1, -1, -1, 2, 4, 6, 8, -1, -1, -1, -1, 2, 4, 6, 8]);

/**
 * decodeADPCM decodes data into out (as int16 values when asInt, else
 * float in [-1, 1)) and returns the number of samples written.
 * @param {number} predictor @param {number} stepIndex @param {Uint8Array} data
 * @param {Float32Array | Int16Array} out
 */
export function decodeADPCM(predictor, stepIndex, data, out) {
  let p = predictor;
  let ix = stepIndex;
  const scale = out instanceof Float32Array ? 1 / 32768 : 1;
  let o = 0;
  for (let i = 0; i < data.length; i++) {
    const b = data[i];
    for (let k = 0; k < 2; k++) {
      const code = k === 0 ? b & 0x0f : b >> 4;
      const step = STEPS[ix];
      let diff = step >> 3;
      if (code & 4) diff += step;
      if (code & 2) diff += step >> 1;
      if (code & 1) diff += step >> 2;
      p += code & 8 ? -diff : diff;
      if (p > 32767) p = 32767;
      else if (p < -32768) p = -32768;
      ix += INDEX[code];
      if (ix < 0) ix = 0;
      else if (ix > 88) ix = 88;
      out[o++] = p * scale;
    }
  }
  return o;
}
