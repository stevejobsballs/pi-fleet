package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"pi-fleet/internal/app"
	"pi-fleet/internal/domain"
)

// One search box: an exact tag, MasterID, serial or work order number
// opens it; anything else lists equipment, work orders and checklists.
func TestSearch(t *testing.T) {
	e := newEnv(t)
	pump, _ := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "PUMP-7", LocationID: e.loc, Manufacturer: "Baxter", Model: "Sigma", Serial: "SN-991", MasterID: "M-1007"})
	e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "PUMP-8", LocationID: e.loc, Manufacturer: "Baxter", Model: "Sigma"})
	wo, number, err := e.app.OpenWorkOrder(e.ctx, e.super, app.NewWorkOrder{Type: "corrective", AssetID: pump, Priority: "normal", Title: "Occlusion alarm"})
	if err != nil {
		t.Fatal(err)
	}
	b := e.browser()
	b.login("admin", "tumbleweed-gasket-42")
	for q, want := range map[string]string{"pump-7": "/assets/" + pump, "M-1007": "/assets/" + pump, "sn-991": "/assets/" + pump, number: "/work-orders/" + wo} {
		if code, loc, _ := b.get("/search?q=" + url.QueryEscape(q)); code != http.StatusSeeOther || loc != want {
			t.Errorf("search %q: %d %q, want %q", q, code, loc, want)
		}
	}
	_, _, page := b.get("/search?q=sigma")
	if !strings.Contains(page, `href="/assets/`+pump+`"`) || !strings.Contains(page, "PUMP-8") || !strings.Contains(page, `<h2>Equipment <span class="count">2</span></h2>`) {
		t.Fatalf("search results:\n%s", page)
	}
	if _, _, page = b.get("/search?q=occlusion"); !strings.Contains(page, number) {
		t.Fatalf("work order not found:\n%s", page)
	}
	if _, _, page = b.get("/search?q=nothing-like-this"); !strings.Contains(page, "Nothing matches") {
		t.Fatalf("no match:\n%s", page)
	}
	// Every other page has the small search box in its header.
	if _, _, page = b.get("/schedules"); !strings.Contains(page, `class="find find-bar"`) {
		t.Fatalf("no header search:\n%s", page)
	}
}

// A new work order or schedule starts by searching for the equipment,
// not by scrolling a list of all of it.
func TestChoosingEquipment(t *testing.T) {
	e := newEnv(t)
	pump, _ := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "PUMP-7", LocationID: e.loc, Manufacturer: "Baxter", Model: "Sigma"})
	e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "VENT-1", LocationID: e.loc, Manufacturer: "Hamilton", Model: "C6"})
	b := e.browser()
	b.login("admin", "tumbleweed-gasket-42")
	_, _, page := b.get("/work-orders/new")
	if strings.Contains(page, `name="asset"`) || !strings.Contains(page, "Which equipment?") {
		t.Fatalf("no search first:\n%s", page)
	}
	_, _, page = b.get("/work-orders/new?q=baxter")
	if !strings.Contains(page, `href="/work-orders/new?asset=`+pump+`"`) || strings.Contains(page, "VENT-1") {
		t.Fatalf("matches:\n%s", page)
	}
	_, _, page = b.get("/work-orders/new?asset=" + pump)
	if !strings.Contains(page, `<input type="hidden" name="asset" value="`+pump+`">`) || !strings.Contains(page, "PUMP-7 · Baxter Sigma") {
		t.Fatalf("chosen:\n%s", page)
	}
	if _, page = b.post("/work-orders", url.Values{"asset": {pump}, "type": {"corrective"}, "priority": {"normal"}, "title": {"Alarm"}}); !strings.Contains(page, "Opened NYC-WO-") {
		t.Fatalf("open:\n%s", page)
	}
	if _, _, page = b.get("/schedules?q=sigma#new"); !strings.Contains(page, `href="/schedules?asset=`+pump+`#new"`) {
		t.Fatalf("schedule matches:\n%s", page)
	}
	_, page = b.post("/schedules", url.Values{"asset": {pump}, "type": {"pm"}, "title": {"Yearly PM"}, "interval": {"365"}, "first_due": {"2026-10-20"}})
	if !strings.Contains(page, "Schedule created.") || !strings.Contains(page, "Next scheduled: <strong>Yearly PM</strong>") {
		t.Fatalf("schedule created:\n%s", page)
	}
}

// My work: my jobs as cards, overdue first; and the equipment page says
// what needs doing.
func TestMyWorkAndEquipmentHub(t *testing.T) {
	e := newEnv(t)
	tessID := e.user("tess", "Tess Tech", domain.RoleUser)
	pump, _ := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "PUMP-7", LocationID: e.loc, Manufacturer: "Baxter", Model: "Sigma"})
	open := func(title, due string) string {
		id, _, err := e.app.OpenWorkOrder(e.ctx, e.super, app.NewWorkOrder{Type: "corrective", AssetID: pump, Priority: "normal", Title: title, DueAt: due})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.app.AssignWorkOrder(e.ctx, e.super, id, tessID); err != nil {
			t.Fatal(err)
		}
		return id
	}
	open("Later job", "2026-10-20")
	open("Late job", "2026-10-01")
	open("Today's job", "2026-10-06")
	b := e.browser()
	b.login("tess", "brass-kettle-orchard-7")
	_, _, page := b.get("/")
	iLate, iToday, iLater := strings.Index(page, "Late job"), strings.Index(page, "Today&#39;s job"), strings.Index(page, "Later job")
	if iLate < 0 || iToday < iLate || iLater < iToday || !strings.Contains(page, "Overdue since Thursday 1 October") ||
		!strings.Contains(page, "Due today") || !strings.Contains(page, `<span class="count">3, 1 overdue</span>`) || !strings.Contains(page, "Biomed shop, New York") {
		t.Fatalf("my work:\n%s", page)
	}
	_, _, page = b.get("/assets/" + pump)
	iNow, iManage := strings.Index(page, "What needs doing"), strings.Index(page, "Manage this equipment")
	if iNow < 0 || iManage < iNow || !strings.Contains(page, "Overdue since Thursday 1 October") || !strings.Contains(page, "Tess Tech") {
		t.Fatalf("equipment hub:\n%s", page)
	}
}

// Scanning a QR label: staff who are signed in land on the equipment;
// anyone else gets the report form, with a way for staff to sign in.
func TestScanningALabel(t *testing.T) {
	e := newEnv(t)
	pump, _ := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "PUMP-7", LocationID: e.loc, Manufacturer: "Baxter", Model: "Sigma", MasterID: "M-1007"})
	b := e.browser()
	_, _, page := b.get("/r/M-1007")
	signIn := "/login?next=" + url.QueryEscape("/assets/"+pump)
	if !strings.Contains(page, "Send report") || !strings.Contains(page, `href="`+signIn+`"`) {
		t.Fatalf("report page:\n%s", page)
	}
	// Signing in from there comes back to the equipment.
	b.get(signIn)
	code, page := b.post("/login", url.Values{"username": {"admin"}, "password": {"tumbleweed-gasket-42"}, "next": {"/assets/" + pump}})
	if code != http.StatusSeeOther || !strings.Contains(page, "What needs doing") {
		t.Fatalf("after signing in: %d\n%s", code, page)
	}
	if code, loc, _ := b.get("/r/M-1007"); code != http.StatusSeeOther || loc != "/assets/"+pump {
		t.Fatalf("signed-in scan: %d %q", code, loc)
	}
	// Only pages on this site.
	b.post("/logout", nil)
	b.get("/login")
	if code, page = b.post("/login", url.Values{"username": {"admin"}, "password": {"tumbleweed-gasket-42"}, "next": {"//evil.example/"}}); !strings.Contains(page, "<h1>My work") {
		t.Fatalf("offsite next: %d\n%s", code, page)
	}
}
