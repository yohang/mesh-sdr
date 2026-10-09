// Feature types of the map legend (MAP-008): what each kind of feature is
// called, its symbol (a shape and a color, so color is never the only
// cue) and whether it is shown by default. Receivers, stations and
// repeaters are hidden by default. Optional types are listed in the legend
// only while some feature has them.

/**
 * @typedef {object} Type
 * @property {string} id the type id (also the ?layers= value)
 * @property {string} label legend text
 * @property {string} one name of one feature (detail panel, list)
 * @property {"point" | "locator" | "call" | "receiver" | "other"} symbol
 * @property {string} color the --msdr-* color token of the markers
 * @property {boolean} shown shown by default
 * @property {boolean} optional listed only while some feature has it
 */

/** @type {Type[]} */
export const TYPES = [
  { id: "aprs", label: "APRS stations", one: "APRS station", symbol: "point", color: "color-accent", shown: true, optional: false },
  { id: "aircraft", label: "Aircraft", one: "Aircraft", symbol: "other", color: "color-info", shown: true, optional: true },
  { id: "vessel", label: "Vessels", one: "Vessel", symbol: "other", color: "color-info", shown: true, optional: true },
  { id: "sonde", label: "Radiosondes", one: "Radiosonde", symbol: "other", color: "color-info", shown: true, optional: true },
  { id: "meshtastic", label: "Meshtastic", one: "Meshtastic node", symbol: "other", color: "color-info", shown: true, optional: true },
  { id: "locator", label: "Locators", one: "Locator", symbol: "locator", color: "color-bmk-pack", shown: true, optional: false },
  { id: "call", label: "Calls", one: "Call", symbol: "call", color: "color-bmk-dial", shown: true, optional: false },
  { id: "receiver", label: "Receivers", one: "Receiver", symbol: "receiver", color: "color-bmk-hub", shown: false, optional: false },
  { id: "station", label: "Stations", one: "Station", symbol: "other", color: "color-info", shown: false, optional: true },
  { id: "repeater", label: "Repeaters", one: "Repeater", symbol: "other", color: "color-info", shown: false, optional: true },
  { id: "other", label: "Other", one: "Feature", symbol: "other", color: "color-info", shown: true, optional: true },
];

const byId = new Map(TYPES.map((t) => [t.id, t]));

/**
 * typeOf returns the type of a feature kind; unknown kinds are "other".
 * @param {string} kind
 * @returns {Type}
 */
export function typeOf(kind) {
  return byId.get(kind) ?? /** @type {Type} */ (byId.get("other"));
}

/**
 * isType tells whether id names a type.
 * @param {string} id
 */
export function isType(id) {
  return byId.has(id);
}
