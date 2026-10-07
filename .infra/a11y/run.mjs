// Accessibility check of the running hub (UI-009, ADR 0007): axe-core with the
// WCAG 2.0/2.1 A and AA rules on every page of urls.txt, for each theme mode
// (one hub per mode: auto, light, dark), in the light and dark OS color
// schemes, at a phone and a desktop viewport. It also checks that the theme
// mode is applied, and fails on any Content Security Policy or
// Permissions-Policy console message, page error, failed request or
// subresource answering >= 400. Finally it checks the shell's boosted
// navigation (same document, focus on #main, announcement, title), and a
// rejected admin form (inline errors and summary) once signed in.
//
// Usage: HUBS="auto=http://hub-auto:8073 light=http://hub-light:8073" node run.mjs

import { readFile } from "node:fs/promises";
import { chromium } from "playwright";
import { AxeBuilder } from "@axe-core/playwright";

const hubs = (process.env.HUBS ?? "auto=http://hub-auto:8073")
  .split(/\s+/)
  .filter(Boolean)
  .map((h) => {
    const [mode, url] = h.split("=");
    return { mode, url };
  });
const tags = ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"];
const schemes = ["light", "dark"];
const viewports = {
  phone: { width: 390, height: 844 },
  desktop: { width: 1280, height: 800 },
};
// --msdr-color-bg of each token set (static/css/input.css).
const background = { light: "rgb(246, 247, 249)", dark: "rgb(13, 17, 23)" };

const pages = (await readFile(new URL("urls.txt", import.meta.url), "utf8"))
  .split("\n")
  .map((l) => l.trim())
  .filter((l) => l && !l.startsWith("#"))
  .map((l) => {
    const [path, status, access] = l.split(/\s+/);
    return { path, status: Number(status), signedIn: access === "signed-in" };
  });

// Account seeded by the compose file (service seed), used for the pages
// marked "signed-in".
const account = { login: process.env.A11Y_USER ?? "a11y", password: process.env.A11Y_PASSWORD ?? "" };

// signIn signs the browser context in through the login page.
async function signIn(context, where) {
  const page = await context.newPage();
  watch(page, where);
  await page.goto("/login", { waitUntil: "networkidle" });
  await page.getByLabel("Username or e-mail").fill(account.login);
  await page.getByLabel("Password", { exact: true }).fill(account.password);
  await Promise.all([
    page.waitForURL((u) => !u.pathname.startsWith("/login")),
    page.getByRole("button", { name: "Sign in" }).click(),
  ]);
  await page.close();
}

// checkFormErrors submits an invalid admin form (signed in as the seeded
// admin) and audits the error state: inline errors and summary.
async function checkFormErrors(context, where) {
  const page = await context.newPage();
  watch(page, where, ["422 /admin/access"]);
  await page.goto("/admin/access", { waitUntil: "networkidle" });
  await page.getByLabel("Idle timeout").fill("1m");
  await page.getByRole("button", { name: "Save Sessions" }).click();
  await page.getByRole("alert").waitFor();
  if ((await page.getByLabel("Idle timeout").getAttribute("aria-invalid")) !== "true") {
    fail(where, "the invalid field is not marked aria-invalid");
  }
  await audit(page, where);
  await page.close();
}

let failures = 0;
const fail = (where, msg) => {
  failures++;
  console.error(`FAIL ${where}: ${msg}`);
};

async function waitForHub(url) {
  const deadline = Date.now() + 60_000;
  for (;;) {
    try {
      const res = await fetch(`${url}/api/v1/healthz/ready`);
      if (res.ok) return;
    } catch {
      // not up yet
    }
    if (Date.now() > deadline) throw new Error(`hub not ready at ${url}`);
    await new Promise((r) => setTimeout(r, 1000));
  }
}

// watch fails on policy violations, page errors and broken subresources.
// expected lists the "<status> <path>" answers a check provokes on purpose.
function watch(page, where, expected = []) {
  page.on("console", (msg) => {
    const text = msg.text();
    if (text.includes("Content Security Policy") || text.includes("Permissions-Policy")) {
      fail(where, `console: ${text}`);
    }
  });
  page.on("pageerror", (err) => fail(where, `page error: ${err.message}`));
  page.on("requestfailed", (req) => fail(where, `request failed: ${req.url()} (${req.failure()?.errorText})`));
  page.on("response", (res) => {
    const answer = `${res.status()} ${new URL(res.url()).pathname}`;
    if (res.status() >= 400 && !res.request().isNavigationRequest() && !expected.includes(answer)) {
      fail(where, `subresource ${res.url()} answered ${res.status()}`);
    }
  });
}

async function audit(page, where) {
  const { violations } = await new AxeBuilder({ page }).withTags(tags).analyze();
  for (const v of violations) {
    const targets = v.nodes.map((n) => n.target.join(" ")).join(", ");
    fail(where, `${v.id} (${v.impact}): ${v.help} [${targets}] ${v.helpUrl}`);
  }
}

// checkTheme verifies the theme mode attribute and the token set in use.
async function checkTheme(page, where, mode, scheme) {
  const got = await page.evaluate(() => ({
    theme: document.documentElement.dataset.theme,
    bg: getComputedStyle(document.body).backgroundColor,
  }));
  const effective = mode === "auto" ? scheme : mode;
  if (got.theme !== mode) fail(where, `data-theme = ${got.theme}, want ${mode}`);
  if (got.bg !== background[effective]) fail(where, `background ${got.bg}, want ${background[effective]} (${effective})`);
}

// checkEvents opens a live page as a signed-in admin: the shell opens the
// hub events socket (ADR 0016), subscribes to the topics the page declares,
// and the live table refetches itself after the ack as a background request
// (X-Msdr-Background), under the strict CSP.
async function checkEvents(context, where) {
  const page = await context.newPage();
  watch(page, where);

  const acked = new Promise((resolve) => {
    page.on("websocket", (ws) => {
      if (!ws.url().endsWith("/api/ws")) return;
      ws.on("framereceived", (frame) => {
        try {
          const env = JSON.parse(frame.payload);
          if (env.type === "ack" && env.payload?.result?.subscribed?.includes("nodes")) resolve(true);
        } catch {
          // not an envelope
        }
      });
    });
  });
  const refetched = page
    .waitForRequest((req) => new URL(req.url()).pathname === "/admin/nodes" && req.headers()["x-msdr-background"] === "1", {
      timeout: 15000,
    })
    .then(() => true)
    .catch(() => false);
  const timeout = new Promise((resolve) => setTimeout(() => resolve(false), 15000));

  await page.goto("/admin/nodes", { waitUntil: "domcontentloaded" });

  if (!(await Promise.race([acked, timeout]))) fail(where, "the events socket did not subscribe to nodes");
  if (!(await refetched)) fail(where, "the live table did not refetch after the subscription");

  await page.close();
}

// Help link set on every hub by the compose file.
const helpPath = "/about";

// checkShellControls checks the top bar as a signed-in admin: key H opens
// the help link in a new tab, the user menu opens, passes axe and closes
// with Escape, the section nav is a bottom tab bar on phones and an inline
// menu on desktops, and boosted navigation through it marks the current
// section.
async function checkShellControls(url, mode, scheme, name, viewport) {
  const where = `shell controls [mode ${mode}, os ${scheme}, ${name}]`;
  const context = await browser.newContext({ baseURL: url, colorScheme: scheme, viewport });
  await signIn(context, where);

  const page = await context.newPage();
  watch(page, where);
  await page.goto("/", { waitUntil: "networkidle" });

  const [popup] = await Promise.all([context.waitForEvent("page"), page.keyboard.press("h")]);
  await popup.waitForLoadState("domcontentloaded");
  if (new URL(popup.url()).pathname !== helpPath) fail(where, `H opened ${popup.url()}`);
  await popup.close();

  await page.getByRole("button", { name: /^Account menu for/ }).click();
  const menu = page.locator("#msdr-user-menu");
  await menu.waitFor({ state: "visible" });
  await audit(page, `${where} (user menu open)`);
  await page.keyboard.press("Escape");
  await menu.waitFor({ state: "hidden" });

  const nav = await page.locator('nav[aria-label="Main"]').boundingBox();
  const atBottom = nav && Math.abs(nav.y + nav.height - viewport.height) <= 1;
  if (name === "phone" && !atBottom) fail(where, `nav is not a bottom tab bar: ${JSON.stringify(nav)}`);
  if (name === "desktop" && (!nav || nav.y > 64)) fail(where, `nav is not in the top bar: ${JSON.stringify(nav)}`);

  await page.getByRole("link", { name: "Map", exact: true }).click();
  await page.waitForURL("**/map");
  await page.waitForFunction(
    () => document.querySelector('nav[aria-label="Main"] a[data-section="map"]')?.getAttribute("aria-current") === "page",
  );
  const current = await page.locator('nav[aria-label="Main"] a[aria-current="page"]').count();
  if (current !== 1) fail(where, `${current} sections marked current after navigation`);

  await context.close();
}

const browser = await chromium.launch();

for (const { mode, url } of hubs) {
  await waitForHub(url);

  for (const scheme of schemes) {
    for (const [name, viewport] of Object.entries(viewports)) {
      const context = await browser.newContext({ baseURL: url, colorScheme: scheme, viewport });

      let signedIn = false;

      // Anonymous pages first, then the signed-in ones in the same context.
      for (const { path, status, signedIn: needsSession } of [...pages].sort((a, b) => a.signedIn - b.signedIn)) {
        const where = `${path} [mode ${mode}, os ${scheme}, ${name}]`;
        if (needsSession && !signedIn) {
          await signIn(context, `sign-in [mode ${mode}, os ${scheme}, ${name}]`);
          signedIn = true;
        }

        const page = await context.newPage();
        watch(page, where);

        const res = await page.goto(path, { waitUntil: "networkidle" });
        if (res?.status() !== status) fail(where, `status ${res?.status()}, want ${status}`);

        await checkTheme(page, where, mode, scheme);
        await audit(page, where);
        await page.close();
      }

      if (signedIn) {
        await checkFormErrors(context, `/admin/access invalid form [mode ${mode}, os ${scheme}, ${name}]`);
        await checkEvents(context, `/admin/nodes live events [mode ${mode}, os ${scheme}, ${name}]`);
      }

      // Boosted navigation keeps the shell, moves focus to #main and
      // announces the new page; the swapped page must pass axe too. It runs
      // in a fresh anonymous context.
      await context.close();
      const navContext = await browser.newContext({ baseURL: url, colorScheme: scheme, viewport });
      const where = `/ → /policy boosted [mode ${mode}, os ${scheme}, ${name}]`;
      const page = await navContext.newPage();
      watch(page, where);
      await page.goto("/", { waitUntil: "networkidle" });
      await page.evaluate(() => {
        window.__shellMarker = true;
      });
      await page.getByRole("link", { name: "Usage policy" }).click();
      await page.waitForURL("**/policy");
      await page.waitForFunction(() => document.activeElement?.id === "main");

      const state = await page.evaluate(() => ({
        sameDocument: window.__shellMarker === true,
        announced: document.getElementById("msdr-announcer")?.textContent,
        title: document.title,
      }));
      if (!state.sameDocument) fail(where, "navigation reloaded the page");
      if (state.announced !== "Usage policy") fail(where, `announcer = ${JSON.stringify(state.announced)}`);
      if (!state.title.startsWith("Usage policy")) fail(where, `title = ${JSON.stringify(state.title)}`);

      await audit(page, where);
      await page.close();
      await navContext.close();

      await checkShellControls(url, mode, scheme, name, viewport);
    }
  }
}

await browser.close();

if (failures > 0) {
  console.error(`${failures} accessibility check(s) failed`);
  process.exit(1);
}
console.log(
  `accessibility: ${pages.length} pages × ${hubs.length} theme modes × ${schemes.length} OS schemes × ${Object.keys(viewports).length} viewports passed`,
);
