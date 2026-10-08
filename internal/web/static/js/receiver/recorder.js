// Browser recording (REC-001, ADR 0027): the PCM actually played (audio.js
// keeps it while recording) becomes a 16-bit mono WAV file, written here
// with no encoder dependency, and downloaded as
// REC-<yymmdd-HHMMSS>-<kHz>.wav (UTC time of the start, tuned frequency).

/**
 * wavBlob writes 16-bit mono PCM as a WAV (RIFF) file.
 * @param {Int16Array[]} chunks @param {number} rate samples per second
 * @returns {Blob}
 */
export function wavBlob(chunks, rate) {
  const samples = chunks.reduce((n, c) => n + c.length, 0);
  const header = new DataView(new ArrayBuffer(44));
  /** @param {number} at @param {string} text */
  const ascii = (at, text) => {
    for (let i = 0; i < text.length; i++) header.setUint8(at + i, text.charCodeAt(i));
  };
  ascii(0, "RIFF");
  header.setUint32(4, 36 + samples * 2, true);
  ascii(8, "WAVE");
  ascii(12, "fmt ");
  header.setUint32(16, 16, true); // fmt chunk size
  header.setUint16(20, 1, true); // PCM
  header.setUint16(22, 1, true); // mono
  header.setUint32(24, rate, true);
  header.setUint32(28, rate * 2, true); // bytes per second
  header.setUint16(32, 2, true); // block align
  header.setUint16(34, 16, true); // bits per sample
  ascii(36, "data");
  header.setUint32(40, samples * 2, true);
  // WAV is little-endian, as are the typed arrays of every supported browser.
  return new Blob([header, ...chunks], { type: "audio/wav" });
}

/**
 * recordingName is the file name of a recording started at date (UTC) on
 * hz: REC-<yymmdd-HHMMSS>-<kHz>.wav.
 * @param {Date} date @param {number} hz
 */
export function recordingName(date, hz) {
  const p = (/** @type {number} */ n) => String(n).padStart(2, "0");
  const stamp = `${p(date.getUTCFullYear() % 100)}${p(date.getUTCMonth() + 1)}${p(date.getUTCDate())}-${p(date.getUTCHours())}${p(date.getUTCMinutes())}${p(date.getUTCSeconds())}`;
  return `REC-${stamp}-${Math.round(hz / 1000)}.wav`;
}

/**
 * saveFile hands a blob to the browser as a download.
 * @param {Blob} blob @param {string} name
 */
export function saveFile(blob, name) {
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  a.hidden = true;
  a.setAttribute("hx-boost", "false");
  document.body.append(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 10_000);
}
