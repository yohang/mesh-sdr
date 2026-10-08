// App shell entry point, loaded once by the layout as a nonce'd ES module. It
// lives outside #main, so it survives boosted navigation. The modules it imports
// inherit its nonce, so the CSP needs no 'self' in script-src. No inline
// scripts, no eval, no hx-on (ADR 0003).
//
// Islands are custom elements defined here: the browser upgrades them whenever
// htmx inserts them (navigation, history restore, fragments), and their
// disconnectedCallback cleans up.

import "./csrf.js";
import { installAdminForms } from "./admin-form.js";
import "./utc-clock.js";
import "./sortable-list.js";
import "./token-url.js";
import "./download.js";
import "./device-log.js";
import { installNavigation } from "./navigation.js";
import { installShortcuts } from "./shortcuts.js";
import { installEvents } from "./events.js";
// Receiver island and shell audio dock (ADR 0015): they share the engine, a
// module singleton outside #main, so audio survives boosted navigation.
import "./receiver/dock.js";
import "./receiver/island.js";

installNavigation();
installShortcuts();
installAdminForms();
installEvents();
