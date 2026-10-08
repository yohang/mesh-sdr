// The receiver's default keyboard shortcuts (UI-014, FEATURE_SPEC §10.17),
// registered while the receiver island is on the page. Each entry is also
// its row in the shortcuts help (UI-015), the bookmark keys (S, Y, B) too.
// The audio dock registers mute and
// volume on every page; shortcuts.js registers "?" and "/".
//
// Shared-state keys (PageUp, PageDown: the centre) are for callers with
// the right; others are told so, and the node checks the right again.

/**
 * @typedef {object} Receiver what the shortcuts drive (the island)
 * @property {ReturnType<typeof import("./engine.js").getEngine>} engine
 * @property {(dir: number) => void} zoom
 * @property {(dir: number) => void} changeStep
 * @property {(hz: number) => void} shiftBand
 * @property {(factor: number) => void} scaleBand
 * @property {(dir: 1 | -1) => void} seek
 * @property {(db: number) => void} squelchBy
 * @property {() => void} autoSquelch
 * @property {() => void} autoLevels
 * @property {(lv: null) => void} setLevels
 * @property {(which: "min" | "max", dir: number) => void} nudgeLevels
 * @property {() => void} toggleSpectrum
 * @property {(i: number) => boolean} selectMode
 * @property {() => boolean} focusFrequency
 * @property {() => boolean} openPicker
 * @property {() => void} togglePanel
 * @property {() => void} showInfo
 * @property {() => void} resetBandpasses
 * @property {() => boolean} recorderOn
 * @property {() => Promise<void>} toggleRecord
 * @property {(dir: 1 | -1) => Promise<void>} centreJump
 * @property {() => boolean} escape
 * @property {HTMLInputElement} nrOn
 * @property {{scanBtn: HTMLElement, findBtn: HTMLElement, ribbonBtn: HTMLElement}} bookmarks
 *   the bookmark controls (bookmarks.js): S, Y and B press them
 */

// Pass band shift of Shift + ← → (Hz) and width factor of Shift + ↑ ↓.
const BAND_SHIFT_HZ = 50;
const BAND_FACTOR = 1.25;

/**
 * receiverShortcuts returns the receiver's bindings.
 * @param {Receiver} rx
 * @returns {import("../shortcuts.js").Shortcut[]}
 */
export function receiverShortcuts(rx) {
  const e = rx.engine;
  const tuned = () => !!e.demod;
  const digits = ["Digit1", "Digit2", "Digit3", "Digit4", "Digit5", "Digit6", "Digit7", "Digit8", "Digit9", "Digit0"];
  return [
    // Tuning.
    { keys: ["ArrowLeft"], label: "Tune down one step", group: "Tuning", repeat: true, enabled: tuned, run: () => e.step(-1) },
    { keys: ["ArrowRight"], label: "Tune up one step", group: "Tuning", repeat: true, enabled: tuned, run: () => e.step(1) },
    { keys: ["Ctrl+ArrowLeft", "Alt+Shift+ArrowLeft"], label: "Smaller tuning step", group: "Tuning", enabled: tuned, run: () => rx.changeStep(-1) },
    { keys: ["Ctrl+ArrowRight", "Alt+Shift+ArrowRight"], label: "Larger tuning step", group: "Tuning", enabled: tuned, run: () => rx.changeStep(1) },
    { keys: ["t"], label: "Type a frequency", group: "Tuning", run: () => rx.focusFrequency() },
    { keys: ["["], label: "Seek the previous signal above the squelch", group: "Tuning", enabled: tuned, run: () => rx.seek(-1) },
    { keys: ["]"], label: "Seek the next signal above the squelch", group: "Tuning", enabled: tuned, run: () => rx.seek(1) },
    {
      keys: ["PageUp"],
      label: "Move the shared centre up a quarter of the bandwidth",
      group: "Tuning",
      note: "Operators only; affects every listener",
      enabled: tuned,
      run: () => void rx.centreJump(1),
    },
    {
      keys: ["PageDown"],
      label: "Move the shared centre down a quarter of the bandwidth",
      group: "Tuning",
      note: "Operators only; affects every listener",
      enabled: tuned,
      run: () => void rx.centreJump(-1),
    },
    { keys: ["p"], label: "Choose the device", group: "Tuning", run: () => rx.openPicker() },

    // Mode and filter.
    {
      keys: digits,
      label: "Select mode button 1 to 10",
      display: "1 … 9, 0",
      group: "Mode and filter",
      enabled: tuned,
      run: (ev) => rx.selectMode(digits.indexOf(ev.code)),
    },
    {
      keys: digits.map((d) => `Shift+${d}`),
      label: "Select mode button 11 to 20",
      display: "Shift + 1 … 9, 0",
      group: "Mode and filter",
      enabled: tuned,
      run: (ev) => rx.selectMode(10 + digits.indexOf(ev.code)),
    },
    { keys: ["Shift+ArrowLeft"], label: `Shift the filter down ${BAND_SHIFT_HZ} Hz`, group: "Mode and filter", repeat: true, enabled: tuned, run: () => rx.shiftBand(-BAND_SHIFT_HZ) },
    { keys: ["Shift+ArrowRight"], label: `Shift the filter up ${BAND_SHIFT_HZ} Hz`, group: "Mode and filter", repeat: true, enabled: tuned, run: () => rx.shiftBand(BAND_SHIFT_HZ) },
    { keys: ["Shift+ArrowUp"], label: "Wider filter", group: "Mode and filter", repeat: true, enabled: tuned, run: () => rx.scaleBand(BAND_FACTOR) },
    { keys: ["Shift+ArrowDown"], label: "Narrower filter", group: "Mode and filter", repeat: true, enabled: tuned, run: () => rx.scaleBand(1 / BAND_FACTOR) },
    { keys: ["|"], label: "Forget the filters saved per mode", group: "Mode and filter", run: () => rx.resetBandpasses() },

    // Squelch and noise.
    { keys: ["{"], label: "Squelch level down 1 dB", group: "Squelch and noise", repeat: true, enabled: tuned, run: () => rx.squelchBy(-1) },
    { keys: ["}"], label: "Squelch level up 1 dB", group: "Squelch and noise", repeat: true, enabled: tuned, run: () => rx.squelchBy(1) },
    { keys: ["a"], label: "Auto squelch", group: "Squelch and noise", enabled: tuned, run: () => rx.autoSquelch() },
    { keys: ["d"], label: "Squelch off", group: "Squelch and noise", enabled: tuned, run: () => e.setSquelch(null) },
    { keys: ["n"], label: "Noise reduction on or off", group: "Squelch and noise", enabled: tuned, run: () => rx.nrOn.click() },

    // View.
    { keys: ["ArrowUp"], label: "Zoom in", group: "View", run: () => rx.zoom(1) },
    { keys: ["ArrowDown"], label: "Zoom out", group: "View", run: () => rx.zoom(-1) },
    { keys: ["z"], label: "Auto waterfall levels", group: "View", run: () => rx.autoLevels() },
    { keys: ["c"], label: "Default waterfall levels", group: "View", run: () => rx.setLevels(null) },
    { keys: [","], label: "Waterfall lower level down", group: "View", repeat: true, run: () => rx.nudgeLevels("min", -1) },
    { keys: ["."], label: "Waterfall lower level up", group: "View", repeat: true, run: () => rx.nudgeLevels("min", 1) },
    { keys: ["<"], label: "Waterfall upper level down", group: "View", repeat: true, run: () => rx.nudgeLevels("max", -1) },
    { keys: [">"], label: "Waterfall upper level up", group: "View", repeat: true, run: () => rx.nudgeLevels("max", 1) },
    { keys: ["v"], label: "Spectrum on or off", group: "View", run: () => rx.toggleSpectrum() },

    // Bookmarks (bookmarks.js): the keys press the visible controls.
    { keys: ["s"], label: "Start or stop the bookmark scanner", group: "Bookmarks", enabled: tuned, run: () => rx.bookmarks.scanBtn.click() },
    { keys: ["y"], label: "Find a bookmark", group: "Bookmarks", run: () => rx.bookmarks.findBtn.click() },
    { keys: ["b"], label: "Band plan ribbon on or off", group: "View", run: () => rx.bookmarks.ribbonBtn.click() },

    // Capture.
    // Only when the recorder is offered (ui.recorder_enabled, admins).
    ...(rx.recorderOn() ? [{ keys: ["r"], label: "Start or stop recording", group: "Capture", run: () => void rx.toggleRecord() }] : []),

    // Panel.
    { keys: ["Enter"], label: "Open or close the side panel", group: "General", run: () => rx.togglePanel() },
    { keys: ["i"], label: "Open the Info tab", group: "General", run: () => rx.showInfo() },
    { keys: ["Escape"], label: "Close the shortcuts help, the More controls, the sheet or the Display menu", group: "General", run: () => rx.escape() },
  ];
}
