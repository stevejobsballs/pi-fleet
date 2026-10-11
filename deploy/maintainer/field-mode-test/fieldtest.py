# Field mode test helpers: see Run-Field-Mode-Test.sh.
# Drives field mode end to end: take work, go offline, save work done,
# come back online, check it was sent.
import sys, time
sys.path.insert(0, sys.argv[2])
from mn import M
base = sys.argv[1]
m = M()
m.flag = sys.argv[3]
step = lambda s: print("-", s, flush=True)

# As a phone does after scanning "Open my work on my phone": the sign-in
# page with the username filled in; after signing in, the work is saved
# on the device by itself.
m.cmd("WebDriver:DeleteAllCookies")
m.go(base + "/login?user=tess&next=%2F%3Ftake%3D1")
assert m.js("return document.querySelector('[name=username]').value") == "tess", "username not filled in"
m.js("""document.querySelector('[name=password]').value='brass-kettle-orchard-7';
document.querySelector('form[action="/login"]').requestSubmit();""")
m.wait("return location.pathname === '/' && document.readyState === 'complete'")
step("signed in as tess from the phone link")
note = m.wait("const n = document.getElementById('field-note'); return n && /^Saved [0-9]+ page/.test(n.textContent) && n.textContent", 30)
step("took work by itself: " + note)
assert m.js("return location.search") == "", "take=1 left in the address"
m.wait("return !!navigator.serviceWorker.controller")
wo = m.js("const a = [...document.querySelectorAll('a.job')].find(a => a.textContent.includes('Yearly inspection')); return a && a.getAttribute('href')")
assert wo, "no Yearly inspection job"

m.offline(True)
step("offline")
m.go(base + wo)
txt = m.wait("return document.querySelector('.field-offline') && document.querySelector('.field-offline').textContent")
step("offline copy: " + txt[:70])
m.js("""const t = document.querySelector('textarea[name=work_done]'); t.value = 'Offline: replaced the paddle cable.';
t.form.requestSubmit();""")
m.wait("return location.search.includes('saved=offline') && document.readyState === 'complete'")
bar = m.wait("const b = document.querySelector('.field-bar'); return b && !b.hidden && b.textContent")
kept = m.js("return document.querySelector('textarea[name=work_done]').value")
step("kept on the device: bar=%r, textarea=%r" % (bar, kept))
assert kept == "Offline: replaced the paddle cable.", kept
m.go(base + "/inventory")
nt = m.wait("return document.querySelector('h1') && document.querySelector('h1').textContent")
step("unsaved page offline: " + nt)
assert nt == "Not on this device", nt

m.offline(False)
step("online")
m.go(base + wo)
sent = m.wait("const b = document.querySelector('.field-bar'); return b && /Sent 1 thing/.test(b.textContent) && b.textContent", 30)
step("sent: " + sent)
m.go(base + wo)
now = m.wait("const t = document.querySelector('textarea[name=work_done]'); return t && t.value")
step("on the master now: %r" % now)
assert now == "Offline: replaced the paddle cable.", now

# A checklist (a form with photos) offline, then the sign-in expiring
# before sending.
cl = m.js("const a = [...document.querySelectorAll('a.cl-open')].find(a => a.textContent.includes('Energy calibration')); return a && a.getAttribute('href')")
m.go(base + "/")
m.js("document.getElementById('field-take').click()")
m.wait("const n = document.getElementById('field-note'); return n && /^Saved [0-9]+ page/.test(n.textContent)")
m.offline(True)
m.go(base + cl)
m.wait("return document.querySelector('.field-offline')")
step = step
field = m.js("const i = document.querySelector('[name=v_e200]'); i.value = '205'; return i.name")
m.js("document.querySelector('form.checklist').requestSubmit()")
m.wait("return location.search.includes('saved=offline') && document.readyState === 'complete'")
v = m.js("return document.querySelector('[name=' + arguments[0] + ']').value", field)
print("- checklist answer kept offline:", field, v, flush=True)
assert v == "205"
m.cmd("WebDriver:DeleteAllCookies")
m.offline(False)
m.go(base + cl)
m.wait("return location.pathname === '/login'")
m.js("""document.querySelector('[name=username]').value='tess';
document.querySelector('[name=password]').value='brass-kettle-orchard-7';
document.querySelector('form[action="/login"]').requestSubmit();""")
sent = m.wait("const b = document.querySelector('.field-bar'); return b && /Sent 1 thing/.test(b.textContent) && b.textContent", 30)
print("- after signing in again:", sent, flush=True)
m.go(base + cl)
saved = m.wait("return document.body.innerText.includes('As found 205 J') && 'yes'")
print("- checklist answer on the master:", saved, flush=True)
print("FIELD MODE OK")
