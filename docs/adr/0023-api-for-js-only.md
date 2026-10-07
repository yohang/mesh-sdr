# ADR 0023: JSON API only for scripts, islands and nodes

- Status: Accepted
- Date: 2026-10-07
- Deciders: project owner
- Amends: ADR 0013 (parity rule, setup API, contract suite) and ADR 0018 (API twins of Admin › Nodes)

## Context

ADR 0013 required every state-changing HTML form to have an `/api/v1` twin (`TestHTMLActionsHaveAPITwins`), and ADR 0018 extended it to Admin › Nodes. Pages never call those twins: htmx posts the forms and the scripts only read the CSRF token. Each UI action therefore existed twice, with two handlers, two sets of tests and a 3 000-line OpenAPI document to keep in step.

## Decision

1. **Scope of `/api/v1`.** The API holds only what scripts, islands, nodes and operations use, plus the resources pages link to. UI actions are HTML forms on page-scoped paths (ADR 0003) and have no API twin. The parity rule, `htmlActions` and `TestHTMLActionsHaveAPITwins` are removed.
2. **Kept operations.**
   - `getOpenAPI`, `getLiveness`, `getReadiness` (operations, the accessibility job);
   - `getSession` (CSRF token of `csrf.js`);
   - `mintAccessToken` (media token refresh, ACC-007) and `getFeatures` (API-001, receiver island);
   - `getReceiverImage` (the `<img>` of the Receiver page and Admin › Site) and `getEffectiveConfig` (the download link of Admin › System);
   - `listPresets`, `createPreset`, `getPreset`, `replacePreset`, `deletePreset`, `listSchedules`, `createSchedule`, `getSchedule`, `replaceSchedule`, `deleteSchedule`: ADR 0020 has no pages for them yet, so the API is their only entry point. The owner keeps them until their HTML pages land; then they follow this rule.
   - Outside `/api/v1`, unchanged: `GET /.well-known/jwks.json` (nodes) and `/api/ws` (events).
3. **Removed operations** (58): login, logout, logout-all, password change, first-admin setup, e-mail change and confirmation, `/me*`, users, roles, sessions, invitations, test e-mail, password resets, audit search, settings (list, patch, delete, schema, public), retention, receiver image upload and removal, nodes, node capabilities and probe, devices, connections. Their HTML pages remain the only way to perform them.
4. **Unchanged.** One error format: every path under `/api` answers problem+json (`internal/http/problem`), including the removed paths (404). JSON-only bodies, closed request schemas, the `x-meshsdr-access` policy and the guard before body reads apply to the remaining operations. The contract suite keeps the rights matrix and a validated 2xx for every remaining operation.
5. **Tests.** Behaviours the API tests covered (role and admin-network checks, CSRF, last admin, version conflicts, session ownership, rate limits, unknown body fields) are covered through the HTML forms or the remaining operations.

## Consequences

- About 5 000 lines less in the repository (OpenAPI document, handlers, tests) and 11 800 less in the generated server and `openapi.json`. No authorisation gap was found: every removed operation's HTML counterpart applies the same gate (`Require`, admin network), CSRF and application service.
- "Sign out everywhere, this session included" (`/auth/logout-all`) moves to the account page: `POST /account/sessions/revoke-all` revokes every session of the user, publishes the revocation like a logout, clears the cookie and redirects to `/login`.
- Lost with the API: the anonymous connection count of `GET /connections`, the public settings document and the admin edit of a user's display name. A script or island that needs one adds the operation back, with its consumer.
- Spec divergence, recorded here (the spec is not edited): the endpoints FEATURE_SPEC and TECHNICAL_SPEC §6.10 name for UI actions are HTML forms.
