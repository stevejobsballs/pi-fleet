package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Signing out: the browser is told to forget the pages it showed, and
// the sign-in can't be used again.
func TestSignOutForgetsPages(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	b.login("admin", "tumbleweed-gasket-42")
	resp, err := b.c.PostForm(e.srv.URL+"/logout", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Clear-Site-Data"); got != `"cache"` {
		t.Fatalf("Clear-Site-Data: %q", got)
	}
	if code, loc, _ := b.get("/assets"); code != http.StatusSeeOther || !strings.HasPrefix(loc, "/login") {
		t.Fatalf("after signing out: %d %s", code, loc)
	}
	// Signed-in pages carry the script that hides pages Back and Forward
	// bring back, and goes to the sign-in page once signed out.
	b.login("admin", "tumbleweed-gasket-42")
	for _, p := range []string{"/", "/assets"} {
		if _, _, page := b.get(p); !strings.Contains(page, `<script src="/static/session.js"></script>`) {
			t.Fatalf("%s without session.js", p)
		}
	}
	if code, _, js := b.get("/static/session.js"); code != http.StatusOK || !strings.Contains(js, "e.persisted") {
		t.Fatalf("session.js: %d", code)
	}
}

// A page left open asks whether it is still signed in, without that
// counting as use: it is signed out on time.
func TestSessionCheckDoesNotKeepTheSignInAlive(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	if code, _, _ := b.get("/session"); code != http.StatusUnauthorized {
		t.Fatalf("not signed in: %d", code)
	}
	b.login("admin", "tumbleweed-gasket-42")
	for range 3 {
		e.now = e.now.Add(6 * time.Minute)
		code, _, _ := b.get("/session")
		if e.now.Sub(time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)) <= IdleTimeout && code != http.StatusNoContent {
			t.Fatalf("signed in: %d", code)
		}
	}
	if code, _, _ := b.get("/session"); code != http.StatusUnauthorized {
		t.Fatalf("checking kept the sign-in alive: %d", code)
	}
}

// Changing a password ends every other sign-in with the old one; a reset
// by a super user ends them all.
func TestPasswordChangeEndsOtherSignIns(t *testing.T) {
	e := newEnv(t)
	tess := e.user("tess", "Tess Tech", "user")
	desk, phone := e.browser(), e.browser()
	desk.login("tess", "brass-kettle-orchard-7")
	phone.login("tess", "brass-kettle-orchard-7")
	desk.get("/password")
	_, page := desk.post("/password", url.Values{"current": {"brass-kettle-orchard-7"}, "new": {"copper-ladder-sunrise"}, "confirm": {"copper-ladder-sunrise"}})
	if !strings.Contains(page, "Password changed") {
		t.Fatalf("change password:\n%s", page)
	}
	if code, _, _ := desk.get("/assets"); code != http.StatusOK {
		t.Fatalf("the sign-in that changed it: %d", code)
	}
	if code, loc, _ := phone.get("/assets"); code != http.StatusSeeOther || !strings.HasPrefix(loc, "/login") {
		t.Fatalf("other sign-in carried on: %d %s", code, loc)
	}

	admin := e.browser()
	admin.login("admin", "tumbleweed-gasket-42")
	admin.get("/admin/users")
	if _, page := admin.post("/admin/users/"+tess+"/reset", url.Values{"verified": {"in person, badge 4411"}}); !strings.Contains(page, "One-time password") {
		t.Fatalf("reset:\n%s", page)
	}
	if code, _, _ := desk.get("/assets"); code != http.StatusSeeOther {
		t.Fatalf("signed in after a reset: %d", code)
	}
}

// After signing in, only pages on this site: not a look-alike site
// through a link with a tab, line break or backslash.
func TestNextPageStaysOnThisSite(t *testing.T) {
	for in, want := range map[string]string{
		"/assets?q=1":          "/assets?q=1",
		"/work-orders/abc#x":   "/work-orders/abc#x",
		"//evil.example":       "/",
		"/\t/evil.example":     "/",
		"/\n/evil.example":     "/",
		"/\\evil.example":      "/",
		"/x\\..\\evil":         "/",
		"https://evil.example": "/",
		"evil.example":         "/",
		"":                     "/",
	} {
		if got := localBack(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	e := newEnv(t)
	b := e.browser()
	b.get("/login")
	resp, err := b.c.PostForm(e.srv.URL+"/login", url.Values{"username": {"admin"}, "password": {"tumbleweed-gasket-42"}, "next": {"/\t/evil.example"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "/" {
		t.Fatalf("signed in, then sent to %q", loc)
	}
}
