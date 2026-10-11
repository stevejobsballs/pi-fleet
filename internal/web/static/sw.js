// pi-fleet's field mode: a service worker that keeps the pages of a
// person's work on their phone or tablet, shows them without a network,
// and keeps what they save offline until it can be sent.
//
// Online, nothing changes: every request goes to the master Pi as usual.
// Offline:
//   - pages saved with "Take my work with me" open from the saved copy;
//   - forms that record work (checklist answers, work done, time, test
//     equipment) are kept on the device, and the page they came from
//     opens again with what was typed;
//   - everything else (signing, starting or completing work) says it
//     needs a connection.
// The page (field.js) asks for what's kept to be sent once the master Pi
// answers again, under the same person's sign-in.
"use strict";

const PAGES = "pifleet-pages-v1";   // saved pages of someone's work
const STATIC = "pifleet-static-v1"; // stylesheets, fonts, field.js
const DB = "pifleet-field";
const QUEUE = "queue";

// What may be kept offline: forms that add to the work order's record.
const QUEUEABLE = [
  /^\/work-orders\/[0-9a-f-]{36}\/checklist$/,
  /^\/work-orders\/[0-9a-f-]{36}\/work-done$/,
  /^\/work-orders\/[0-9a-f-]{36}\/labor$/,
  /^\/work-orders\/[0-9a-f-]{36}\/test-equipment(\/remove)?$/,
];

self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (e) => e.waitUntil(self.clients.claim()));

// --- the queue, in IndexedDB ---

function db() {
  return new Promise((resolve, reject) => {
    const r = indexedDB.open(DB, 1);
    r.onupgradeneeded = () => r.result.createObjectStore(QUEUE, { keyPath: "id", autoIncrement: true });
    r.onsuccess = () => resolve(r.result);
    r.onerror = () => reject(r.error);
  });
}

async function tx(mode, f) {
  const d = await db();
  return new Promise((resolve, reject) => {
    const t = d.transaction(QUEUE, mode);
    const out = f(t.objectStore(QUEUE));
    t.oncomplete = () => resolve(out.result !== undefined ? out.result : out);
    t.onerror = () => reject(t.error);
  });
}

const allQueued = () => tx("readonly", (s) => s.getAll());
const addQueued = (item) => tx("readwrite", (s) => s.add(item));
const dropQueued = (id) => tx("readwrite", (s) => s.delete(id));

// --- requests ---

self.addEventListener("fetch", (e) => {
  const req = e.request;
  const url = new URL(req.url);
  if (url.origin !== location.origin) return;
  if (req.method === "POST") {
    if (req.mode === "navigate") e.respondWith(post(req, url));
    return;
  }
  if (req.method !== "GET") return;
  if (url.pathname.startsWith("/static/")) {
    e.respondWith(staticFile(req));
  } else if (req.mode === "navigate") {
    e.respondWith(page(req, url));
  }
});

// Stylesheets and fonts: from the device, refreshed in the background.
async function staticFile(req) {
  const cache = await caches.open(STATIC);
  const saved = await cache.match(req);
  const fresh = fetch(req).then((r) => {
    if (r.ok) cache.put(req, r.clone());
    return r;
  });
  if (saved) {
    fresh.catch(() => {});
    return saved;
  }
  return fresh;
}

// Pages: from the master Pi; a saved copy is kept up to date, and shown
// when the master can't be reached.
async function page(req, url) {
  const cache = await caches.open(PAGES);
  try {
    const r = await fetch(req);
    if (r.ok && r.headers.get("Content-Type")?.startsWith("text/html") && (await cache.match(url.pathname))) {
      cache.put(url.pathname, r.clone());
    }
    return r;
  } catch (err) {
    const saved = await cache.match(url.pathname);
    if (saved) return offlineCopy(saved);
    return notice("Not on this device",
      "You're offline, and this page wasn't saved on this device. Pages of your own work are saved when you press “Take my work with me” on My work.");
  }
}

// A saved page, marked as such.
async function offlineCopy(saved) {
  const at = saved.headers.get("X-Field-Saved-At") || "";
  const html = (await saved.text()).replace("<main>",
    `<main><p class="flash warn field-offline" role="status">You're offline. This is the copy saved on this device${at ? " at " + at : ""}. What you save is kept here and sent when you're back on the network.</p>`);
  const headers = new Headers(saved.headers); // keeps its security policy
  headers.delete("X-Field-Saved-At");
  return new Response(html, { headers });
}

// Forms: sent as usual; offline, a form that records work is kept on the
// device, and the page it came from opens again.
async function post(req, url) {
  const copy = req.clone();
  try {
    return await fetch(req);
  } catch (err) {
    if (!QUEUEABLE.some((re) => re.test(url.pathname))) {
      return notice("This needs a connection",
        "You're offline. Starting, completing and signing work need the master Pi; try again when you're back on the network.");
    }
    const fd = await copy.formData();
    const entries = [];
    for (const [k, v] of fd.entries()) entries.push([k, v]); // files are kept as they are
    const back = new URL(req.referrer || "/", location.origin);
    await addQueued({
      url: url.pathname, entries, page: back.pathname,
      user: (fd.get("field_user") || "").toString(), at: new Date().toISOString(),
    });
    return Response.redirect(back.pathname + "?saved=offline", 303);
  }
}

function notice(title, text) {
  const html = `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">` +
    `<title>${title} · pi-fleet</title><link rel="stylesheet" href="/static/style.css"><link rel="stylesheet" href="/static/large.css"></head>` +
    `<body><main><h1>${title}</h1><p class="flash warn">${text}</p><p><a class="button" href="/">My work</a></p></main></body></html>`;
  return new Response(html, { status: 503, headers: { "Content-Type": "text/html; charset=utf-8" } });
}

// --- messages from the page (field.js) ---

self.addEventListener("message", (e) => {
  const m = e.data || {};
  const reply = (v) => e.ports[0] && e.ports[0].postMessage(v);
  const run = { take, send, status, forget }[m.type];
  if (run) e.waitUntil(run(m).then(reply, (err) => reply({ error: String(err) })));
});

// take saves the pages of someone's work, as the master Pi lists them.
async function take() {
  const r = await fetch("/field/manifest", { cache: "no-store" });
  if (!r.ok) throw new Error("the list of your work couldn't be fetched");
  const m = await r.json();
  const pages = await caches.open(PAGES);
  for (const k of await pages.keys()) await pages.delete(k); // only the current work
  const staticCache = await caches.open(STATIC);
  let saved = 0;
  const at = new Date().toLocaleString([], { weekday: "short", hour: "2-digit", minute: "2-digit" });
  for (const path of m.pages) {
    const p = await fetch(path, { cache: "no-store" });
    if (!p.ok) continue;
    const headers = new Headers(p.headers);
    headers.set("X-Field-Saved-At", at);
    await pages.put(path, new Response(await p.blob(), { headers }));
    saved++;
  }
  for (const path of m.static) {
    const s = await fetch(path);
    if (s.ok) await staticCache.put(path, s);
  }
  return { saved, at };
}

// status says what is kept for whom, and the answers kept for a page.
async function status(m) {
  const items = await allQueued();
  const mine = items.filter((i) => i.user === m.user);
  const forPage = mine.filter((i) => i.page === m.page).map((i) => ({ url: i.url, entries: i.entries.filter(([, v]) => typeof v === "string") }));
  return { mine: mine.length, others: items.length - mine.length, forPage };
}

// send sends what this person kept, in order, with the page's current
// form token. It stops if they need to sign in again.
async function send(m) {
  const results = { sent: 0, failed: [], signin: false };
  for (const item of await allQueued()) {
    if (item.user !== m.user) continue;
    const fd = new FormData();
    for (const [k, v] of item.entries) fd.append(k, v);
    fd.set("csrf", m.csrf);
    let r;
    try {
      r = await fetch(item.url, { method: "POST", body: fd, credentials: "same-origin", redirect: "follow" });
    } catch (err) {
      break; // offline again: keep the rest
    }
    if (new URL(r.url).pathname === "/login" || r.status === 403) {
      results.signin = true;
      break;
    }
    const text = await r.text();
    const flash = /<p class="flash[^"]*" role="status">([^<]*)<\/p>/.exec(text);
    const said = flash ? flash[1].replace(/&#39;/g, "'").replace(/&quot;|&#34;/g, '"').replace(/&amp;/g, "&") : "";
    if (!r.ok || said.includes("Not saved") || said.includes("Not added")) {
      results.failed.push({ at: item.at, page: item.page, said: said || "the master Pi refused it (" + r.status + ")" });
    } else {
      results.sent++;
    }
    await dropQueued(item.id);
  }
  return results;
}

// forget removes the saved pages (on signing out); kept work stays until
// it is sent by the person who saved it.
async function forget() {
  await caches.delete(PAGES);
  return { forgotten: true };
}
