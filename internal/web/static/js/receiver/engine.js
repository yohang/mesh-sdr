// Receiver engine: the shell-level singleton that owns the long-lived
// resources (node media WebSocket, AudioContext, decoders, the recent FFT
// history, the display settings). It lives outside #main, so boosted
// navigation never stops the audio (ADR 0003, ADR 0007, ADR 0015 decision
// 9); the <msdr-receiver> island only attaches a view to it and detaches on
// disconnectedCallback, and <msdr-audio-dock> controls the audio from any
// page.
//
// Connection (RX-002, TECHNICAL_SPEC §5.16, §6.2–6.5): the browser opens
// /nodes/{nodeId}/ws through the gateway with the rx.v1 subprotocol; the
// session cookie authorises the upgrade and the gateway injects the first
// access token. The engine then sends session.hello, device.attach and
// demod.create (the device's start mode and offset). Before the token
// expires it gets a new one from POST /api/v1/auth/token (CSRF through
// apiFetch) and sends it with auth.refresh; tokens live in memory only
// (§5.8). A lost connection is retried with a jittered exponential delay
// (RX-003); each new upgrade gets a fresh token from the gateway.
//
// Demodulator controls (RX-007, RX-020, RX-023/024, RX-027): mode, pass
// band, squelch and noise reduction go to the node with demod.set; the ack
// names the values in force (clamped by the node), which the engine keeps.
// The pass band the listener sets is saved per mode (RX-021, bandpasses.js)
// and sent with each new demodulator and mode switch.
// Broadcast FM needs 48 kHz audio (HD path), so a switch to or from it
// reconfigures the audio stream around the mode change. The demodulator
// settings are kept across reconnections to the same device.
//
// Shared state (RX-006, RX-010): a caller with the preset right switches the
// device's preset with preset.select, one with the retune right moves its
// centre with device.retune (device.config.permissions says which). The node
// retunes the device for every listener and sends them the new
// device.config and the moved demodulator (stream.update applied).
// Refusals (forbidden, rate_limited, preset_incompatible) become a short
// detail of the state.
//
// Tuning step (RX-012): the visitor picks a step; 0 follows the device's
// tuning_step_hz, which every device.config (attach, preset switch)
// restores.
//
// Device switch (RX-040): connect() to another device closes the node
// WebSocket and opens /nodes/{nodeId}/ws again; the gateway's forward auth
// mints the new connection's access token, scoped to the devices of that
// node the visitor may listen to (the hub checks the listen policy, the
// node checks the token again).
//
// Deep link (RX-028): connect() takes the wanted tuning of a link (Hz, mode,
// squelch). It only sets this visitor's demodulator: a frequency outside
// the capture band is kept in outOfBand (the island offers a retune to
// callers with that right) and applied once a centre change brings it
// into the band.
//
// Node offline (GRID-021): the connection is retried with backoff, keeping
// the device and the demodulator settings; retryNow() reconnects at once
// when the hub says the node is back.
//
// Changes are announced with a "change" event whose detail names what
// changed: "state", "config", "tune", "meter" or "audio". Other events:
// "shared" ({kind: "preset" | "centre", preset, centerHz}) when someone
// else switched the device's preset or moved its centre (RX-042),
// "message" ({text}) for an unsolicited node error (RX-038) and "session"
// when the hub refused a token refresh (session ended). FFT lines go to the
// attached views directly (hot path). Network figures (RX-035) are in
// net: received bit rate, round-trip time and its jitter.

import { apiFetch } from "../csrf.js";
import { decodeADPCM } from "./adpcm.js";
import { AudioPlayer } from "./audio.js";
import { savedBandpass, saveBandpass } from "./bandpasses.js";
import { Codec, envelope, Flag, FrameType, parseADPCM, parseFFTU8, parseFrame, seqGap } from "./rxv1.js";

const SUBPROTOCOL = "rx.v1";
const TOKEN_URL = "/api/v1/auth/token";
// Raw FFT lines kept to redraw the waterfall after navigation, zoom or a
// level change (ADR 0015 decision 9).
const HISTORY = 1024;
const MIN_RETRY_MS = 1000;
const MAX_RETRY_MS = 60_000;
// The token is refreshed this long before it expires (auth.token_ttl is 5
// minutes by default); a failed refresh is tried again after RETRY_REFRESH_MS.
const REFRESH_MARGIN_MS = 60_000;
const RETRY_REFRESH_MS = 15_000;
// demod.set at most every TUNE_INTERVAL_MS while dragging (msg_rate, §6.9).
const TUNE_INTERVAL_MS = 100;
// Squelch range of the node (dBFS); the minimum keeps the squelch open.
export const SQUELCH_MIN_DB = -150;
export const SQUELCH_MAX_DB = 0;
// NR threshold range of the node (dB).
export const NR_MIN_DB = -20;
export const NR_MAX_DB = 20;
// Narrowest pass band the node accepts (Hz).
export const MIN_BANDWIDTH_HZ = 100;
// Auto squelch margin over the signal level when device.config has none.
const SQUELCH_AUTO_MARGIN_DB = 10;
// Audio rates: speech band, and the HD path broadcast FM needs.
const AUDIO_RATE = 12000;
const HD_AUDIO_RATE = 48000;
const HD_MODES = new Set(["wfm", "usbd", "lsbd"]);
// Audio frames last 20 ms (§6.7); a sequence gap of n frames is n × 20 ms.
const FRAME_MS = 20;
// Close codes after which reconnecting cannot help (§6.2 client behaviour).
const NO_RETRY = new Set([1000, 1003, 1008, 1009, 4400, 4403, 4426]);
// A shared change seen this soon after this client asked for one is its
// own (no "shared" event).
const OWN_SHARED_MS = 5000;
// Jitter: smoothing of the round-trip time variation (RFC 3550 style).
const JITTER_GAIN = 1 / 8;

/**
 * @typedef {object} Wanted the tuning of a deep link (RX-028)
 * @property {number} [hz] frequency
 * @property {string} [mode]
 * @property {number | null} [squelchDb] null: open
 * @property {string} [decoder] digital mode (m2)
 */

/**
 * @typedef {object} Target the device the engine listens to
 * @property {string} node_id
 * @property {string} device_id
 * @property {string} name
 */

/**
 * @typedef {object} Demod the demodulator in use, as last applied
 * @property {string} id
 * @property {number} streamId
 * @property {string} mode
 * @property {number} offsetHz
 * @property {number} lowHz pass band, relative to the offset
 * @property {number} highHz
 * @property {number | null} squelchDb null: open
 * @property {{enabled: boolean, threshold: number}} nr
 */

/**
 * @typedef {object} Kept demod.create fields kept across reconnections
 * @property {string} [mode]
 * @property {number} [offset_hz]
 * @property {number} [hz] the absolute frequency when the connection was
 *   lost: a reconnection keeps it when the (possibly new) centre still
 *   has it in band, else starts at the device's start frequency
 * @property {{low_hz: number, high_hz: number}} [bandpass]
 * @property {number} [squelch_db]
 * @property {{enabled: boolean, threshold: number}} [nr]
 */

/**
 * @typedef {object} View
 * @property {(bins: Uint8Array) => void} onFFT called for each FFT line
 */

class Engine extends EventTarget {
  constructor() {
    super();
    this.audio = new AudioPlayer();
    /** @type {Set<View>} */
    this.views = new Set();
    /** @type {Uint8Array[]} */
    this.history = [];
    /** @type {Target | null} */
    this.target = null;
    /** @type {WebSocket | null} */
    this.ws = null;
    this.gen = 0;
    this.retry = 0;
    this.retryTimer = 0;
    /** @type {Kept} */
    this.kept = {};
    this.offsetMs = 0; // node clock − client clock
    // Audio codec asked for in session.hello (hub setting
    // audio_compression, set by the island): adpcm-ima or pcm-s16le.
    this.codec = "adpcm-ima";
    // Display settings, kept across navigation (per-session runtime state):
    // zoom factor and first visible bin, spectrum visibility (RX-016), manual
    // levels (null: the device defaults, RX-017/018), docked side panel
    // open (UI-019; null: the layout's default, UI-007).
    this.display = {
      zoom: 1,
      start: 0,
      spectrum: true,
      /** @type {{min: number, max: number} | null} */ levels: null,
      /** @type {boolean | null} */ panel: null,
    };
    /** @type {Wanted | null} deep link tuning, applied at the next attach */
    this.want = null;
    /** @type {number | null} a linked frequency outside the capture band */
    this.outOfBand = null;
    // When this client last asked for a shared change (ms).
    this.ownShared = 0;
    // Network figures (RX-035): bits per second received over the last
    // second, last round-trip time and its jitter (ms).
    this.net = { bytes: 0, bitRate: 0, rttMs: /** @type {number | null} */ (null), jitterMs: /** @type {number | null} */ (null) };
    setInterval(() => {
      this.net.bitRate = this.ws ? this.net.bytes * 8 : 0;
      this.net.bytes = 0;
    }, 1000);
    this.reset();
    document.addEventListener("visibilitychange", () => this.onVisibility());
  }

  /** @param {string} type @param {any} [detail] */
  signal(type, detail) {
    this.dispatchEvent(new CustomEvent(type, { detail }));
  }

  // reset forgets the per-connection state.
  reset() {
    clearTimeout(this.refreshTimer);
    clearTimeout(this.tuneTimer);
    this.tuneTimer = 0;
    /** @type {Record<string, any>} demod.set fields waiting for the next send */
    this.queued = {};
    this.state =this.target ? "connecting" : "idle";
    this.detail = "";
    this.cid = "";
    this.tokenExp = 0;
    // Tuning step picked by the visitor (Hz), 0: the device's step.
    this.stepHz = 0;
    this.bestRtt = Infinity;
    /** @type {Map<string, {resolve: (v: any) => void, reject: (e: any) => void}>} */
    this.pending = new Map();
    /** @type {any} device.config, patched */
    this.device = null;
    /** @type {{state: string, reason?: string} | null} */
    this.deviceState = null;
    /** @type {{streamId: number, size: number, startHz: number, spanHz: number, dbMin: number, dbStep: number} | null} */
    this.fft = null;
    /** @type {Demod | null} */
    this.demod = null;
    /** @type {{id: number, rate: number, codec: string} | null} */
    this.audioStream = null;
    /** @type {{levelDb: number, open: boolean} | null} */
    this.meter = null;
    /** @type {Map<number, number>} */
    this.lastSeq = new Map();
  }

  /** @param {string} what */
  emit(what) {
    this.dispatchEvent(new CustomEvent("change", { detail: what }));
  }

  /** @param {string} state @param {string} [detail] */
  setState(state, detail = "") {
    this.state = state;
    this.detail = detail;
    this.emit("state");
  }

  /**
   * connect listens to target; a different device closes the current
   * connection first. want is the tuning of a deep link: for the device
   * already listened to, it is applied at once.
   * @param {Target} target @param {Wanted | null} [want]
   */
  connect(target, want = null) {
    const t = this.target;
    if (t && t.node_id === target.node_id && t.device_id === target.device_id && (this.ws || this.retryTimer)) {
      if (want) this.applyLink(want);
      return;
    }
    this.disconnect();
    this.target = target;
    this.history = [];
    // Demodulator settings kept across reconnections of this device.
    this.kept = {};
    this.want = want;
    this.outOfBand = null;
    this.open();
  }

  // retryNow reconnects at once when a retry is pending (the node is back,
  // GRID-021).
  retryNow() {
    if (!this.target || this.ws || !this.retryTimer) return;
    clearTimeout(this.retryTimer);
    this.retryTimer = 0;
    this.retry = 0;
    this.open();
  }

  /**
   * applyLink tunes this visitor's demodulator to a deep link (RX-028): mode,
   * frequency (outOfBand when outside the capture band), squelch. Before the
   * demodulator exists, it waits for the next attach.
   * @param {Wanted} w
   */
  applyLink(w) {
    if (!this.demod || !this.device) {
      this.want = w;
      return;
    }
    if (w.mode && w.mode !== this.demod.mode) this.setMode(w.mode);
    if (typeof w.hz === "number") this.outOfBand = this.tune(w.hz, false) ? null : w.hz;
    if (w.squelchDb !== undefined) this.setSquelch(w.squelchDb);
    if (w.decoder && w.decoder !== this.demod.decoder) this.setDecoder(w.decoder);
    this.emit("tune");
  }

  /**
   * inBand reports whether hz is inside the capture band.
   * @param {number} hz
   */
  inBand(hz) {
    const [lo, hi] = this.band();
    return this.device !== null && hz >= lo && hz <= hi;
  }

  // disconnect closes the connection and stops retrying.
  disconnect() {
    clearTimeout(this.retryTimer);
    this.retryTimer = 0;
    this.gen++;
    const ws = this.ws;
    this.ws = null;
    if (ws) ws.close(1000);
    this.rejectPending("disconnected");
    this.target = null;
    this.reset();
    this.audio.flush();
    this.emit("state");
  }

  open() {
    const target = /** @type {Target} */ (this.target);
    const url = new URL(`/nodes/${encodeURIComponent(target.node_id)}/ws`, location.href);
    url.protocol = location.protocol === "https:" ? "wss:" : "ws:";
    const gen = ++this.gen;
    const ws = new WebSocket(url, SUBPROTOCOL);
    ws.binaryType = "arraybuffer";
    this.ws = ws;
    this.setState("connecting");
    ws.onopen = () => {
      this.send("session.hello", {
        client: { name: "meshsdr-web", version: "1" },
        capabilities: {
          // Listing pcm-s16le alone makes the node send PCM.
          audio_codecs: this.codec === "pcm-s16le" ? ["pcm-s16le"] : ["adpcm-ima", "pcm-s16le"],
          fft_codecs: ["u8-db"],
          audio_rates: [AUDIO_RATE, HD_AUDIO_RATE],
          max_fft_fps: 60,
        },
      });
    };
    ws.onmessage = (e) => {
      if (gen !== this.gen) return;
      if (typeof e.data === "string") {
        this.net.bytes += e.data.length;
        this.onText(e.data);
      } else {
        this.net.bytes += e.data.byteLength;
        this.onBinary(e.data);
      }
    };
    ws.onclose = (e) => {
      if (gen !== this.gen) return;
      this.onClose(e.code, e.reason);
    };
  }

  /** @param {number} code @param {string} reason */
  onClose(code, reason) {
    const welcomed = this.cid !== "";
    // Listeners keep their absolute frequency, not their offset: the
    // centre may differ after the reconnection (the node restarted its
    // device, an operator moved it meanwhile).
    if (this.demod) this.kept.hz = this.tunedHz;
    this.ws = null;
    this.gen++;
    this.rejectPending("connection closed");
    this.reset();
    if (NO_RETRY.has(code)) {
      this.setState("failed", reason || `connection closed (${code})`);
      return;
    }
    if (welcomed) this.retry = 0;
    const delay = Math.min(MAX_RETRY_MS, MIN_RETRY_MS * 2 ** this.retry++) * (0.5 + Math.random() / 2);
    this.setState("reconnecting");
    this.retryTimer = setTimeout(() => {
      this.retryTimer = 0;
      if (this.target && !this.ws) this.open();
    }, delay);
  }

  /** @param {string} reason */
  rejectPending(reason) {
    for (const p of this.pending.values()) p.reject({ code: "closed", message: reason });
    this.pending.clear();
  }

  /** @param {string} type @param {object} payload */
  send(type, payload) {
    if (this.ws?.readyState !== WebSocket.OPEN) return;
    this.ws.send(envelope(type, payload).text);
  }

  /**
   * request sends a message with a correlation id and resolves with the
   * ack result, or rejects with the error payload.
   * @param {string} type @param {object} payload
   * @returns {Promise<any>}
   */
  request(type, payload) {
    if (this.ws?.readyState !== WebSocket.OPEN) {
      return Promise.reject({ code: "closed", message: "not connected" });
    }
    const env = envelope(type, payload, true);
    const ws = this.ws;
    return new Promise((resolve, reject) => {
      this.pending.set(/** @type {string} */ (env.id), { resolve, reject });
      ws.send(env.text);
    });
  }

  /** @param {string} text */
  onText(text) {
    let env;
    try {
      env = JSON.parse(text);
    } catch {
      return;
    }
    const p = env?.payload ?? {};
    switch (env?.type) {
      case "session.welcome":
        this.cid = p.cid ?? "";
        this.tokenExp = p.token_exp ?? 0;
        if (p.server_time) this.offsetMs = p.server_time - Date.now();
        this.setState("connected");
        this.scheduleRefresh();
        this.timeSync();
        this.attach();
        break;
      case "time.sync.reply": {
        const t3 = Date.now();
        const rtt = t3 - p.t0 - (p.t2 - p.t1);
        const n = this.net;
        if (n.rttMs !== null) n.jitterMs = (n.jitterMs ?? 0) + (Math.abs(rtt - n.rttMs) - (n.jitterMs ?? 0)) * JITTER_GAIN;
        n.rttMs = Math.max(0, rtt);
        if (rtt <= this.bestRtt) {
          this.bestRtt = rtt;
          this.offsetMs = (p.t1 - p.t0 + (p.t2 - t3)) / 2;
        }
        break;
      }
      case "ack": {
        const req = this.pending.get(p.re);
        this.pending.delete(p.re);
        req?.resolve(p.result);
        break;
      }
      case "error": {
        const req = p.re ? this.pending.get(p.re) : undefined;
        if (req) {
          this.pending.delete(p.re);
          req.reject(p);
        } else {
          this.detail = p.message ?? "";
          this.emit("state");
          if (this.detail) this.signal("message", { text: this.detail });
        }
        break;
      }
      case "device.config": {
        const prev = this.device;
        this.device = p;
        // A new config (attach, preset switch) restores the device's step.
        this.stepHz = 0;
        this.sharedChanged(prev);
        this.emit("config");
        this.emit("tune");
        break;
      }
      case "device.config.patch":
        if (this.device && p.device_id === this.device.device_id) {
          const prev = { ...this.device };
          Object.assign(this.device, p.set ?? {});
          for (const k of p.unset ?? []) delete this.device[k];
          this.sharedChanged(prev);
          this.emit("config");
        }
        break;
      case "device.state":
        this.deviceState = { state: p.state, reason: p.reason };
        this.emit("config");
        break;
      case "stream.open":
        this.openStream(p);
        break;
      case "stream.update":
        this.updateStream(p);
        break;
      case "stream.close":
        if (this.fft?.streamId === p.stream_id) this.fft = null;
        if (this.audioStream?.id === p.stream_id) this.audioStream = null;
        this.emit("config");
        break;
      case "demod.meter":
        if (this.demod?.id === p.demod_id) {
          this.meter = { levelDb: p.level_db, open: p.squelch_open };
          this.emit("meter");
        }
        break;
      // The listener's decoder (DEC-002): its messages and its status.
      case "decode":
        if (this.demod?.id === p.demod_id) this.signal("decode", p);
        break;
      case "diag.state":
        if (this.demod?.id === p.demod_id) {
          if (p.state === "stopped" && this.demod.decoder === p.decoder) this.demod.decoder = null;
          this.signal("decoderstatus", p);
          this.emit("tune");
        }
        break;
    }
  }

  /** @param {any} p stream.open */
  openStream(p) {
    if (p.kind === "fft" && p.fft) {
      this.setFFT(p.stream_id, p.fft);
    } else if (p.kind === "audio") {
      this.audioStream = { id: p.stream_id, rate: p.sample_rate, codec: p.codec };
      this.audio.flush();
    }
    this.emit("config");
  }

  /** @param {any} p stream.update */
  updateStream(p) {
    if (this.fft?.streamId === p.stream_id && p.fft) this.setFFT(p.stream_id, p.fft);
    if (this.audioStream?.id === p.stream_id) {
      if (p.sample_rate) this.audioStream.rate = p.sample_rate;
      if (p.codec) this.audioStream.codec = p.codec;
    }
    // A shared preset switch or retune moved the demodulator (applied
    // parameters); they are kept for a reconnection.
    const a = p.applied;
    const d = this.demod;
    if (a && d?.streamId === p.stream_id) {
      if (typeof a.offset_hz === "number" && !("offset_hz" in this.queued)) d.offsetHz = a.offset_hz;
      this.applied(a);
      const k = this.kept;
      k.offset_hz = d.offsetHz;
      if (a.mode && a.mode !== k.mode) {
        k.mode = a.mode;
        delete k.bandpass;
      }
      if (typeof a.squelch_db === "number" && !("squelch_db" in this.queued)) k.squelch_db = a.squelch_db;
      if (a.nr && !("nr" in this.queued)) k.nr = { ...d.nr };
    }
    this.emit("config");
  }

  /**
   * sharedChanged follows a change of the device's shared state (RX-042):
   * a linked frequency the new centre brings into the band is tuned, and a
   * preset switch or a centre move by someone else is announced ("shared").
   * @param {any} prev the device.config before the change
   */
  sharedChanged(prev) {
    const d = this.device;
    if (!prev || !d || prev.device_id !== d.device_id) return;
    if (this.outOfBand !== null && this.demod && this.inBand(this.outOfBand)) {
      const hz = this.outOfBand;
      this.outOfBand = null;
      // After the stream.update that moves the demodulator.
      setTimeout(() => this.tune(hz, false), 0);
    }
    if (Date.now() - this.ownShared < OWN_SHARED_MS) return;
    if ((prev.active_preset?.id ?? "") !== (d.active_preset?.id ?? "")) {
      this.signal("shared", { kind: "preset", preset: d.active_preset?.name ?? "", centerHz: d.center_hz });
    } else if (prev.center_hz !== d.center_hz) {
      this.signal("shared", { kind: "centre", centerHz: d.center_hz });
    }
  }

  /** @param {number} id @param {any} f StreamFFT */
  setFFT(id, f) {
    if (this.fft && this.fft.size !== f.size) this.history = [];
    this.fft = { streamId: id, size: f.size, startHz: f.start_hz, spanHz: f.span_hz, dbMin: f.db_min, dbStep: f.db_step };
  }

  // attach asks for the device's FFT, then a demodulator at its start mode
  // and offset (audio and meter).
  async attach() {
    const gen = this.gen;
    const target = /** @type {Target} */ (this.target);
    try {
      const res = await this.request("device.attach", { device_id: target.device_id, fft: { codec: "u8-db" } });
      if (gen !== this.gen) return;
      if (res?.device) this.device = res.device;
      for (const s of res?.streams ?? []) this.openStream(s);
      const start = this.device?.start ?? {};
      const k = this.kept;
      // A deep link sets this visitor's demodulator only (RX-028).
      const w = this.want;
      this.want = null;
      if (w) {
        if (w.mode) {
          k.mode = w.mode;
          delete k.bandpass;
        }
        if (typeof w.hz === "number") {
          if (this.inBand(w.hz)) k.offset_hz = Math.round(w.hz) - this.centerHz;
          this.outOfBand = this.inBand(w.hz) ? null : w.hz;
        }
        if (w.squelchDb !== undefined) k.squelch_db = w.squelchDb ?? SQUELCH_MIN_DB;
        if (w.decoder) k.decoder = w.decoder;
      }
      if (typeof k.hz === "number" && typeof w?.hz !== "number") {
        if (this.inBand(k.hz)) k.offset_hz = k.hz - this.centerHz;
        else delete k.offset_hz;
      }
      const mode = k.mode || start.mode || "nfm";
      // Each connection starts with speech-band audio.
      if (HD_MODES.has(mode)) {
        await this.request("audio.configure", { codec: this.audioCodec(), sample_rate: HD_AUDIO_RATE });
        if (gen !== this.gen) return;
      }
      /** @type {Record<string, any>} */
      const create = {
        device_id: target.device_id,
        mode,
        offset_hz: k.offset_hz ?? start.offset_hz ?? 0,
        squelch_db: k.squelch_db ?? this.device?.squelch?.initial ?? SQUELCH_MIN_DB,
      };
      // The pass band kept for this device, else the one saved for the
      // mode (RX-021), else the mode's default.
      const bandpass = k.bandpass ?? savedBandpass(mode);
      if (bandpass) create.bandpass = bandpass;
      if (k.nr) create.nr = k.nr;
      const d = await this.request("demod.create", create);
      if (gen !== this.gen) return;
      this.demod = {
        id: d.demod_id,
        streamId: d.audio_stream_id,
        mode,
        offsetHz: d.applied?.offset_hz ?? create.offset_hz,
        lowHz: 0,
        highHz: 0,
        squelchDb: null,
        nr: { enabled: false, threshold: 0 },
        decoder: null,
      };
      delete k.hz;
      // The decoder of the previous connection, or of the link (m2): the
      // new demodulator has none yet.
      const decoder = k.decoder;
      const variant = k.variant;
      this.applied(d.applied ?? {});
      this.setState("listening");
      this.emit("tune");
      if (decoder) this.setDecoder(decoder, variant);
    } catch (err) {
      if (gen !== this.gen) return;
      this.setState("error", err?.message ?? String(err));
    }
  }

  timeSync() {
    const gen = this.gen;
    let n = 0;
    const tick = () => {
      if (gen !== this.gen) return;
      this.send("time.sync", { t0: Date.now() });
      setTimeout(tick, ++n < 5 ? 500 : 10_000);
    };
    tick();
  }

  scheduleRefresh() {
    clearTimeout(this.refreshTimer);
    if (!this.tokenExp) return;
    const left = this.tokenExp * 1000 - (Date.now() + this.offsetMs);
    this.refreshTimer = setTimeout(() => this.refresh(), Math.max(5000, left - REFRESH_MARGIN_MS));
  }

  // refresh gets a new access token for this connection and sends it in
  // band. When refreshes keep failing, the node closes the connection at
  // expiry (4401) and the reconnection gets a fresh token from the gateway.
  async refresh() {
    const gen = this.gen;
    const target = this.target;
    if (!target || !this.cid) return;
    try {
      const res = await apiFetch(TOKEN_URL, {
        method: "POST",
        headers: { "Content-Type": "application/json", Accept: "application/json" },
        body: JSON.stringify({ node_id: target.node_id, cid: this.cid }),
      });
      if (res.status === 401) this.signal("session");
      if (res.status === 401 || res.status === 403) return;
      if (!res.ok) throw new Error(`token API answered ${res.status}`);
      const body = await res.json();
      if (gen !== this.gen) return;
      await this.request("auth.refresh", { token: body.token });
      if (gen !== this.gen) return;
      this.tokenExp = Date.parse(body.expires_at) / 1000;
      this.scheduleRefresh();
    } catch {
      if (gen !== this.gen) return;
      this.refreshTimer = setTimeout(() => this.refresh(), RETRY_REFRESH_MS);
    }
  }

  /** @param {ArrayBuffer} buf */
  onBinary(buf) {
    const f = parseFrame(buf);
    if (!f) return;
    const prev = this.lastSeq.get(f.streamId);
    const gap = prev === undefined ? 0 : seqGap(prev, f.seq);
    this.lastSeq.set(f.streamId, f.seq);

    if (f.type === FrameType.FFT) {
      if (f.streamId !== this.fft?.streamId || f.codec !== Codec.FFT_U8_DB) return;
      const p = parseFFTU8(f.payload);
      if (!p) return;
      this.fft.dbMin = p.dbMin;
      this.fft.dbStep = p.dbStep;
      // Copy: the frame buffer is not kept, the history is.
      const bins = p.bins.slice();
      this.history.push(bins);
      if (this.history.length > HISTORY) this.history.shift();
      for (const v of this.views) v.onFFT(bins);
      return;
    }
    if (f.type !== FrameType.AUDIO || !this.audioStream || f.streamId !== this.audioStream.id) return;
    if (gap > 0 || f.flags & Flag.DISCONTINUITY) this.audio.gap(Math.max(gap, 1) * FRAME_MS);
    let pcm;
    if (f.codec === Codec.ADPCM_IMA) {
      const a = parseADPCM(f.payload);
      if (!a) return;
      pcm = new Float32Array(a.data.length * 2);
      decodeADPCM(a.predictor, a.stepIndex, a.data, pcm);
    } else if (f.codec === Codec.PCM_S16LE) {
      const n = f.payload.byteLength >> 1;
      const dv = new DataView(f.payload.buffer, f.payload.byteOffset, n * 2);
      pcm = new Float32Array(n);
      for (let i = 0; i < n; i++) pcm[i] = dv.getInt16(i * 2, true) / 32768;
    } else {
      return; // Opus is not negotiated in M1a.
    }
    if (f.flags & Flag.SQUELCHED) pcm.fill(0);
    this.audio.push(pcm, this.audioStream.rate);
  }

  /** startAudio creates or resumes the audio output; call it from a click (RX-004). */
  async startAudio() {
    await this.audio.start();
    this.emit("audio");
  }

  async stopAudio() {
    await this.audio.suspend();
    this.emit("audio");
  }

  /** @param {number} v 0..1 */
  setVolume(v) {
    this.audio.setVolume(v);
    this.emit("audio");
  }

  /** @param {boolean} m */
  setMuted(m) {
    this.audio.setMuted(m);
    this.emit("audio");
  }

  /** centerHz is the device centre frequency, 0 before device.config. */
  get centerHz() {
    return this.device?.center_hz ?? 0;
  }

  /** tunedHz is the demodulator frequency, 0 before demod.create. */
  get tunedHz() {
    return this.demod ? this.centerHz + this.demod.offsetHz : 0;
  }

  /** @returns {[number, number]} the capture band, in Hz */
  band() {
    const half = Math.floor((this.device?.sample_rate || 0) / 2);
    return [this.centerHz - half, this.centerHz + half];
  }

  /** @returns {number} the tuning step in use (Hz, RX-012) */
  tuningStep() {
    return this.stepHz || this.device?.tuning_step_hz || 1;
  }

  /** @param {number} hz the tuning step, 0: the device's (RX-012) */
  setStep(hz) {
    this.stepHz = Math.max(0, Math.round(hz));
    this.emit("tune");
  }

  /**
   * tune moves the demodulator to hz (RX-008 click/drag, RX-009 steps),
   * snapped to the tuning step and kept inside the capture band.
   * Sent at most every TUNE_INTERVAL_MS; the last position wins.
   * @param {number} hz
   * @param {boolean} [snap] false: direct entry (RX-011), not snapped, and
   *   refused outside the capture band instead of clamped
   * @returns {boolean} false when the frequency was refused
   */
  tune(hz, snap = true) {
    if (!this.demod || !this.device) return false;
    const step = this.tuningStep();
    const half = Math.floor((this.device.sample_rate || 0) / 2);
    const wanted = Math.round(snap ? Math.round(hz / step) * step : hz) - this.centerHz;
    if (!snap && Math.abs(wanted) > half) return false;
    const offset = Math.min(half, Math.max(-half, wanted));
    // The visitor tuned elsewhere: a linked frequency is no longer wanted.
    this.outOfBand = null;
    if (offset === this.demod.offsetHz) return true;
    this.demod.offsetHz = offset;
    this.kept.offset_hz = offset;
    this.emit("tune");
    this.queue({ offset_hz: offset });
    return true;
  }

  /** @param {number} steps tune by this many tuning steps (RX-009) */
  step(steps) {
    if (!this.demod || !this.device) return;
    this.tune(this.tunedHz + steps * this.tuningStep());
  }

  /**
   * setBandpass sets the pass band edges relative to the tuned frequency
   * (RX-020); the node clamps them to the mode's limits.
   * @param {number} lowHz @param {number} highHz
   */
  setBandpass(lowHz, highHz) {
    const d = this.demod;
    if (!d) return;
    const low = Math.round(Math.min(lowHz, highHz - MIN_BANDWIDTH_HZ));
    const high = Math.round(Math.max(highHz, low + MIN_BANDWIDTH_HZ));
    if (low === d.lowHz && high === d.highHz) return;
    d.lowHz = low;
    d.highHz = high;
    this.kept.bandpass = { low_hz: low, high_hz: high };
    saveBandpass(d.mode, low, high);
    this.emit("tune");
    this.queue({ bandpass: { low_hz: low, high_hz: high } });
  }

  /**
   * setSquelch sets the squelch level in dBFS (RX-023); null opens it.
   * @param {number | null} db
   */
  setSquelch(db) {
    const d = this.demod;
    if (!d) return;
    const level = db === null ? SQUELCH_MIN_DB : Math.round(Math.min(SQUELCH_MAX_DB, Math.max(SQUELCH_MIN_DB, db)));
    d.squelchDb = level === SQUELCH_MIN_DB ? null : level;
    this.kept.squelch_db = level;
    this.emit("tune");
    this.queue({ squelch_db: level });
  }

  /**
   * autoSquelch sets the squelch once from the current signal level plus
   * the device's constant margin (RX-024).
   * @returns {number | null} the level set, null without a meter reading
   */
  autoSquelch() {
    if (!this.demod || !this.meter) return null;
    const margin = this.device?.squelch?.auto_margin || SQUELCH_AUTO_MARGIN_DB;
    const level = Math.round(Math.min(SQUELCH_MAX_DB, Math.max(SQUELCH_MIN_DB + 1, this.meter.levelDb + margin)));
    this.setSquelch(level);
    return level;
  }

  /**
   * setNR sets the noise reduction (RX-027).
   * @param {boolean} enabled @param {number} threshold dB
   */
  setNR(enabled, threshold) {
    const d = this.demod;
    if (!d) return;
    const nr = { enabled, threshold: Math.round(Math.min(NR_MAX_DB, Math.max(NR_MIN_DB, threshold))) };
    d.nr = nr;
    this.kept.nr = nr;
    this.emit("tune");
    this.queue({ nr });
  }

  /**
   * selectPreset switches the device's shared preset (RX-006); the node
   * sends every listener the new device.config.
   * @param {string} id
   * @returns {Promise<boolean>} false when refused
   */
  async selectPreset(id) {
    return this.shared("preset.select", { preset_id: id }, "switch the preset");
  }

  /**
   * retune moves the device's shared centre to hz (RX-010); every listener
   * keeps its frequency while it stays in the band.
   * @param {number} hz
   * @returns {Promise<boolean>} false when refused
   */
  async retune(hz) {
    return this.shared("device.retune", { center_hz: Math.round(hz) }, "move the centre");
  }

  /**
   * shared sends a change of the device's shared state; a refusal becomes
   * the state detail.
   * @param {string} type @param {object} payload @param {string} action
   * @returns {Promise<boolean>}
   */
  async shared(type, payload, action) {
    const t = this.target;
    if (!t || !this.device) return false;
    const gen = this.gen;
    this.ownShared = Date.now();
    try {
      await this.request(type, { device_id: t.device_id, ...payload });
      this.ownShared = Date.now();
      if (gen === this.gen && this.detail && this.state === "listening") this.setState("listening");
      return true;
    } catch (err) {
      if (gen !== this.gen) return false;
      this.detail = refusal(err, action);
      this.emit("state");
      return false;
    }
  }

  /** @returns {string} the codec of the audio stream */
  audioCodec() {
    return this.audioStream?.codec || this.codec;
  }

  /**
   * setMode switches the demodulator mode (RX-007); the node applies the
   * mode's default pass band. Broadcast FM needs the 48 kHz audio stream:
   * the audio is reconfigured before switching to it and after leaving it.
   * @param {string} mode
   */
  async setMode(mode) {
    const d = this.demod;
    if (!d || mode === d.mode) return;
    const gen = this.gen;
    const rate = HD_MODES.has(mode) ? HD_AUDIO_RATE : AUDIO_RATE;
    try {
      if (rate > (this.audioStream?.rate ?? AUDIO_RATE)) {
        await this.request("audio.configure", { codec: this.audioCodec(), sample_rate: rate });
        if (gen !== this.gen) return;
      }
      delete this.queued.bandpass;
      // The pass band saved for the new mode (RX-021), else its default.
      const saved = savedBandpass(mode);
      /** @type {Record<string, any>} */
      const set = { demod_id: d.id, mode };
      if (saved) set.bandpass = saved;
      const res = await this.request("demod.set", set);
      if (gen !== this.gen || this.demod !== d) return;
      this.kept.mode = mode;
      if (saved) this.kept.bandpass = saved;
      else delete this.kept.bandpass;
      this.applied(res?.applied ?? {});
      if (rate < (this.audioStream?.rate ?? AUDIO_RATE)) {
        await this.request("audio.configure", { codec: this.audioCodec(), sample_rate: rate });
      }
    } catch (err) {
      if (gen !== this.gen) return;
      this.detail = err?.message ?? "";
      this.emit("state");
    }
  }

  /**
   * setDecoder starts the decoder of a digital mode on the demodulator, or
   * stops it (null) (DEC-002). The node switches the demodulator to the
   * mode's default underlying mode when the current one is not allowed
   * (DEC-003). variant picks the decoder variant (default: the mode's).
   * @param {string | null} mode @param {string} [variant]
   */
  async setDecoder(mode, variant) {
    const d = this.demod;
    if (!d) return;
    const gen = this.gen;
    try {
      /** @type {Record<string, any>} */
      const req = { demod_id: d.id, decoder: mode };
      if (mode && variant) req.options = { variant };
      const res = await this.request("decoder.set", req);
      if (gen !== this.gen || this.demod !== d) return;
      if (res?.variant) this.kept.variant = res.variant;
      const a = res?.applied ?? {};
      if (a.mode && a.mode !== this.kept.mode) {
        this.kept.mode = a.mode;
        delete this.kept.bandpass;
      }
      this.applied(a);
      if (HD_MODES.has(d.mode) && (this.audioStream?.rate ?? AUDIO_RATE) < HD_AUDIO_RATE) {
        await this.request("audio.configure", { codec: this.audioCodec(), sample_rate: HD_AUDIO_RATE });
      }
    } catch (err) {
      if (gen !== this.gen) return;
      delete this.kept.decoder;
      this.detail = err?.message ?? "";
      this.emit("state");
      if (this.detail) this.signal("message", { text: this.detail });
    }
  }

  /**
   * queue merges fields into the next demod.set, sent at most every
   * TUNE_INTERVAL_MS (msg_rate, §6.9); the last value of a field wins.
   * @param {Record<string, any>} fields
   */
  queue(fields) {
    Object.assign(this.queued, fields);
    if (!this.tuneTimer) this.flushQueue();
  }

  flushQueue() {
    const d = this.demod;
    const fields = this.queued;
    this.queued = {};
    if (!d || Object.keys(fields).length === 0) {
      this.tuneTimer = 0;
      return;
    }
    const gen = this.gen;
    this.request("demod.set", { demod_id: d.id, ...fields }).then(
      (res) => {
        if (gen === this.gen && this.demod === d) this.applied(res?.applied ?? {});
      },
      (err) => {
        if (gen !== this.gen) return;
        this.detail = err?.message ?? "";
        this.emit("state");
      },
    );
    this.tuneTimer = setTimeout(() => this.flushQueue(), TUNE_INTERVAL_MS);
  }

  /**
   * applied keeps the demodulator values in force (clamped by the node)
   * from a demod.create or demod.set ack. A field with a newer change
   * waiting to be sent keeps its local value, so a drag never jumps back.
   * The offset is not read back: the node refuses one outside the band
   * rather than clamping it.
   * @param {any} a Applied
   */
  applied(a) {
    const d = this.demod;
    if (!d) return;
    const q = this.queued;
    if (a.mode) d.mode = a.mode;
    if (a.bandpass && !("bandpass" in q)) {
      d.lowHz = a.bandpass.low_hz;
      d.highHz = a.bandpass.high_hz;
    }
    if (!("squelch_db" in q)) {
      const sq = typeof a.squelch_db === "number" ? a.squelch_db : null;
      d.squelchDb = sq === null || sq <= SQUELCH_MIN_DB ? null : sq;
    }
    if (a.nr && !("nr" in q)) d.nr = { enabled: !!a.nr.enabled, threshold: a.nr.threshold ?? 0 };
    if ("decoder" in a) {
      d.decoder = a.decoder ?? null;
      if (d.decoder) this.kept.decoder = d.decoder;
      else {
        delete this.kept.decoder;
        delete this.kept.variant;
      }
    }
    this.emit("tune");
    if (this.detail && this.state === "listening") this.setState("listening");
  }

  onVisibility() {
    // §6.8: hidden tabs SHOULD pause FFT streams; audio keeps playing.
    if (!this.fft) return;
    this.request("stream.configure", { stream_id: this.fft.streamId, paused: document.hidden }).catch(() => {});
  }

  /** @param {View} v */
  attachView(v) {
    this.views.add(v);
  }

  /** @param {View} v */
  detachView(v) {
    this.views.delete(v);
  }
}

/**
 * refusal is the short text of a refused shared change.
 * @param {any} err error payload @param {string} action
 */
function refusal(err, action) {
  switch (err?.code) {
    case "forbidden":
      return `not allowed to ${action} now`;
    case "rate_limited":
      return `${action} again in ${Math.max(1, Math.ceil((err.retry_after_ms ?? 1000) / 1000))} s`;
    case "preset_incompatible":
      return "this preset does not fit the device";
    default:
      return err?.message || `could not ${action}`;
  }
}

/** @type {Engine | null} */
let engine = null;

/** getEngine returns the page's engine, created on first use. */
export function getEngine() {
  engine ??= new Engine();
  return engine;
}
