// pi-fleet's field mode, on the page: starts the service worker (sw.js),
// offers "Take my work with me", shows what is kept on this device, puts
// kept answers back into the forms they came from, and sends them once
// the master Pi answers again. Without it, every page works as before.
"use strict";

(function () {
  const main = document.querySelector("main");
  const user = document.body.dataset.user || "";
  const take = document.getElementById("field-take");
  const note = document.getElementById("field-note");
  const say = (el, text) => { if (el) { el.textContent = text; el.hidden = false; } };
  const queueable = /^\/work-orders\/[0-9a-f-]{36}\/(checklist|work-done|labor|test-equipment(\/remove)?)$/;

  if (!("serviceWorker" in navigator) || !main || !user) {
    say(note, "This browser can't keep work for offline use.");
    return;
  }

  // A bar at the top of the page for what field mode has to say.
  const bar = document.createElement("div");
  bar.className = "field-bar";
  bar.setAttribute("role", "status");
  bar.hidden = true;
  main.prepend(bar);
  function show(text, button) {
    bar.replaceChildren();
    const p = document.createElement("p");
    p.textContent = text;
    bar.append(p);
    if (button) bar.append(button);
    bar.hidden = false;
  }

  // Ask the service worker something, and wait for its answer.
  function ask(msg) {
    return navigator.serviceWorker.ready.then((reg) => new Promise((resolve) => {
      const ch = new MessageChannel();
      ch.port1.onmessage = (e) => resolve(e.data || {});
      (navigator.serviceWorker.controller || reg.active).postMessage(msg, [ch.port2]);
    }));
  }

  // Forms that record work carry who filled them in, so what is kept
  // offline is only ever sent under the same person's sign-in.
  document.addEventListener("submit", (e) => {
    const f = e.target;
    if (!(f instanceof HTMLFormElement) || !queueable.test(new URL(f.action, location.href).pathname)) return;
    if (!f.querySelector('input[name="field_user"]')) {
      const i = document.createElement("input");
      i.type = "hidden";
      i.name = "field_user";
      i.value = user;
      f.append(i);
    }
  }, true);

  navigator.serviceWorker.register("/sw.js").then(start, () => {
    say(note, "Offline use needs a certificate this device trusts: a super user installs the hospital's on Admin › Pis.");
  });

  async function start() {
    if (take) {
      take.hidden = false;
      take.addEventListener("click", async () => {
        take.disabled = true;
        say(note, "Saving your work on this device…");
        const r = await ask({ type: "take" });
        take.disabled = false;
        say(note, r.error ? "Not saved: " + r.error + "." :
          `Saved ${r.saved} page${r.saved === 1 ? "" : "s"} at ${r.at}. They open on this device without a network; what you save there is sent when you're back.`);
      });
    }
    // Opened from the "open my work on my phone" code: save the work here.
    if (take && new URLSearchParams(location.search).get("take") === "1") {
      history.replaceState(null, "", location.pathname);
      await navigator.serviceWorker.ready;
      take.click();
    }
    const st = await ask({ type: "status", user, page: location.pathname });
    for (const item of st.forPage || []) refill(item);
    if (new URLSearchParams(location.search).get("saved") === "offline") {
      show("Saved on this device. It's sent when you're back on the network.");
    } else if (st.mine) {
      show(`${st.mine} thing${st.mine === 1 ? "" : "s"} saved on this device, waiting to be sent.`);
    }
    if (st.mine && (await online())) send();
    window.addEventListener("online", () => setTimeout(async () => { if (await online()) send(); }, 1500));
    signOutGuard(st.mine);
  }

  async function online() {
    try {
      return (await fetch("/field/ping", { cache: "no-store" })).ok;
    } catch (e) {
      return false;
    }
  }

  async function send() {
    const csrf = document.querySelector('input[name="csrf"]');
    if (!csrf) return;
    const r = await ask({ type: "send", user, csrf: csrf.value });
    const parts = [];
    if (r.sent) parts.push(`Sent ${r.sent} thing${r.sent === 1 ? "" : "s"} saved on this device.`);
    for (const f of r.failed || []) parts.push(`Not accepted (saved ${new Date(f.at).toLocaleString()}): ${f.said}`);
    if (r.signin) {
      const a = document.createElement("a");
      a.className = "button";
      a.href = "/login?next=" + encodeURIComponent(location.pathname);
      a.textContent = "Sign in to send the rest";
      show(parts.concat("Some are still waiting: sign in again to send them.").join(" "), a);
      return;
    }
    if (parts.length) {
      const again = document.createElement("a");
      again.className = "button secondary";
      again.href = location.pathname;
      again.textContent = "Show the page as it is now";
      show(parts.join(" "), again);
    }
  }

  // Put answers kept for this page back into its form, as typed.
  function refill(item) {
    const form = [...document.forms].find((f) => new URL(f.action, location.href).pathname === item.url);
    if (!form) return;
    const values = new Map();
    for (const [k, v] of item.entries) {
      if (k === "csrf" || k === "field_user") continue;
      if (!values.has(k)) values.set(k, []);
      values.get(k).push(v);
    }
    for (const [name, vs] of values) {
      for (const el of form.querySelectorAll(`[name="${CSS.escape(name)}"]`)) {
        if (el.type === "radio" || el.type === "checkbox") el.checked = vs.includes(el.value);
        else if (el.type !== "file" && el.type !== "hidden") el.value = vs[vs.length - 1];
      }
    }
    form.classList.add("field-kept");
  }

  // Signing out forgets the saved pages; work not yet sent stays, and is
  // sent when the same person signs in here again.
  function signOutGuard(kept) {
    const out = document.querySelector('form[action="/logout"]');
    if (!out) return;
    out.addEventListener("submit", async (e) => {
      if (out.dataset.ready) return;
      e.preventDefault();
      if (kept && !confirm(`${kept} thing${kept === 1 ? "" : "s"} saved on this device ${kept === 1 ? "isn't" : "aren't"} sent yet. They stay here and are sent when you sign in on this device again. Sign out?`)) return;
      await ask({ type: "forget" });
      out.dataset.ready = "yes";
      out.submit();
    });
  }
})();
