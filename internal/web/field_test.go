package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"pi-fleet/internal/app"
	"pi-fleet/internal/domain"
)

// Field mode's parts on the master Pi: the service worker at the top of
// the site, the reachability check, the list of someone's work to keep,
// and the script on signed-in pages only.
func TestFieldMode(t *testing.T) {
	e := newEnv(t)
	tessID := e.user("tess", "Tess Tech", domain.RoleUser)
	pump, _ := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "PUMP-1", LocationID: e.loc, Manufacturer: "Baxter", Model: "Sigma"})
	proc, _, err := e.app.PublishProcedure(e.ctx, e.super, "Pump PM", []domain.Step{{ID: "s1", Text: "Housing", Kind: domain.StepCheck}})
	if err != nil {
		t.Fatal(err)
	}
	mine, _, _ := e.app.OpenWorkOrder(e.ctx, e.super, app.NewWorkOrder{Type: "pm", AssetID: pump, Priority: "normal", Title: "PM", ProcedureID: proc})
	e.app.AssignWorkOrder(e.ctx, e.super, mine, tessID)
	other, _, _ := e.app.OpenWorkOrder(e.ctx, e.super, app.NewWorkOrder{Type: "corrective", AssetID: pump, Priority: "normal", Title: "Someone else's"})

	resp, err := e.srv.Client().Get(e.srv.URL + "/sw.js")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/javascript") || resp.Header.Get("Cache-Control") != "no-cache" ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "connect-src 'self'") || !strings.Contains(string(body), "QUEUEABLE") {
		t.Fatalf("sw.js: %d %v", resp.StatusCode, resp.Header)
	}
	if resp, _ := e.srv.Client().Get(e.srv.URL + "/field/ping"); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("ping: %d", resp.StatusCode)
	}

	tess := e.browser()
	if code, loc, _ := tess.get("/field/manifest"); code != http.StatusSeeOther || !strings.HasPrefix(loc, "/login") {
		t.Fatalf("manifest without signing in: %d %s", code, loc)
	}
	// The sign-in page has no script; signed-in pages have field mode's.
	if _, _, page := tess.get("/login"); strings.Contains(page, "<script") {
		t.Fatal("script on the sign-in page")
	}
	tess.login("tess", "brass-kettle-orchard-7")
	_, _, page := tess.get("/")
	if !strings.Contains(page, `<script src="/static/field.js" defer></script>`) || !strings.Contains(page, `data-user="`+tessID+`"`) ||
		!strings.Contains(page, `id="field-take" class="secondary" hidden>Take my work with me</button>`) {
		t.Fatalf("My work:\n%s", page)
	}
	// The code a phone scans: sign-in with the username filled in, then My
	// work, which saves the work on the phone. No password or token in it.
	open := phoneURL("https://pifleet.hospital.org", "tess")
	if open != "https://pifleet.hospital.org/login?user=tess&next=%2F%3Ftake%3D1" || !strings.Contains(page, `popovertarget="phone-qr">Open my work on my phone</button>`) {
		t.Fatalf("phone link %q, page:\n%s", open, page)
	}
	if code, _, png := tess.get("/field/phone.png"); code != 200 || !strings.HasPrefix(png, "\x89PNG") {
		t.Fatalf("phone QR: %d", code)
	}
	phone := e.browser()
	if _, _, signin := phone.get("/login?user=tess&next=%2F%3Ftake%3D1"); !strings.Contains(signin, `name="username" value="tess" autocomplete="username" required>`) ||
		!strings.Contains(signin, `autocomplete="current-password" required autofocus>`) || !strings.Contains(signin, `name="next" value="/?take=1"`) {
		t.Fatalf("phone sign-in:\n%s", signin)
	}
	phone.get("/login?user=tess&next=%2F%3Ftake%3D1")
	if code, loc := phonePost(phone, "tess"); code != http.StatusSeeOther || loc != "/?take=1" {
		t.Fatalf("after signing in on the phone: %d %s", code, loc)
	}
	_, _, js := tess.get("/field/manifest")
	var m fieldManifest
	if err := json.Unmarshal([]byte(js), &m); err != nil {
		t.Fatalf("manifest %q: %v", js, err)
	}
	want := []string{"/", "/work-orders/" + mine, "/work-orders/" + mine + "/checklists/" + proc, "/assets/" + pump}
	if strings.Join(m.Pages, " ") != strings.Join(want, " ") || strings.Contains(js, other) || !strings.Contains(js, "/static/fonts/") {
		t.Fatalf("manifest: %+v", m)
	}
}

// phonePost signs in on the phone's sign-in page, returning where it goes.
func phonePost(b *browser, username string) (int, string) {
	b.e.t.Helper()
	resp, err := b.c.PostForm(b.e.srv.URL+"/login", url.Values{"username": {username}, "password": {"brass-kettle-orchard-7"}, "next": {"/?take=1"}})
	if err != nil {
		b.e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Location")
}
