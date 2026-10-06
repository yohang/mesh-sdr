// Removes single-use tokens (setup, invitation and password reset links)
// from the address bar and the history once the page has read them (SR-07):
// a page that carries one marks an element with data-replace-url, the URL to
// show instead.

function replaceTokenURL() {
  const el = document.querySelector("[data-replace-url]");
  const url = el?.dataset.replaceUrl;
  if (url && url.startsWith("/") && location.pathname !== url) {
    history.replaceState(history.state, "", url);
  }
}

replaceTokenURL();
document.addEventListener("htmx:after:swap", replaceTokenURL);
