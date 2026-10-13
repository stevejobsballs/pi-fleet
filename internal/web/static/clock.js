// pi-fleet's running clocks tick: every element with data-since (when the
// clock started) shows the time since then, h:mm:ss. Without this script
// the page shows h:mm as it was when the page opened.
"use strict";

function tick() {
  const now = Date.now();
  for (const el of document.querySelectorAll("[data-since]")) {
    const s = Math.max(0, Math.floor((now - Date.parse(el.dataset.since)) / 1000));
    const h = Math.floor(s / 3600), m = Math.floor(s / 60) % 60, sec = s % 60;
    el.textContent = h + ":" + String(m).padStart(2, "0") + ":" + String(sec).padStart(2, "0");
  }
}
tick();
setInterval(tick, 1000);
