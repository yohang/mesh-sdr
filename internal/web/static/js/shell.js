// App shell entry point, loaded once by the layout as a nonce'd module. It lives
// outside #main, so it survives boosted navigation. Its imports inherit the nonce
// (module graph), so the CSP needs no 'self' in script-src. No inline scripts, no eval.
//
// Islands are custom elements: the browser upgrades them whenever htmx inserts them
// (navigation, history restore, fragments) and disconnectedCallback cleans them up.

import { installCSRF } from "./csrf.js";
import { installNavigation } from "./navigation.js";
import { AudioProbe } from "./islands/audio-probe.js";
import { ReceiverIsland } from "./islands/receiver.js";
import { UtcClock } from "./islands/utc-clock.js";

installCSRF();
installNavigation();

customElements.define("msdr-utc-clock", UtcClock);
customElements.define("msdr-receiver-island", ReceiverIsland);
customElements.define("msdr-audio-probe", AudioProbe);
