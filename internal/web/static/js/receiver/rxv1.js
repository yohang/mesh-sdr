// rx.v1 client codec (TECHNICAL_SPEC §6.2, §6.7), mirroring
// internal/protocol/rxv1: 24-byte little-endian binary header, FFT u8 dB and
// IMA ADPCM payload prefixes, JSON envelopes. Plain ES module, JSDoc types.

export const HEADER_SIZE = 24;
export const MAGIC = 0xa5;
export const FRAME_VERSION = 1;
export const MAX_FRAME_BYTES = 64 * 1024;

export const FrameType = Object.freeze({ FFT: 0x01, AUDIO: 0x02, FFT2: 0x03, AUDIO_HD: 0x04 });
export const Codec = Object.freeze({ PCM_S16LE: 0x00, ADPCM_IMA: 0x01, OPUS: 0x02, FFT_U8_DB: 0x10, FFT_F32_DB: 0x11 });
export const Flag = Object.freeze({ DISCONTINUITY: 1, RESET: 2, END_OF_STREAM: 4, SQUELCHED: 8 });
const FLAGS_RESERVED = 0xfff0;

/**
 * @typedef {object} Frame
 * @property {number} type
 * @property {number} codec
 * @property {number} streamId
 * @property {number} flags
 * @property {number} seq
 * @property {number} timestampUs  µs since the Unix epoch (fits a double)
 * @property {Uint8Array} payload  view into the received buffer, no copy
 */

/**
 * parseFrame checks the structural rules (length, magic, version,
 * payload_len) and the semantic ones (known type/codec family, reserved
 * flags). It returns null for any frame to drop (§6.7, ADR 0004 decision 3).
 * @param {ArrayBuffer} buf
 * @returns {Frame | null}
 */
export function parseFrame(buf) {
  if (buf.byteLength < HEADER_SIZE || buf.byteLength > MAX_FRAME_BYTES) return null;
  const dv = new DataView(buf);
  if (dv.getUint8(0) !== MAGIC || dv.getUint8(1) !== FRAME_VERSION) return null;
  const len = dv.getUint32(20, true);
  if (len !== buf.byteLength - HEADER_SIZE) return null;
  const type = dv.getUint8(2);
  const codec = dv.getUint8(3);
  const flags = dv.getUint16(6, true);
  const isAudio = type === FrameType.AUDIO || type === FrameType.AUDIO_HD;
  const isFFT = type === FrameType.FFT || type === FrameType.FFT2;
  if (!isAudio && !isFFT) return null;
  if (isAudio && codec > Codec.OPUS) return null;
  if (isFFT && codec !== Codec.FFT_U8_DB && codec !== Codec.FFT_F32_DB) return null;
  if (flags & FLAGS_RESERVED) return null;
  return {
    type,
    codec,
    streamId: dv.getUint16(4, true),
    flags,
    seq: dv.getUint32(8, true),
    timestampUs: Number(dv.getBigUint64(12, true)),
    payload: new Uint8Array(buf, HEADER_SIZE, len),
  };
}

/**
 * seqGap returns the number of missing frames between prev and next
 * (wrap-aware, like rxv1.SeqGap).
 * @param {number} prev @param {number} next
 */
export function seqGap(prev, next) {
  return (next - prev - 1) >>> 0;
}

/**
 * @param {Uint8Array} payload
 * @returns {{dbMin: number, dbStep: number, bins: Uint8Array} | null}
 */
export function parseFFTU8(payload) {
  if (payload.byteLength < 8) return null;
  const dv = new DataView(payload.buffer, payload.byteOffset, 8);
  const dbMin = dv.getFloat32(0, true);
  const dbStep = dv.getFloat32(4, true);
  if (!Number.isFinite(dbMin) || !Number.isFinite(dbStep) || dbStep <= 0) return null;
  return { dbMin, dbStep, bins: payload.subarray(8) };
}

/**
 * @param {Uint8Array} payload
 * @returns {{predictor: number, stepIndex: number, data: Uint8Array} | null}
 */
export function parseADPCM(payload) {
  if (payload.byteLength < 4) return null;
  const dv = new DataView(payload.buffer, payload.byteOffset, 4);
  const stepIndex = dv.getUint8(2);
  if (stepIndex > 88) return null;
  return { predictor: dv.getInt16(0, true), stepIndex, data: payload.subarray(4) };
}

let nextId = 0;

/**
 * envelope builds a client → node JSON text frame.
 * @param {string} type @param {object} payload @param {boolean} [withId]
 * @returns {{id: string | undefined, text: string}}
 */
export function envelope(type, payload, withId = false) {
  const id = withId ? `c-${++nextId}` : undefined;
  return { id, text: JSON.stringify({ v: 1, type, id, ts: Date.now(), payload }) };
}
