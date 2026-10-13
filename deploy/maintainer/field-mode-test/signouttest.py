# Signing out in a real browser: Back and Forward don't show signed-in
# pages again, and a page left open goes to the sign-in page once its
# sign-in has ended. See Run-Sign-Out-Test.sh.
import sys, time
sys.path.insert(0, sys.argv[2])
from mn import M
base = sys.argv[1]
m = M()
step = lambda s: print("-", s, flush=True)

def at():
    time.sleep(1.5)
    return m.url().replace(base, ""), m.js("return !!document.querySelector('form[action=\"/logout\"]')")

def sign_in():
    m.go(base + "/login")
    m.js("""document.querySelector('[name=username]').value='tess';document.querySelector('[name=password]').value='brass-kettle-orchard-7';
document.querySelector('form[action="/login"]').requestSubmit();""")
    m.wait("return location.pathname === '/' && document.readyState === 'complete'")

m.cmd("WebDriver:DeleteAllCookies")
sign_in()
m.go(base + "/assets")
assert at()[1], "not signed in"
m.js("document.querySelector('form[action=\"/logout\"]').requestSubmit()")
m.wait("return location.pathname === '/login' && document.readyState === 'complete'")
step("signed out")
for move in ["Back", "Back", "Forward", "Forward"]:
    m.cmd("WebDriver:" + move)
    where, signed_in = at()
    step(f"{move}: {where}")
    assert not signed_in, f"{move} showed a signed-in page after signing out: {where}"

sign_in()
m.go(base + "/assets")
m.cmd("WebDriver:DeleteAllCookies")  # as when the sign-in ends
step("sign-in ended with the page open; waiting for it to notice")
m.wait("return location.pathname === '/login'", 75)
step("left open: " + at()[0])
print("SIGN OUT OK")
