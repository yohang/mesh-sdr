// Design tokens for JS and canvases (UI-008). Colors are registered with
// @property, so getComputedStyle returns a resolved rgb() value whatever the
// theme mode.
//
//   import { readToken, onThemeChange } from "/static/js/tokens.js";
//   const fg = readToken("color-fg");
//   const stop = onThemeChange(() => redraw());

// readToken returns the computed value of --msdr-<name> on el (the root
// element by default).
export function readToken(name, el = document.documentElement) {
  return getComputedStyle(el).getPropertyValue(`--msdr-${name}`).trim();
}

// onThemeChange calls callback when the used color scheme changes, so
// canvases re-read their tokens. That only happens in auto mode, when the OS
// switches between light and dark: an admin change of the theme mode applies
// on the next full page load. It returns a function that unsubscribes.
export function onThemeChange(callback) {
  const query = matchMedia("(prefers-color-scheme: dark)");
  const listener = () => {
    if (document.documentElement.dataset.theme === "auto") {
      callback();
    }
  };
  query.addEventListener("change", listener);
  return () => query.removeEventListener("change", listener);
}
