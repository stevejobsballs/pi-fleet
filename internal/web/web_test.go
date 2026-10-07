package web

import (
	"context"
	"crypto/ed25519"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"pi-fleet/internal/app"
	"pi-fleet/internal/blobs"
	"pi-fleet/internal/domain"
	"pi-fleet/internal/event"
	"pi-fleet/internal/fleetca"
	"pi-fleet/internal/fleetsync"
	"pi-fleet/internal/hlc"
	"pi-fleet/internal/password"
	"pi-fleet/internal/store"
)

type env struct {
	t     *testing.T
	ctx   context.Context
	now   time.Time
	app   *app.App
	srv   *httptest.Server
	super app.Actor
	site  string
	loc   string
	key   ed25519.PrivateKey // central's event key
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, ctx: context.Background(), now: time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)}
	clock := func() time.Time { return e.now }
	pub, priv, _ := ed25519.GenerateKey(nil)
	e.key = priv
	nodeID := uuid.Must(uuid.NewV7()).String()
	st, err := store.Open(filepath.Join(t.TempDir(), "c.db"), store.WithApplier(&domain.Projector{LocalNodeID: nodeID}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.TrustKey(e.ctx, nodeID, pub)
	e.app = &app.App{Store: st, Params: password.Params{Time: 1, MemoryKiB: 64, Threads: 1}, Now: clock,
		Blobs: &blobs.Store{Dir: filepath.Join(t.TempDir(), "blobs")},
		Author: &store.Author{NodeID: nodeID, ChainID: uuid.Must(uuid.NewV7()).String(),
			Signer: event.Signer{KeyID: event.KeyID(pub), Key: priv}, Clock: hlc.New(clock, 0), Now: clock}}
	id, err := e.app.BootstrapSuperUser(e.ctx, app.NewUser{Username: "admin", LegalName: "Ada Admin", Email: "ada@example.org", IdentityVerification: "console"}, "tumbleweed-gasket-42")
	if err != nil {
		t.Fatal(err)
	}
	e.super = app.Actor{UserID: id, SessionID: "t"}
	e.site, _ = e.app.CreateSite(e.ctx, e.super, "NYC", "New York", "America/New_York")
	e.loc, _ = e.app.CreateLocation(e.ctx, e.super, e.site, "", "Biomed shop", "room")

	s := &Server{App: e.app, Role: "central", Now: clock, PHIPatterns: DefaultPHIPatterns, Secure: true}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	// Like the master Pi: the sync API and the web interface on one address.
	ca, err := fleetca.LoadOrCreate(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/", (&fleetsync.Server{App: e.app, CentralKey: e.key, Now: clock, Logf: t.Logf,
		Fleet: (&FleetAPI{App: e.app}).Handler(), Blobs: e.app.Blobs, CA: ca}).Handler())
	mux.Handle("/", h)
	e.srv = httptest.NewTLSServer(mux)
	t.Cleanup(e.srv.Close)
	return e
}

// user creates an active user with password "brass-kettle-orchard-7".
func (e *env) user(username, legalName, role string) string {
	e.t.Helper()
	id, temp, err := e.app.CreateUser(e.ctx, e.super, app.NewUser{Username: username, LegalName: legalName, Email: username + "@example.org", Role: role, HomeSites: []string{e.site}, IdentityVerification: "badge"})
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.app.ChangePassword(e.ctx, app.Actor{UserID: id, SessionID: "t"}, temp, "brass-kettle-orchard-7"); err != nil {
		e.t.Fatal(err)
	}
	return id
}

// browser is one signed-in person using Firefox (see browser_test.go).
type browser struct {
	e    *env
	c    *http.Client
	csrf string
}

func (e *env) browser() *browser {
	jar, _ := cookiejar.New(nil)
	ff := &firefox{t: e.t, base: e.srv.Client().Transport}
	return &browser{e: e, c: &http.Client{Jar: jar, Transport: ff, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

var csrfRE = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (b *browser) get(path string) (int, string, string) {
	b.e.t.Helper()
	resp, err := b.c.Get(b.e.srv.URL + path)
	if err != nil {
		b.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if m := csrfRE.FindSubmatch(body); m != nil {
		b.csrf = string(m[1])
	}
	return resp.StatusCode, resp.Header.Get("Location"), string(body)
}

// post submits a form with the session's CSRF token, following one
// redirect to read the flash message.
func (b *browser) post(path string, form url.Values) (int, string) {
	b.e.t.Helper()
	if form == nil {
		form = url.Values{}
	}
	if form.Get("csrf") == "" {
		form.Set("csrf", b.csrf)
	}
	resp, err := b.c.PostForm(b.e.srv.URL+path, form)
	if err != nil {
		b.e.t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther {
		_, _, page := b.get(resp.Header.Get("Location"))
		return resp.StatusCode, page
	}
	return resp.StatusCode, string(body)
}

func (b *browser) login(username, pw string) {
	b.e.t.Helper()
	b.get("/login")
	code, page := b.post("/login", url.Values{"username": {username}, "password": {pw}})
	if code != http.StatusSeeOther {
		b.e.t.Fatalf("login %s: %d\n%s", username, code, page)
	}
	b.get("/")
}

func TestSecurityHeadersAndLoginRequired(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	code, loc, _ := b.get("/work-orders")
	if code != http.StatusSeeOther || loc != "/login" {
		t.Fatalf("unauthenticated: %d %s", code, loc)
	}
	resp, err := e.srv.Client().Get(e.srv.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "script-src") {
		t.Errorf("CSP = %q", csp)
	}
	for h, want := range map[string]string{"X-Frame-Options": "DENY", "X-Content-Type-Options": "nosniff", "Cache-Control": "no-store"} {
		if resp.Header.Get(h) != want {
			t.Errorf("%s = %q", h, resp.Header.Get(h))
		}
	}
}

func TestLoginCSRFAndOrigin(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	code, page := b.post("/login", url.Values{"username": {"admin"}, "password": {"wrong"}})
	if code != http.StatusUnauthorized || !strings.Contains(page, "wrong username or password") {
		t.Fatalf("bad login: %d", code)
	}
	b.login("admin", "tumbleweed-gasket-42")
	for _, c := range b.c.Jar.Cookies(mustURL(e.srv.URL)) {
		if c.Name == cookieName && c.Value == "" {
			t.Fatal("empty session cookie")
		}
	}
	// Forged form without the token.
	code, _ = b.post("/admin/sites", url.Values{"csrf": {"forged"}, "code": {"EVL"}, "name": {"x"}, "timezone": {"UTC"}})
	if code != http.StatusForbidden {
		t.Fatalf("missing CSRF token: %d", code)
	}
	// Cross-origin POST, even with the token.
	req, _ := http.NewRequest("POST", e.srv.URL+"/admin/sites", strings.NewReader(url.Values{"csrf": {b.csrf}, "code": {"EVL"}, "name": {"x"}, "timezone": {"UTC"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Referer", "https://evil.example/")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := b.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin POST: %d", resp.StatusCode)
	}
	if _, err := domain.GetSiteByCode(e.ctx, e.app.Store.DB(), "EVL"); err == nil {
		t.Fatal("forged request created a site")
	}
}

func TestSameOrigin(t *testing.T) {
	cases := []struct {
		name string
		hdr  map[string]string
		want bool
	}{
		{"firefox form post", map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "null"}, true},
		{"typed in address bar", map[string]string{"Sec-Fetch-Site": "none"}, true},
		{"other site", map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}, false},
		{"sibling host", map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "https://other.local:8443"}, false},
		{"old browser, own origin", map[string]string{"Origin": "https://fleet-master.local:8443"}, true},
		{"old browser, other origin", map[string]string{"Origin": "https://evil.example"}, false},
		{"old browser, own referer", map[string]string{"Origin": "null", "Referer": "https://fleet-master.local:8443/login"}, true},
		{"null origin, nothing else", map[string]string{"Origin": "null"}, false},
		{"no headers (tools)", nil, true},
	}
	for _, c := range cases {
		r := httptest.NewRequest("POST", "https://fleet-master.local:8443/login", nil)
		for k, v := range c.hdr {
			r.Header.Set(k, v)
		}
		if got := sameOrigin(r); got != c.want {
			t.Errorf("%s: sameOrigin = %v, want %v", c.name, got, c.want)
		}
	}
}

func mustURL(s string) *url.URL { u, _ := url.Parse(s); return u }

func TestNewUserMustChangePassword(t *testing.T) {
	e := newEnv(t)
	admin := e.browser()
	admin.login("admin", "tumbleweed-gasket-42")
	_, page := admin.post("/admin/users", url.Values{"username": {"tess"}, "legal_name": {"Tess Tech"}, "email": {"tess@example.org"},
		"role": {"user"}, "home_sites": {e.site}, "verified": {"in person, badge 4411"}})
	otp := regexp.MustCompile(`class="otp">([0-9A-Z-]+)<`).FindStringSubmatch(page)
	if otp == nil {
		t.Fatalf("no one-time password shown:\n%s", page)
	}

	tess := e.browser()
	code, _ := tess.post("/login", url.Values{"username": {"tess"}, "password": {otp[1]}})
	if code != http.StatusSeeOther {
		t.Fatalf("one-time login: %d", code)
	}
	if code, loc, _ := tess.get("/work-orders"); code != http.StatusSeeOther || loc != "/password" {
		t.Fatalf("before changing password: %d %s", code, loc)
	}
	tess.get("/password")
	_, page = tess.post("/password", url.Values{"current": {otp[1]}, "new": {"short"}, "confirm": {"short"}})
	if !strings.Contains(page, "at least 12 characters") {
		t.Fatalf("weak password accepted:\n%s", page)
	}
	_, page = tess.post("/password", url.Values{"current": {otp[1]}, "new": {"copper-ladder-sunrise"}, "confirm": {"copper-ladder-sunrise"}})
	if !strings.Contains(page, "Password changed") {
		t.Fatalf("change password:\n%s", page)
	}
	if code, _, _ := tess.get("/work-orders"); code != http.StatusOK {
		t.Fatalf("after changing password: %d", code)
	}
	// An ordinary user can't reach admin pages.
	if code, _, _ := tess.get("/admin/users"); code != http.StatusForbidden {
		t.Fatalf("user on admin page: %d", code)
	}
	if code, _ := tess.post("/admin/users", url.Values{"username": {"mallory"}}); code != http.StatusForbidden {
		t.Fatalf("user posting to admin: %d", code)
	}
}

func TestSessionIdleTimeout(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	b.login("admin", "tumbleweed-gasket-42")
	e.now = e.now.Add(14 * time.Minute)
	if code, _, _ := b.get("/"); code != http.StatusOK {
		t.Fatalf("within idle window: %d", code)
	}
	e.now = e.now.Add(16 * time.Minute)
	if code, loc, _ := b.get("/"); code != http.StatusSeeOther || loc != "/login" {
		t.Fatalf("after idle timeout: %d %s", code, loc)
	}
}

func TestOutputIsEscaped(t *testing.T) {
	e := newEnv(t)
	b := e.browser()
	b.login("admin", "tumbleweed-gasket-42")
	b.get("/assets/new")
	_, page := b.post("/assets", url.Values{"tag": {`<script>alert(1)</script>`}, "location": {e.loc}, "manufacturer": {`"><img src=x onerror=alert(1)>`}, "model": {"M"}})
	if strings.Contains(page, "<script>alert") || strings.Contains(page, "<img src=x") {
		t.Fatal("unescaped markup in page")
	}
	if !strings.Contains(page, "&lt;script&gt;") {
		t.Fatalf("expected escaped tag in page:\n%s", page)
	}
}

func TestPHIWarning(t *testing.T) {
	e := newEnv(t)
	asset, _ := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "A1", LocationID: e.loc, Manufacturer: "Fluke", Model: "M"})
	b := e.browser()
	b.login("admin", "tumbleweed-gasket-42")
	b.get("/work-orders/new")
	form := url.Values{"asset": {asset}, "type": {"corrective"}, "priority": {"high"}, "title": {"Pump alarm"},
		"problem": {"Alarmed on patient in room 4 bed 2, MRN 12345678"}}
	_, page := b.post("/work-orders", form)
	if !strings.Contains(page, "may contain patient information") {
		t.Fatalf("PHI not flagged:\n%s", page)
	}
	var n int
	e.app.Store.DB().QueryRow(`SELECT count(*) FROM work_orders`).Scan(&n)
	if n != 0 {
		t.Fatal("work order with likely PHI saved without confirmation")
	}
	form.Set("problem", "Occlusion alarm during use, no fault found on bench")
	if _, page = b.post("/work-orders", form); !strings.Contains(page, "Opened NYC-WO-") {
		t.Fatalf("clean work order:\n%s", page)
	}
}

// TestCalibrationWorkOrderThroughTheUI runs a calibration work order from
// opening to closure with Part 11 signatures, entirely through forms.
func TestCalibrationWorkOrderThroughTheUI(t *testing.T) {
	e := newEnv(t)
	e.user("mona", "Mona Mid", domain.RoleMidTier)
	tessID := e.user("tess", "Tess Tech", domain.RoleUser)
	asset, _ := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "INST-1", LocationID: e.loc, Manufacturer: "Fluke", Model: "ESA615"})
	std, _ := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "STD-1", LocationID: e.loc, Manufacturer: "Fluke", Model: "5522A", IsReferenceStandard: true})

	mona, tess := e.browser(), e.browser()
	mona.login("mona", "brass-kettle-orchard-7")
	tess.login("tess", "brass-kettle-orchard-7")

	mona.get("/work-orders/new")
	_, page := mona.post("/work-orders", url.Values{"asset": {asset}, "type": {"calibration"}, "priority": {"normal"}, "title": {"Annual calibration"}})
	woID := regexp.MustCompile(`/work-orders/([0-9a-f-]{36})/assign`).FindStringSubmatch(page)
	if woID == nil {
		t.Fatalf("no work order page:\n%s", page)
	}
	wo := "/work-orders/" + woID[1]
	mona.post(wo+"/assign", url.Values{"assignee": {tessID}})

	tess.get(wo)
	tess.post(wo+"/status", url.Values{"to": {"in_progress"}})
	_, page = tess.post(wo+"/calibration", url.Values{
		"procedure": {"ESA615 check v3"}, "temperature": {"21.5"}, "standards": {std},
		"parameter": {"Mains voltage", "Earth leakage", ""}, "unit": {"V", "µA", ""}, "nominal": {"120.0", "0", ""},
		"tol_kind": {"pct", "limits", "abs"}, "tol_value": {"2", "", ""}, "tol_lower": {"", "0", ""}, "tol_upper": {"", "300", ""},
		"as_found": {"122.4", "150", ""}, "as_left": {"", "", ""},
	})
	if !strings.Contains(page, "Calibration recorded") || !strings.Contains(page, "117.6 – 122.4") {
		t.Fatalf("calibration:\n%s", page)
	}

	// Sign as performed: wrong password, then the clock acknowledgement.
	_, page = tess.post(wo+"/sign", url.Values{"meaning": {"performed"}, "password": {"nope"}})
	if !strings.Contains(page, "wrong username or password") {
		t.Fatalf("wrong signing password:\n%s", page)
	}
	_, page = tess.post(wo+"/sign", url.Values{"meaning": {"performed"}, "password": {"brass-kettle-orchard-7"}})
	if !strings.Contains(page, "Not signed: this Pi&#39;s clock") {
		t.Fatalf("clock acknowledgement not demanded:\n%s", page)
	}
	_, page = tess.post(wo+"/sign", url.Values{"meaning": {"performed"}, "password": {"brass-kettle-orchard-7"}, "ack_clock": {"yes"}})
	if !strings.Contains(page, "Signed.") || !strings.Contains(page, "<strong>completed</strong>") {
		t.Fatalf("performed signature:\n%s", page)
	}

	// Tess can't review her own work; Mona reviews and approves.
	_, _, page = tess.get(wo)
	if strings.Contains(page, "Sign as reviewed") {
		t.Fatal("performer offered the review signature")
	}
	mona.get(wo)
	mona.post(wo+"/sign", url.Values{"meaning": {"reviewed"}, "password": {"brass-kettle-orchard-7"}, "ack_clock": {"yes"}})
	_, page = mona.post(wo+"/sign", url.Values{"meaning": {"approved"}, "password": {"brass-kettle-orchard-7"}, "ack_clock": {"yes"}})
	if !strings.Contains(page, "<strong>closed</strong>") {
		t.Fatalf("not closed:\n%s", page)
	}
	// Signature manifestation: name, meaning, time, statement (§11.50).
	for _, want := range []string{"<strong>Tess Tech</strong> (tess) · <em>performed</em> · 2026-10-06 09:00 UTC (2026-10-06 05:00 EDT)",
		"<strong>Mona Mid</strong> (mona) · <em>reviewed</em>", "<strong>Mona Mid</strong> (mona) · <em>approved</em>",
		"I performed this work as recorded.", "signed with an unverified clock"} {
		if !strings.Contains(page, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestEveryPageRenders(t *testing.T) {
	e := newEnv(t)
	asset, _ := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "A1", LocationID: e.loc, Manufacturer: "Fluke", Model: "M"})
	wo, _, _ := e.app.OpenWorkOrder(e.ctx, e.super, app.NewWorkOrder{Type: "calibration", AssetID: asset, Priority: "low", Title: "x"})
	b := e.browser()
	b.login("admin", "tumbleweed-gasket-42")
	for _, p := range []string{"/", "/assets", "/assets?q=Fluke", "/assets/new", "/assets/" + asset, "/work-orders", "/work-orders?view=all",
		"/work-orders/new", "/work-orders/" + wo, "/inventory", "/schedules", "/review", "/admin/users", "/admin/sites", "/admin/nodes", "/admin/kiosks", "/procedures", "/password",
		"/static/style.css"} {
		if code, _, body := b.get(p); code != http.StatusOK || strings.Contains(body, "something went wrong") {
			t.Errorf("GET %s: %d", p, code)
		}
	}
	if code, _, _ := b.get("/work-orders/" + uuid.NewString()); code != http.StatusNotFound {
		t.Errorf("missing work order: %d", code)
	}
}

func TestKioskAdminPage(t *testing.T) {
	e := newEnv(t)
	tessID := e.user("tess", "Tess Tech", domain.RoleUser)
	b := e.browser()
	b.login("admin", "tumbleweed-gasket-42")
	b.get("/admin/kiosks")
	_, page := b.post("/admin/kiosks", url.Values{"site": {e.site}, "name": {"nyc-shop"}})
	if !regexp.MustCompile(`class="otp">[0-9A-Z]{4}(-[0-9A-Z]{4}){3}<`).MatchString(page) || !strings.Contains(page, "pi-fleet activate -kiosk nyc-shop") {
		t.Fatalf("kiosk creation:\n%s", page)
	}
	_, _, page = b.get("/admin/kiosks")
	id := regexp.MustCompile(`/admin/kiosks/([0-9a-f-]{36})/members`).FindStringSubmatch(page)
	if id == nil {
		t.Fatalf("kiosk not listed:\n%s", page)
	}
	if _, page = b.post("/admin/kiosks/"+id[1]+"/members", url.Values{"user": {tessID}}); !strings.Contains(page, "Tess Tech (tess)") {
		t.Fatalf("member not shown:\n%s", page)
	}
	if _, page = b.post("/admin/kiosks/"+id[1]+"/members/remove", url.Values{"user": {tessID}}); !strings.Contains(page, "No members yet") {
		t.Fatalf("member not removed:\n%s", page)
	}
	if _, page = b.post("/admin/kiosks", url.Values{"site": {e.site}, "name": {"Bad Name!"}}); !strings.Contains(page, "Not saved") {
		t.Fatalf("bad kiosk name accepted:\n%s", page)
	}
}
