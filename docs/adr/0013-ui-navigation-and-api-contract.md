# ADR 0013: App shell navigation and REST API contract

- Status: Accepted
- Date: 2026-10-06
- Deciders: project owner
- Scope: epic part `epic/ui-2`, the remaining UI and API tickets of M0: UI-006 #75, UI-010 #79, UI-002 #71, RX-001 #84, API-002 #83, API-001 #82, and RX-034 #193.

## Context

ADR 0003 and ADR 0007 built the app shell: boosted navigation that swaps `#main`, fragments on `HX-Request`, a nonce-only CSP with `hx-on` banned, `light-dark()` tokens, a bottom tab bar on mobile and axe-core in CI. Other parts in flight touched the top bar:

- `epic/adm-1` (ADR 0010) added an "Admin" link for admins and the settings `receiver.help_url`, `ui.shortcut_set` and `listen_policy`;
- `epic/acc-1` (ADR 0011) adds a user slot: Sign in, Account, Sign out.

API-002 asks that every browser and admin operation exist under `/api/v1`, described by `openapi.json`, with one error format. ADR 0003 puts htmx actions on page-scoped paths, so both must coexist. API-001 asks for a public feature summary next to the admin capability report from `epic/grid-2` (ADR 0008).

The sections Map, Decodes and Files have no content before M1+.

## Decision

Numbers refer to the questions of the design proposal. The owner accepted every recommendation.

### Delivery (Q1)

- `epic/ui-2` is stacked on `epic/adm-1` and rebased onto `main` as the other parts land.
- Its commits replace adm-1's `Viewer`/Admin-only link with the section navigation.
- They also add the user slot with the shape acc-1 uses (`layout.User`, `ShellUser`).
- Whichever of ui-2 and acc-1 merges second merges the two `ShellUser` methods: acc-1's Account link joins the links of ui-2's menu.

### Navigation (Q2, Q3, Q4, Q11)

- **Section list.** `shell/domain.Section` lists Receiver, Map, Decodes, Files and Admin in the FEATURE_SPEC §10.2 order. `shell/app.Navigation` shows a section only when its gate allows it, and a section without a gate is never shown (fail closed).
- **Gates.** The gates are consumer-side ports supplied at wiring:
  - Admin: identity `Authorize(admin)`, which includes `admin.allowed_networks`;
  - the other sections: open to everyone until the FIL, DEC and MAP modules bring their own policies.
- **Placeholder pages.** `/map`, `/decodes` and `/files` are placeholders in the shell module, each with its heading and a notice. A section's module takes its route over when it lands.
- **Receiver page.** `/` is the Receiver section. It shows the station from Admin › Site:
  - name and location;
  - the avatar and the panorama, linked only when set;
  - the panorama title and the Markdown description (goldmark, raw HTML dropped);
  - a notice that live listening is not available yet.
- **Mobile.** One `<nav aria-label="Main">` is inline in the top bar from 768 px and a fixed bottom tab bar below, in CSS only.
  - Tabs show an icon and a label, with targets of at least 44 px.
  - Icons are hand-drawn inline SVG, decorative (`aria-hidden`). No icon set is vendored.
  - The current section is marked by color, weight and a bar.
- **Logo.** The generated product icon (decorative) sits next to the site name.

### Top bar (Q5, Q6, Q7, Q8)

- **UTC clock.** The server renders `HH:MM UTC` in a `<time datetime>`. The `<msdr-utc-clock>` island updates it on every minute boundary. It is not a live region. This also delivers RX-034.
- **Notifications.** A bell opens the notifications area, empty ("No notifications.") until UI-011 fills it.
- **Popovers.** Menus are native popovers (`popovertarget`): Esc and outside clicks close them, and `navigation.js` closes them after a boosted navigation.
  - They are fixed under the top bar on the right, because CSS anchor positioning is not in every supported browser.
  - They are disclosure lists of links, not `role=menu`.
- **User menu.**
  - Anonymous visitors see "Sign in" (§10.2 wording).
  - Signed-in users see their name and the badge of their highest global role, then:
    - Change password (and Account with acc-1);
    - Help, when set;
    - Usage policy and About;
    - Sign out, a POST to `/logout` answered with `HX-Redirect`.
  - Shortcuts comes with UI-015.
  - Admin is a top-level section, not a menu item.
- **About page.** `/about` shows the software, its version, the `AGPL-3.0-or-later` licence and the source code URL, as AGPL-3.0 section 13 requires. The footer repeats them on every page.
  - The URL defaults to `https://github.com/yohang/mesh-sdr` and can be set at link time with `-ldflags "-X github.com/yohang/mesh-sdr/internal/version.sourceURL=…"`.
  - The repository carries the licence text as `LICENSE`.

### Help and shortcuts (Q9, Q10)

- **Help link.** `receiver.help_url` (a `shell/domain.HelpLink`: an absolute http or https URL) appears in three places: a help button in the top bar, Help in the user menu and Help in the footer.
  - All three open it in a new, unnamed tab (`target="_blank"`, `rel="noopener noreferrer"`, `hx-boost="false"`, "opens in a new tab" for screen readers), never a named window (RX-001).
  - An unset or invalid value hides every entry.
- **Shortcuts.** `static/js/shortcuts.js` has one `keydown` listener. An element opts in with `data-shortcut="<key>"`, and the key activates (clicks) that element. So `H` does exactly what the help button does, and opening the tab counts as a user action.
  - Keys are ignored in text fields, with modifiers, on auto-repeat and during IME composition.
  - `ui.shortcut_set = off` renders `<body data-shortcuts="off">`, which turns them off.
  - The full shortcut set (`F`, `M`, …) stays with UI-014.

### REST API contract (Q12, Q13, Q14)

- **Error format.** Every path under `/api` answers problem+json errors, including unversioned and unknown versions.
- **Parity.** `TestHTMLActionsHaveAPITwins` walks the router and maps every state-changing HTML route to its `/api/v1` operation. A new form without an API twin fails the test.
  - Two forms had no twin and got one: the e-mail confirmation (`POST /api/v1/auth/email/confirm`, anonymous, with the token of the link) and the test e-mail (`POST /api/v1/mail/test`, admin).
  - Two forms map to operations that are not one-to-one. "Sign out other sessions" is `revokeOwnSession` for each session `GET /me/sessions` lists except the current one. The audit export (CSV or JSON) is `searchAudit`, whose results the client formats.
  - `GET /.well-known/jwks.json` stays outside `/api`: token verifiers expect it at that well-known path. It is a read, so the parity test does not see it; the contract suite checks it directly.
- **Setup API.** The first-admin setup page was the only gap. `GET /api/v1/auth/setup/{token}` and `POST /api/v1/auth/setup` close it, with the page's network restriction, rate limit and token redaction.
- **Contract tests.** `internal/http/api/apitest` validates exchanges with kin-openapi against `openapi.yaml`:
  - every response under `/api/v1`: status, content type and body schema;
  - every request the server accepts;
  - the problem+json shape of every error.
- **Hub-level suite.** `internal/wire/contract_test.go` runs the whole hub through `apitest`:
  - every operation is called by anonymous, listener, operator and admin callers, and by an admin outside `admin.allowed_networks`. Below its `x-meshsdr-access` level the call is refused (401 `unauthenticated`, 403 `forbidden` or `admin_network_denied`); at or above it, never. The levels are read from the document, so new operations are covered as they are declared;
  - every operation answers a validated 2xx at least once. The node probe needs a connected node and is covered by the grid end-to-end test. The e-mail confirmation and the test e-mail need mail, which the suite does not configure; the identity HTTP tests cover them.
- **Dependency.** kin-openapi stays a build- and test-time dependency. depguard refuses it outside `specgen`, `apitest` and `_test.go` files, so `meshsdr` never links it.
- **Breaking changes.** The `/api/v2` rule for breaking changes is documented only, pre-1.0. A CI diff against `main` (oasdiff) can come with the first release.

### Feature summary (Q15)

- `GET /api/v1/features` (anonymous) lists the enabled devices the caller may listen to: `id`, `node_id`, `name`, `online` and the available modes.
  - The modes are the node's available `mode:*` capabilities, empty when the node has not reported or reports the device's driver missing.
  - The effective listen policy is the device's node config override, else `listen_policy`. Registered-only devices are hidden from anonymous callers, and an unreadable policy counts as registered.
- Node addresses, versions and missing requirements stay in the admin report `GET /nodes/{id}/capabilities` (ADR 0008), unchanged.
- In M0 nodes report no decoders, so `modes` is empty.

### Accessibility job (Q16)

- `urls.txt` gains `/map`, `/decodes`, `/files`, `/about`, and `/files` signed in.
- The compose file sets a help URL on every hub (a page of the hub) and raises the login rate limit for the checker.
- `run.mjs` checks the shell controls in every mode, scheme and viewport:
  - `H` opens the help link in a new tab;
  - the user menu opens, passes axe and closes with Escape;
  - the nav is a bottom tab bar on phones and in the top bar on desktops;
  - boosted navigation through it marks exactly one current section.

### Tickets (Q17)

The PR closes #75, #79, #71, #84, #83, #82 and #193.

- #79: Shortcuts comes with UI-015 #179.
- #84: Status and Log go to the M1 Info tab.

## Implementation notes

- The contract tests found adm-1 schemas that did not allow `null` (`receiver.gps` in `GET /settings`, `/settings/public`, `/config/effective`, and resets in `PATCH /settings`). They are declared `nullable`.
- `shell.Deps.AdminGate`, `User` and `Images` are wired in `internal/wire`. Identity is wired after the shell, because it renders with the shell renderer, so the gate and the user menu read it once it exists.
- The station images are linked only when `files/app.Branding` has them, so the Receiver page never requests a missing image (the accessibility job fails on subresource errors).

## Divergences from the specification

Recorded here; the spec is not edited.

1. **Login label.** UI-010 says "Log in"; §10.2 and the identity pages say "Sign in". "Sign in" is used.
2. **Feature summary path.** API-001 names `GET /api/v1/features`; §6.10 lists `GET /capabilities` (anonymous summary, admin detail). `/features` is used, and the admin detail stays per node.
3. **User menu contents.** UI-010 (name and role, Change password, Shortcuts, Help, Policy, About, Log out) and §10.2 (Account, Keyboard shortcuts, Sign out) differ. The menu is the union of what exists in M0.
4. **Help placement.** UI-002 puts help in the user menu and on `H`. §10.2 also puts it in the app bar and the footer. All are used, which also covers anonymous visitors, whose menu is only "Sign in".
5. **UTC clock milestone.** RX-034 is M1 and UI-006 (M0) requires the clock. It ships with UI-006.
6. **Section visibility.** §10.2 gates Map, Decodes and Files with site settings and roles that do not exist yet. They are open to everyone until their modules add those settings.

## Consequences

- Feature modules replace the placeholder routes and plug their access policies into the navigation through gates, with no change to the shell templates.
- Every new `/api/v1` operation is covered by the rights matrix and response validation as soon as it is declared, and needs a happy path in the contract suite. Every new HTML action needs an API twin.
- `meshsdr` is licensed AGPL-3.0-or-later and shows where its source code is.
