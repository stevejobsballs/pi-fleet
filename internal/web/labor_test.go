package web

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"pi-fleet/internal/app"
	"pi-fleet/internal/domain"
)

// The work clock: Start work asks labour or travel and runs a clock; Stop
// work (here, on another page, or by switching) records the time as a
// line; time can be typed in instead, and any line changed afterwards.
func TestWorkClock(t *testing.T) {
	e := newEnv(t)
	e.user("mona", "Mona Mid", domain.RoleMidTier)
	tessID := e.user("tess", "Tess Tech", domain.RoleUser)
	e.user("ulf", "Ulf User", domain.RoleUser)
	asset, _ := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "PUMP-1", LocationID: e.loc, Manufacturer: "Baxter", Model: "Sigma"})
	open := func(title string) string {
		id, _, err := e.app.OpenWorkOrder(e.ctx, e.super, app.NewWorkOrder{Type: "corrective", AssetID: asset, Priority: "normal", Title: title, Problem: "Alarm"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.app.AssignWorkOrder(e.ctx, e.super, id, tessID); err != nil {
			t.Fatal(err)
		}
		return "/work-orders/" + id
	}
	wo, wo2 := open("Alarm"), open("Second job")
	tess := e.browser()
	tess.login("tess", "brass-kettle-orchard-7")
	// Time passes while Tess keeps using pi-fleet (no idle sign-out).
	pass := func(d time.Duration) {
		for d > 0 {
			step := min(d, 10*time.Minute)
			e.now = e.now.Add(step)
			d -= step
			tess.get("/session")
			tess.get("/today/open")
		}
	}
	status := func(path string) string {
		w, _ := domain.GetWorkOrder(e.ctx, e.app.Store.DB(), strings.TrimPrefix(path, "/work-orders/"))
		return w.Status
	}

	// Not started: Start work, asking labour or travel.
	_, _, page := tess.get(wo)
	for _, want := range []string{"Start work</summary>", "What are you doing?", `name="kind" value="labor" class="kind labor"`, `name="kind" value="travel" class="kind travel"`, "Add time by hand"} {
		if !strings.Contains(page, want) {
			t.Fatalf("lacks %q:\n%s", want, page)
		}
	}
	// Travel first: the clock runs, and the work is now in progress.
	_, page = tess.post(wo+"/clock/start", url.Values{"kind": {"travel"}})
	if !strings.Contains(page, "Travel clock started.") || !strings.Contains(page, "Stop work") || !strings.Contains(page, "Arrived: switch to labour") ||
		!strings.Contains(page, `class="timecard running travel"`) || status(wo) != domain.WOInProgress {
		t.Fatalf("travel started (%s):\n%s", status(wo), page)
	}
	// Arrived: one press switches to labour, recording the travel.
	pass(25 * time.Minute)
	if _, page = tess.post(wo+"/clock/start", url.Values{"kind": {"labor"}}); !strings.Contains(page, "Recorded 0:25 of travel on") || !strings.Contains(page, "Labour clock started.") {
		t.Fatalf("switched:\n%s", page)
	}
	// Elsewhere, a strip shows the running clock, with Stop work.
	pass(70 * time.Minute)
	_, _, page = tess.get("/assets")
	if !strings.Contains(page, `class="timer-strip"`) || !strings.Contains(page, "<strong>Labour</strong> on") || !strings.Contains(page, ">1:10</span>") {
		t.Fatalf("strip:\n%s", page)
	}
	if _, page = tess.post("/clock/stop", url.Values{"back": {"/assets"}}); !strings.Contains(page, "Recorded 1:10 of labour") || strings.Contains(page, `class="timer-strip"`) {
		t.Fatalf("stopped from the strip:\n%s", page)
	}
	_, _, page = tess.get(wo)
	from := e.now.Add(-95 * time.Minute).Local().Format("15:04")
	for _, want := range []string{"Labour <strong>1:10</strong>", "Travel <strong>0:25</strong>", "· " + from + "–", `<strong class="tl-time">0:25</strong>`} {
		if !strings.Contains(page, want) {
			t.Fatalf("time lines lack %q:\n%s", want, page)
		}
	}

	// Typed in instead: travel back.
	_, page = tess.post(wo+"/labor", url.Values{"kind": {"travel"}, "time": {"0:30"}, "date": {"2026-10-06"}, "note": {"back to the shop"}})
	if !strings.Contains(page, "Travel time added.") || !strings.Contains(page, "Travel <strong>0:55</strong>") || !strings.Contains(page, "typed in") {
		t.Fatalf("typed in:\n%s", page)
	}
	// Changed afterwards: the labour was really an hour and a half.
	ids := regexp.MustCompile(`action="/labor/([0-9a-f-]{36})/edit"`).FindAllStringSubmatch(page, -1)
	if len(ids) != 3 {
		t.Fatalf("edit forms: %d", len(ids))
	}
	var labour string
	for _, id := range ids {
		var kind string
		e.app.Store.DB().QueryRow(`SELECT kind FROM labor_entries WHERE id = ?`, id[1]).Scan(&kind)
		if kind == domain.LaborKindLabor {
			labour = id[1]
		}
	}
	if _, page = tess.post("/labor/"+labour+"/edit", url.Values{"kind": {"labor"}, "time": {"1:30"}, "date": {"2026-10-06"}, "note": {"replaced the cable"}}); !strings.Contains(page, "Time changed.") ||
		!strings.Contains(page, "Labour <strong>1:30</strong>") || !strings.Contains(page, "replaced the cable") {
		t.Fatalf("changed:\n%s", page)
	}
	// Someone else (not mid-tier) can't change Tess's time.
	ulf := e.browser()
	ulf.login("ulf", "brass-kettle-orchard-7")
	if _, _, page = ulf.get(wo); strings.Contains(page, "/labor/"+labour+"/edit") {
		t.Fatal("edit offered to someone else")
	}
	if _, page = ulf.post("/labor/"+labour+"/edit", url.Values{"kind": {"travel"}, "time": {"9:00"}, "date": {"2026-10-06"}}); strings.Contains(page, "Time changed.") {
		t.Fatal("someone else changed Tess's time")
	}

	// One clock per person: starting on another job stops this one.
	tess.post(wo+"/clock/start", url.Values{"kind": {"labor"}})
	pass(15 * time.Minute)
	if _, page = tess.post(wo2+"/clock/start", url.Values{"kind": {"labor"}}); !strings.Contains(page, "Recorded 0:15 of labour") {
		t.Fatalf("second job:\n%s", page)
	}
	// Mona sees Tess's clock on the job and can stop it.
	mona := e.browser()
	mona.login("mona", "brass-kettle-orchard-7")
	if _, _, page = mona.get(wo2); !strings.Contains(page, "Tess Tech: labour since") || !strings.Contains(page, "Stop their clock") {
		t.Fatalf("others' clocks:\n%s", page)
	}
	// Putting the job on hold stops the clock; under a minute records nothing.
	pass(20 * time.Second)
	if _, page = tess.post(wo2+"/status", url.Values{"to": {"on_hold"}, "reason": {"waiting for parts"}}); strings.Contains(page, `class="timecard running`) {
		t.Fatalf("on hold, clock still running:\n%s", page)
	}
	if _, ok, _ := domain.RunningClock(e.ctx, e.app.Store.DB(), tessID); ok {
		t.Fatal("clock still running")
	}
	var n int
	e.app.Store.DB().QueryRow(`SELECT count(*) FROM labor_entries WHERE wo_id = ?`, strings.TrimPrefix(wo2, "/work-orders/")).Scan(&n)
	if n != 0 {
		t.Fatalf("%d entries for 20 seconds", n)
	}
	// Bad input is refused.
	if c, _ := tess.post(wo+"/clock/start", url.Values{"kind": {"lunch"}}); c != http.StatusSeeOther {
		t.Fatalf("bad kind: %d", c)
	}
	if _, ok, _ := domain.RunningClock(e.ctx, e.app.Store.DB(), tessID); ok {
		t.Fatal("started a clock for an unknown kind")
	}
	// The printable record lists both kinds.
	if _, _, print := tess.get(wo + "/print"); !strings.Contains(print, "Time (labour 1:45, travel 0:55)") || !strings.Contains(print, "Travel "+from+"–") {
		t.Fatalf("printable record:\n%s", print)
	}
}
