// CSRF client (AUTH-019, ADR 0003 §4C): the token comes only from
// GET /api/v1/auth/session (session-bound, or bound to the pre-session cookie
// for anonymous visitors) and is sent in X-CSRF-Token on every unsafe
// same-origin request, from htmx and from islands (apiFetch).
//
// The token is fetched lazily, cached for the page's lifetime and fetched
// again once after a 403 csrf_failed (expired or rotated session). Login and
// logout are full page loads, so the cache starts fresh.
//
// Importing this module installs the htmx hook once (ES modules are
// evaluated once per page), so the shell and pages may both import it.

const UNSAFE_METHODS = new Set(["POST", "PUT", "PATCH", "DELETE"]);
const CSRF_HEADER = "X-CSRF-Token";
const SESSION_URL = "/api/v1/auth/session";

let pending = null;

// csrfToken resolves to the current CSRF token; refresh forces a new fetch.
export function csrfToken(refresh = false) {
  if (refresh || pending === null) {
    pending = fetch(SESSION_URL, {
      credentials: "same-origin",
      cache: "no-store",
      headers: { Accept: "application/json" },
    })
      .then((res) => {
        if (!res.ok) {
          throw new Error(`session API answered ${res.status}`);
        }
        return res.json();
      })
      .then((body) => body.csrf_token);
    pending.catch(() => {
      pending = null;
    });
  }
  return pending;
}

function isSameOrigin(url) {
  try {
    return new URL(url, location.href).origin === location.origin;
  } catch {
    return false;
  }
}

function isUnsafe(method) {
  return UNSAFE_METHODS.has((method ?? "GET").toUpperCase());
}

// csrfRejected reports whether a response is the CSRF failure (403 with the
// problem code csrf_failed), without consuming the body.
async function csrfRejected(res) {
  if (res.status !== 403) {
    return false;
  }
  try {
    const body = await res.clone().json();
    return body?.code === "csrf_failed";
  } catch {
    // HTML endpoints answer the shell 403 page: retry once anyway.
    return true;
  }
}

// withToken runs send(token) and retries once with a fresh token when the
// server rejects the token.
async function withToken(send) {
  const res = await send(await csrfToken());
  if (!(await csrfRejected(res))) {
    return res;
  }
  return send(await csrfToken(true));
}

// apiFetch is fetch() for islands: same-origin credentials, and the CSRF
// token on unsafe same-origin requests.
export function apiFetch(input, init = {}) {
  const url = input instanceof Request ? input.url : String(input);
  const method = init.method ?? (input instanceof Request ? input.method : "GET");
  if (!isUnsafe(method) || !isSameOrigin(url)) {
    return fetch(input, { ...init, credentials: "same-origin" });
  }
  return withToken((token) => {
    const headers = new Headers(init.headers);
    headers.set(CSRF_HEADER, token);
    return fetch(input, { ...init, headers, credentials: "same-origin" });
  });
}

// getJSON reads a JSON resource with the same-origin credentials; it throws
// when the server answers an error status.
export async function getJSON(url, { signal } = {}) {
  const res = await fetch(url, { credentials: "same-origin", headers: { Accept: "application/json" }, signal });
  if (!res.ok) throw new Error(`${url} answered ${res.status}`);
  return res.json();
}

// installCSRF hooks htmx 4 requests. htmx events cannot be awaited, so the
// hook wraps the request's fetch function (ctx.fetch), which htmx awaits.
function installCSRF() {
  document.addEventListener("htmx:before:request", (evt) => {
    const ctx = evt.detail.ctx;
    const req = ctx.request;
    if (!isUnsafe(req.method) || !isSameOrigin(req.action)) {
      return;
    }
    const send = ctx.fetch ?? window.fetch.bind(window);
    ctx.fetch = (action, init) =>
      withToken((token) => {
        init.headers = { ...init.headers, [CSRF_HEADER]: token };
        return send(action, init);
      });
  });
}

installCSRF();
