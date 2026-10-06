// Accessibility check of the running hub (UI-009, ADR 0007): axe-core with the
// WCAG 2.0/2.1 A and AA rules on every page of urls.txt, in the light and dark
// color schemes (the hub runs in auto theme mode), at a phone and a desktop
// viewport. It also fails on any Content Security Policy violation or page
// error, and checks the shell's post-navigation focus and announcement.
//
// Usage: BASE_URL=http://hub:8073 node run.mjs

import { readFile } from "node:fs/promises";
import { chromium } from "playwright";
import { AxeBuilder } from "@axe-core/playwright";

const baseURL = process.env.BASE_URL ?? "http://hub:8073";
const tags = ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"];
const schemes = ["light", "dark"];
const viewports = {
  phone: { width: 390, height: 844 },
  desktop: { width: 1280, height: 800 },
};

const pages = (await readFile(new URL("urls.txt", import.meta.url), "utf8"))
  .split("\n")
  .map((l) => l.trim())
  .filter((l) => l && !l.startsWith("#"))
  .map((l) => {
    const [path, status] = l.split(/\s+/);
    return { path, status: Number(status) };
  });

let failures = 0;
const fail = (where, msg) => {
  failures++;
  console.error(`FAIL ${where}: ${msg}`);
};

async function waitForHub() {
  const deadline = Date.now() + 60_000;
  for (;;) {
    try {
      const res = await fetch(`${baseURL}/api/v1/healthz/ready`);
      if (res.ok) return;
    } catch {
      // not up yet
    }
    if (Date.now() > deadline) throw new Error(`hub not ready at ${baseURL}`);
    await new Promise((r) => setTimeout(r, 1000));
  }
}

// watch records CSP violations and page errors on page.
function watch(page, where) {
  page.on("console", (msg) => {
    if (msg.text().includes("Content Security Policy")) fail(where, `CSP violation: ${msg.text()}`);
  });
  page.on("pageerror", (err) => fail(where, `page error: ${err.message}`));
}

async function audit(page, where) {
  const { violations } = await new AxeBuilder({ page }).withTags(tags).analyze();
  for (const v of violations) {
    const targets = v.nodes.map((n) => n.target.join(" ")).join(", ");
    fail(where, `${v.id} (${v.impact}): ${v.help} [${targets}] ${v.helpUrl}`);
  }
}

await waitForHub();

const browser = await chromium.launch();

for (const scheme of schemes) {
  for (const [name, viewport] of Object.entries(viewports)) {
    const context = await browser.newContext({ baseURL, colorScheme: scheme, viewport });

    for (const { path, status } of pages) {
      const where = `${path} [${scheme}, ${name}]`;
      const page = await context.newPage();
      watch(page, where);

      const res = await page.goto(path, { waitUntil: "networkidle" });
      if (res?.status() !== status) fail(where, `status ${res?.status()}, want ${status}`);

      await audit(page, where);
      await page.close();
    }

    // Boosted navigation keeps the shell, moves focus to #main and
    // announces the new page; the swapped page must pass axe too.
    const where = `/ → /policy (boosted) [${scheme}, ${name}]`;
    const page = await context.newPage();
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
    await context.close();
  }
}

await browser.close();

if (failures > 0) {
  console.error(`${failures} accessibility check(s) failed`);
  process.exit(1);
}
console.log(`accessibility: ${pages.length} pages × ${schemes.length} schemes × ${Object.keys(viewports).length} viewports passed`);
