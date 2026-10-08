// Marks of the bookmark bar (BMK-001, BMK-007, RX-030): pack and hub
// bookmarks and bandplan dial frequencies, in one sorted list, and the
// helpers the bar, the Bookmarks tab, the search and the scanner share.

// Analog modes a demodulator can run (RX-007; no digital mode before M2).
export const ANALOG_MODES = ["am", "sam", "nfm", "wfm", "usb", "lsb", "cw"];

/**
 * @typedef {object} Mark a marker of the bookmark bar
 * @property {string} key unique: "b:<id>" (bookmark) or "d:<Hz>:<mode>" (dial)
 * @property {"hub" | "pack" | "dial"} origin
 * @property {string} name plain text
 * @property {number} frequency Hz
 * @property {string} modulation
 * @property {string} underlying "" when none
 * @property {string} description plain text, "" when none
 * @property {boolean} scannable
 */

// Order of the origins on the same frequency (BMK-007): dial frequency,
// then hub, then pack.
const RANK = { dial: 0, hub: 1, pack: 2 };

/** @param {Mark} a @param {Mark} b */
export function compareMarks(a, b) {
  return a.frequency - b.frequency || RANK[a.origin] - RANK[b.origin] || a.name.localeCompare(b.name);
}

/**
 * markOf reads a bookmark of GET /api/v1/bookmarks. Pack rows are origin
 * builtin; every other origin is a hub row.
 * @param {any} b
 * @returns {Mark}
 */
export function markOf(b) {
  return {
    key: `b:${b.id}`,
    origin: b.origin === "builtin" ? "pack" : "hub",
    name: String(b.name ?? ""),
    frequency: Number(b.frequency),
    modulation: String(b.modulation ?? ""),
    underlying: String(b.underlying ?? ""),
    description: String(b.description ?? ""),
    scannable: !!b.scannable,
  };
}

/**
 * dialOf reads a dial frequency of GET /api/v1/bandplan.
 * @param {any} d
 * @returns {Mark}
 */
export function dialOf(d) {
  const mode = String(d.mode ?? "");
  return {
    key: `d:${d.frequency}:${mode}`,
    origin: "dial",
    name: mode.toUpperCase(),
    frequency: Number(d.frequency),
    modulation: mode,
    underlying: String(d.underlying ?? ""),
    description: d.band ? `Dial frequency in the ${d.band} band` : "Dial frequency",
    scannable: false,
  };
}

// Default underlying analog mode of the digital modes, the first of their
// underlying list in OpenWebRX+ (luarvique/openwebrx owrx/modes.py at
// 519a366b, AGPL-3.0, like the packs and band plans of ADR 0027). Modes
// whose underlying is "empty" there (ADS-B, LoRa, ISM…) have none.
const DEFAULT_UNDERLYING = /** @type {Record<string, string>} */ ({
  bpsk31: "usb", bpsk63: "usb", rtty170: "usb", rtty450: "usb", rtty85: "usb",
  sitorb: "usb", navtex: "usb", dsc: "usb", msk144: "usb", fax: "usb", hfdl: "usb",
  ft8: "usb", ft4: "usb", jt65: "usb", jt9: "usb", wspr: "usb", fst4: "usb", fst4w: "usb", q65: "usb", js8: "usb",
  cwdecoder: "usb", sstv: "usb", speech: "am", audio: "am", acars: "am",
  packet: "nfm", ais: "nfm", page: "nfm", selcall: "nfm", zvei: "nfm", eas: "nfm", vdl2: "nfm",
  "sonde-rs41": "nfm", "sonde-dfm9": "nfm", "sonde-dfm17": "nfm", "sonde-mts01": "nfm", "sonde-m10": "nfm", "sonde-m20": "nfm",
});

/**
 * tuneMode is the analog mode a mark tunes on a device offering modes: its
 * modulation when analog, else its underlying mode, else the default
 * underlying mode of its digital modulation (OpenWebRX+); "" when none is
 * offered (the current mode stays). Digital modes and their offsets come
 * with M2.
 * @param {string} modulation @param {string} underlying @param {string[]} modes
 */
export function tuneMode(modulation, underlying, modes) {
  for (const m of [modulation, underlying || DEFAULT_UNDERLYING[modulation] || ""]) {
    if (ANALOG_MODES.includes(m) && (modes.length === 0 || modes.includes(m))) return m;
  }
  return "";
}

/** @param {"hub" | "pack" | "dial"} origin */
export function originText(origin) {
  return { hub: "hub bookmark", pack: "pack bookmark", dial: "dial frequency" }[origin];
}

/** @param {number} hz as MHz, three to six decimals */
export function formatMHz(hz) {
  const s = (hz / 1e6).toFixed(6).replace(/0{1,3}$/, "");
  return `${s} MHz`;
}
