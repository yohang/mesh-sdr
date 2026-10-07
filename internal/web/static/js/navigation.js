// Post-navigation accessibility (UI-009): after a boosted navigation or a
// history restore, close the top bar menus, mark the current section in the
// nav, move focus to #main, scroll to the top and announce the new page in
// the polite live region, so keyboard and screen-reader users know the page
// changed.
//
// A boosted navigation replaces #main; a history restore syncs it in place
// after the URL changed, so watch both.
export function installNavigation() {
  let currentMain = document.getElementById("main");
  let currentHref = location.href;

  document.addEventListener("htmx:after:swap", () => {
    const main = document.getElementById("main");
    if (!main || (main === currentMain && location.href === currentHref)) {
      return;
    }
    currentMain = main;
    currentHref = location.href;

    for (const menu of document.querySelectorAll("header [popover]:popover-open")) {
      menu.hidePopover();
    }

    for (const link of document.querySelectorAll("header a[data-section]")) {
      if (main.dataset.section && link.dataset.section === main.dataset.section) {
        link.setAttribute("aria-current", "page");
      } else {
        link.removeAttribute("aria-current");
      }
    }

    const heading = main.querySelector("h1");
    const announcer = document.getElementById("msdr-announcer");
    if (announcer) {
      announcer.textContent = heading?.textContent ?? document.title;
    }
    main.focus({ preventScroll: true });
    window.scrollTo({ top: 0 });
  });
}
