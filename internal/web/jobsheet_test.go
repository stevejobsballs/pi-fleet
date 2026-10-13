package web

import (
	"net/url"
	"regexp"
	"strings"
	"testing"

	"pi-fleet/internal/app"
	"pi-fleet/internal/domain"
)

// The work order as a job sheet: where the equipment is and who to call,
// its recent history, the step bar, parts used from stock (and put back),
// putting work on hold with a reason, the equipment's status, and what's
// missing before signing.
func TestJobSheet(t *testing.T) {
	e := newEnv(t)
	tessID := e.user("tess", "Tess Tech", domain.RoleUser)
	icu, err := e.app.ProposeLocation(e.ctx, e.super, domain.LocationProposed{SiteID: e.site, Name: "ICU", Kind: "room",
		Details: domain.LocationDetails{Building: "Main", Floor: "3", Room: "312", Department: "Intensive care", Contact: "Charge nurse", Phone: "x4410"}})
	if err != nil {
		t.Fatal(err)
	}
	asset, _ := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "PUMP-1", LocationID: icu, Manufacturer: "Baxter", Model: "Sigma", Serial: "S-77"})
	open := func(title string) string {
		id, _, err := e.app.OpenWorkOrder(e.ctx, e.super, app.NewWorkOrder{Type: "corrective", AssetID: asset, Priority: "high", Title: title, Problem: "Occlusion alarm with no line blocked"})
		if err != nil {
			t.Fatal(err)
		}
		e.app.AssignWorkOrder(e.ctx, e.super, id, tessID)
		return id
	}
	open("Earlier job")
	woID := open("Alarm")
	wo := "/work-orders/" + woID
	part, _ := e.app.CreatePart(e.ctx, e.super, domain.PartCreated{PartNo: "BX-100", Description: "Occlusion sensor", Unit: "each"})
	van, _ := e.app.CreateStockLocation(e.ctx, e.super, domain.StockLocationCreated{SiteID: e.site, Name: "Van 3", OwnerUserID: tessID})
	if _, err := e.app.RecordStock(e.ctx, e.super, domain.StockTxnRecorded{Kind: domain.StockReceive, PartID: part, StockLocationID: van, Quantity: 4}); err != nil {
		t.Fatal(err)
	}
	tess := e.browser()
	tess.login("tess", "brass-kettle-orchard-7")
	tess.post(wo+"/status", url.Values{"to": {"in_progress"}})
	_, _, page := tess.get(wo)
	for _, want := range []string{
		`<span class="chip st-in_progress">in progress</span>`, `<span class="prio-tag high">High</span>`, `<p class="wo-title">Alarm</p>`,
		"Occlusion alarm with no line blocked", "Main, floor 3, room 312", "Intensive care", "Ask Charge nurse", `href="tel:x4410"`,
		"Recent work on it", "Earlier job", `href="#parts" class="step"`, `href="#finish" class="step"`,
		"BX-100 · Occlusion sensor — 4 each in Van 3 (Tess Tech)", "Nearly ready: worth doing first", "Describe the work done", "Record your time",
		"Sign and finish the work", "Sign: the work is done",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("job sheet lacks %q:\n%s", want, page)
		}
	}
	// A part used, then one put back.
	if _, page = tess.post(wo+"/parts", url.Values{"part": {part + "|" + van}, "quantity": {"2"}}); !strings.Contains(page, "Part added: taken from stock.") ||
		!strings.Contains(page, `<strong class="pl-qty">2</strong>`) || !strings.Contains(page, "— 2 each in Van 3") {
		t.Fatalf("part used:\n%s", page)
	}
	if !regexp.MustCompile(`href="#parts" class="step done">.*<span>Parts</span><span class="badge">1</span>`).MatchString(page) {
		t.Fatalf("step bar after a part:\n%s", page)
	}
	if _, page = tess.post(wo+"/parts", url.Values{"part": {part + "|" + van}, "quantity": {"0"}}); !strings.Contains(page, "Not saved: the quantity") {
		t.Fatalf("no quantity:\n%s", page)
	}
	if _, page = tess.post(wo+"/parts/return", url.Values{"part": {part}, "location": {van}, "quantity": {"1"}}); !strings.Contains(page, "Put back into stock.") ||
		!strings.Contains(page, "— 3 each in Van 3") || !strings.Contains(page, `<strong class="pl-qty">−1</strong>`) {
		t.Fatalf("put back:\n%s", page)
	}
	// Out of service from the finish panel, back on the work order.
	_, page = tess.post("/assets/"+asset+"/status", url.Values{"status": {"out_of_service"}, "reason": {"sensor failed"}, "version": {"1"}, "back": {wo + "#finish"}})
	if !strings.Contains(page, "Status changed.") || !strings.Contains(page, `<p class="wo-title">Alarm</p>`) || !strings.Contains(page, "Put it back in service") {
		t.Fatalf("equipment status from the work order:\n%s", page)
	}
	// On hold with a reason, then resumed.
	if _, page = tess.post(wo+"/status", url.Values{"to": {"on_hold"}, "reason": {"Waiting for parts"}, "detail": {"sensor on order"}}); !strings.Contains(page, "On hold.") ||
		!strings.Contains(page, "Resume the work") {
		t.Fatalf("on hold:\n%s", page)
	}
	var reason string
	e.app.Store.DB().QueryRow(`SELECT payload FROM events WHERE type = ? ORDER BY local_order DESC LIMIT 1`, domain.TypeWorkOrderStatusChanged).Scan(&reason)
	if !strings.Contains(reason, "Waiting for parts: sensor on order") {
		t.Fatalf("hold reason: %s", reason)
	}
	tess.post(wo+"/status", url.Values{"to": {"in_progress"}})
	// Ready once the work is described and time recorded: signing with
	// the clock running stops it.
	tess.post(wo+"/work-done", url.Values{"work_done": {"Replaced the occlusion sensor."}})
	tess.post(wo+"/clock/start", url.Values{"kind": {"labor"}})
	if _, _, page = tess.get(wo); !strings.Contains(page, "Ready to finish") || !strings.Contains(page, "Your clock is running: signing stops it") {
		t.Fatalf("ready:\n%s", page)
	}
	if _, page = tess.post(wo+"/sign", url.Values{"meaning": {"performed"}, "password": {"brass-kettle-orchard-7"}, "ack_clock": {"yes"}}); !strings.Contains(page, "Signed.") ||
		!strings.Contains(page, `<span class="chip st-completed">completed</span>`) {
		t.Fatalf("signed:\n%s", page)
	}
	if _, ok, _ := domain.RunningClock(e.ctx, e.app.Store.DB(), tessID); ok {
		t.Fatal("clock still running after signing the work done")
	}
}
