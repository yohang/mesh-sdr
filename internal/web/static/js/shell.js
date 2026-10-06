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
import "./token-url.js";
import { installNavigation } from "./navigation.js";

installNavigation();
installAdminForms();
