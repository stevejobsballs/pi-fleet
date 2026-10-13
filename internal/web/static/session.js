// pi-fleet's signed-in pages: never shown once signed out.
//
// - Back and Forward: a browser may show a page again from its memory,
//   without asking the Pi, even after signing out. Such a page is hidden
//   and asked for again: signed out, the Pi answers with the sign-in page.
// - Left open: once the sign-in has ended (15 minutes without use), the
//   page goes to the sign-in page instead of staying on screen. Asking
//   doesn't count as use, and offline (field mode) nothing changes.
"use strict";

addEventListener("pagehide", (e) => {
  if (e.persisted) document.documentElement.hidden = true;
});
addEventListener("pageshow", (e) => {
  if (e.persisted) {
    document.documentElement.hidden = true;
    location.reload();
  }
});

async function stillSignedIn() {
  let r;
  try {
    r = await fetch("/session", { cache: "no-store", credentials: "same-origin", redirect: "manual" });
  } catch (err) {
    return; // offline: carry on
  }
  if (r.status === 401) {
    document.documentElement.hidden = true;
    location.replace("/login?next=" + encodeURIComponent(location.pathname + location.search));
  }
}
setInterval(stillSignedIn, 60 * 1000);
document.addEventListener("visibilitychange", () => {
  if (document.visibilityState === "visible") stillSignedIn();
});
