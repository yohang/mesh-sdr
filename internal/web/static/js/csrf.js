// CSRF integration (TECHNICAL_SPEC §5.7): the session-bound synchronizer token is
// rendered by the shell in <meta name="csrf-token"> and sent in X-CSRF-Token on every
// unsafe same-origin request, from htmx and from islands (apiFetch).

const UNSAFE_METHODS = new Set(["POST", "PUT", "PATCH", "DELETE"]);
const CSRF_HEADER = "X-CSRF-Token";

function csrfToken() {
  return document.querySelector('meta[name="csrf-token"]')?.content ?? "";
}

function isSameOrigin(url) {
  try {
    return new URL(url, location.href).origin === location.origin;
  } catch {
    return false;
  }
}

// URLSearchParams/FormData → JSON object (repeated keys become arrays). No files.
function entriesToObject(body) {
  const out = {};
  for (const [key, value] of body.entries()) {
    if (Object.hasOwn(out, key)) {
      out[key] = [].concat(out[key], value);
    } else {
      out[key] = value;
    }
  }
  return out;
}

// fetch() wrapper for islands: same CSRF rule as htmx requests.
export function apiFetch(input, init = {}) {
  const method = (init.method ?? "GET").toUpperCase();
  const headers = new Headers(init.headers);
  if (UNSAFE_METHODS.has(method) && isSameOrigin(input)) {
    headers.set(CSRF_HEADER, csrfToken());
  }
  return fetch(input, { ...init, headers, credentials: "same-origin" });
}

// installCSRF hooks htmx requests.
//
// htmx:before:request (not htmx:config:request): htmx 4 turns the body into
// URLSearchParams after config:request, so the JSON encoding must run here.
export function installCSRF() {
  document.addEventListener("htmx:before:request", (evt) => {
    const req = evt.detail.ctx.request;
    if (!UNSAFE_METHODS.has(req.method) || !isSameOrigin(req.action)) {
      return;
    }

    req.headers[CSRF_HEADER] = csrfToken();

    // SPIKE: JSON-only bodies (AUTH-019 / TECHNICAL_SPEC §5.7) for every unsafe htmx
    // request. Multipart uploads (Files) would need an opt-out; see ADR 0003.
    if (req.body instanceof URLSearchParams || req.body instanceof FormData) {
      req.headers["Content-Type"] = "application/json";
      req.body = JSON.stringify(entriesToObject(req.body));
    }
  });
}
