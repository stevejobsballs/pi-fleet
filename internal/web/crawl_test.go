package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"pi-fleet/internal/app"
	"pi-fleet/internal/domain"
)

// seedEverything puts at least one of every kind of record in the
// fleet, so a crawl reaches every page.
func (e *env) seedEverything() {
	e.t.Helper()
	must := func(err error) {
		e.t.Helper()
		if err != nil {
			e.t.Fatal(err)
		}
	}
	e.flaggedCalibration() // mona (mid-tier), tess (user), a flagged calibration
	mona, err := domain.GetUserByUsername(e.ctx, e.app.Store.DB(), "mona")
	must(err)
	tess, err := domain.GetUserByUsername(e.ctx, e.app.Store.DB(), "tess")
	must(err)
	mid := app.Actor{UserID: mona.ID, SessionID: "m"}

	bos, err := e.app.CreateSite(e.ctx, e.super, "BOS", "Boston", "America/New_York")
	must(err)
	_, err = e.app.CreateLocation(e.ctx, e.super, bos, "", "OR 3", "room")
	must(err)
	vent, err := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "VENT-1", LocationID: e.loc, Manufacturer: "Hamilton", Model: "C6", Serial: "H-1"})
	must(err)
	_, err = e.app.RecordMeter(e.ctx, mid, vent, "hours", "1000", e.now.Add(-time.Hour), false)
	must(err)
	_, err = e.app.CreateSchedule(e.ctx, mid, domain.PMScheduleCreated{AssetID: vent, WOType: "pm", Title: "500 h PM",
		IntervalDays: 365, FirstDue: "2026-10-20", Meter: "hours", MeterInterval: "500"})
	must(err)
	proc, _, err := e.app.PublishProcedure(e.ctx, mid, "Ventilator PM", []domain.Step{
		{ID: "visual", Text: "Visual", Kind: domain.StepCheck, Required: true},
		{ID: "leak", Text: "Leakage", Kind: domain.StepNumber, Unit: "µA", Lower: "0", Upper: "300", Required: true},
	})
	must(err)
	wo, _, err := e.app.OpenWorkOrder(e.ctx, mid, app.NewWorkOrder{Type: "pm", AssetID: vent, Priority: "normal", Title: "PM", ProcedureID: proc})
	must(err)
	_, err = e.app.AssignWorkOrder(e.ctx, mid, wo, tess.ID)
	must(err)
	_, err = e.app.AddAttachment(e.ctx, e.super, domain.EntityAsset, vent, "manual.pdf", "service manual", []byte("%PDF-1.4 test"))
	must(err)
	part, err := e.app.CreatePart(e.ctx, e.super, domain.PartCreated{PartNo: "FUSE-2A", Description: "Fuse", Unit: "each"})
	must(err)
	shop, err := e.app.CreateStockLocation(e.ctx, e.super, domain.StockLocationCreated{SiteID: e.site, Name: "Biomed stockroom"})
	must(err)
	_, err = e.app.RecordStock(e.ctx, e.super, domain.StockTxnRecorded{Kind: domain.StockReceive, PartID: part, StockLocationID: shop, Quantity: 10})
	must(err)
	// Duplicates: merged with and without their history, and a pair waiting.
	twin := func(tag, masterID string) string {
		id, err := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: tag, LocationID: e.loc, Manufacturer: "Baxter", Model: "Sigma", MasterID: masterID})
		must(err)
		return id
	}
	kept := twin("PUMP-1", "M-1")
	must(e.app.MergeAssets(e.ctx, mid, kept, 1, twin("PROV-1", "M-1"), "registered twice", true))
	must(e.app.MergeAssets(e.ctx, mid, kept, 2, twin("OLD-1", "M-1"), "old database import", false))
	twin("PUMP-2", "M-2")
	twin("PROV-2", "M-2")
	kiosk, _, err := e.app.CreateKiosk(e.ctx, e.super, e.site, "nyc-shop")
	must(err)
	must(e.app.AddKioskMember(e.ctx, e.super, kiosk, tess.ID))
	// Locations proposed while registering equipment: one waiting for
	// review, one rejected (its equipment at the Unallocated site).
	tessActor := app.Actor{UserID: tess.ID, SessionID: "t"}
	propose := func(name, tag string) string {
		loc, err := e.app.ProposeLocation(e.ctx, tessActor, domain.LocationProposed{SiteID: e.site, Name: name, Kind: "room",
			Details: domain.LocationDetails{Building: "East", Floor: "2", Directions: "Past the lifts"}})
		must(err)
		_, err = e.app.RegisterAsset(e.ctx, tessActor, domain.AssetRegistered{Tag: tag, LocationID: loc, Manufacturer: "GE", Model: "Logiq"})
		must(err)
		return loc
	}
	// A satellite clinic inside the main site, with equipment of its own.
	north, err := e.app.CreateSiteIn(e.ctx, e.super, e.site, "NORTH", "North Satellite Clinic", "America/New_York")
	must(err)
	exam, err := e.app.CreateLocation(e.ctx, e.super, north, "", "Exam 2", "room")
	must(err)
	_, err = e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "ECG-9", LocationID: exam, Manufacturer: "GE", Model: "MAC 2000"})
	must(err)
	// Problems reported from QR labels: one waiting, one turned into work.
	_, _, err = e.app.SubmitRequest(e.ctx, domain.RequestSubmitted{AssetID: kept, Category: "Alarm or error message",
		Description: "Occlusion alarm with no line blocked", Name: "Nora Nurse", Department: "Ward 3B", Phone: "x4410"})
	must(err)
	reqID, _, err := e.app.SubmitRequest(e.ctx, domain.RequestSubmitted{AssetID: vent, Category: "Not working",
		Description: "Won't power on", Name: "Ray Resp", Department: "ICU", Phone: "x2200"})
	must(err)
	_, _, err = e.app.ConvertRequest(e.ctx, mid, reqID, "corrective", "high")
	must(err)
	// A checklist using every kind of step, being filled in by tess.
	defib, err := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "DEFIB-1", LocationID: e.loc, Manufacturer: "Zoll", Model: "R Series", MasterID: "ZL-77"})
	must(err)
	richProc, _, err := e.app.PublishChecklist(e.ctx, mid, domain.ProcedurePublished{Name: "Defibrillator inspection", Category: "Performance check",
		Reference: "IEC 60601-2-4", Description: "Yearly, and after any drop.", AppliesTo: []domain.EquipmentMatch{{Manufacturer: "Zoll", Model: "R Series"}},
		Steps: []domain.Step{
			{ID: "start", Text: "Before you start", Kind: domain.StepHeading},
			{ID: "warn", Text: "Discharge into the test load only. Keep clear of the paddles.", Kind: domain.StepInfo},
			{ID: "condition", Text: "Housing condition", Kind: domain.StepChoice, Required: true, NoteOnFail: true,
				Options: []domain.StepOption{{Label: "Good"}, {Label: "Worn"}, {Label: "Cracked", Fails: true}}},
			{ID: "battery", Text: "Battery fitted?", Kind: domain.StepChoice, Required: true, Options: []domain.StepOption{{Label: "Yes"}, {Label: "No"}}},
			{ID: "volts", Text: "Battery voltage", Kind: domain.StepNumber, Unit: "V", Lower: "11.5", Upper: "13", Required: true,
				ShowIf: &domain.StepCondition{Step: "battery", Equals: "Yes"}},
			{ID: "tests", Text: "Energy tests", Kind: domain.StepHeading},
			{ID: "energy", Text: "Delivered energy at 200 J", Kind: domain.StepNumber, Unit: "J", Lower: "170", Upper: "230", Required: true, Critical: true,
				Help: "Average of three discharges into the analyser."},
			{ID: "alarm", Text: "Alarms sound", Kind: domain.StepCheck, Required: true},
			{ID: "parts", Text: "Parts replaced", Kind: domain.StepMulti, Options: []domain.StepOption{{Label: "Pads"}, {Label: "Battery"}, {Label: "Cable"}}},
			{ID: "expiry", Text: "Pads expiry date", Kind: domain.StepDate},
			{ID: "notes", Text: "Observations", Kind: domain.StepLongText},
		}})
	must(err)
	richWO, _, err := e.app.OpenWorkOrder(e.ctx, mid, app.NewWorkOrder{Type: "inspection", AssetID: defib, Priority: "normal", Title: "Yearly inspection", ProcedureID: richProc})
	must(err)
	_, err = e.app.AssignWorkOrder(e.ctx, mid, richWO, tess.ID)
	must(err)
	tessAct := app.Actor{UserID: tess.ID, SessionID: "t"}
	must(e.app.ChangeWorkOrderStatus(e.ctx, tessAct, richWO, domain.WOInProgress, ""))
	must(e.app.RecordStep(e.ctx, tessAct, richWO, "condition", "Worn", ""))
	must(e.app.RecordStep(e.ctx, tessAct, richWO, "battery", "No", ""))
	must(e.app.RecordStep(e.ctx, tessAct, richWO, "energy", "150", "low on all three"))
	esa, _, err := e.app.PublishChecklist(e.ctx, mid, domain.ProcedurePublished{Name: "Electrical safety (IEC 62353)", Category: "Electrical safety",
		Reference: "IEC 62353", Steps: []domain.Step{
			{ID: "pe", Text: "Protective earth resistance", Kind: domain.StepNumber, Unit: "Ω", Upper: "0.3", Required: true, Critical: true},
			{ID: "leak", Text: "Equipment leakage current", Kind: domain.StepNumber, Unit: "µA", Upper: "500", Required: true, Critical: true},
		}})
	must(err)
	must(e.app.AddChecklist(e.ctx, tessAct, richWO, esa))
	// The sample portable X-ray checklist, for a GE AMX 4+, in progress.
	file, err := os.ReadFile("../../deploy/sample-checklists/portable-x-ray.checklist.json")
	must(err)
	var sample checklistFile
	must(json.Unmarshal(file, &sample))
	xray, err := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "XRAY-1", LocationID: e.loc, Manufacturer: "GE", Model: "AMX 4+", MasterID: "GE-AMX-0451"})
	must(err)
	xrayProc, _, err := e.app.PublishChecklist(e.ctx, mid, domain.ProcedurePublished{Name: sample.Name, Description: sample.Description, Reference: sample.Reference,
		Category: sample.Category, AppliesTo: []domain.EquipmentMatch{{Manufacturer: "GE", Model: "AMX 4+"}}, Steps: sample.Steps})
	must(err)
	xrayWO, _, err := e.app.OpenWorkOrder(e.ctx, mid, app.NewWorkOrder{Type: "pm", AssetID: xray, Priority: "normal", Title: "Yearly PM and QC", ProcedureID: xrayProc})
	must(err)
	_, err = e.app.AssignWorkOrder(e.ctx, mid, xrayWO, tess.ID)
	must(err)
	must(e.app.ChangeWorkOrderStatus(e.ctx, tessAct, xrayWO, domain.WOInProgress, ""))
	for _, a := range [][3]string{{"power", "Battery", ""}, {"housing", "pass", ""}, {"tube", "pass", ""}, {"cables", "pass", ""},
		{"kv80", "81.3", "Analyser RTI Piranha 657, calibration due 2027-03"}, {"kv60", "64.1", "high on three exposures"}} {
		must(e.app.RecordStep(e.ctx, tessAct, xrayWO, a[0], a[1], a[2]))
	}
	// Stat and high priority work, for Today › Open work orders.
	_, _, err = e.app.OpenWorkOrder(e.ctx, mid, app.NewWorkOrder{Type: "corrective", AssetID: vent, Priority: "urgent", Title: "Ventilator alarm not sounding"})
	must(err)
	_, _, err = e.app.OpenWorkOrder(e.ctx, mid, app.NewWorkOrder{Type: "corrective", AssetID: xray, Priority: "high", Title: "Drive judders when reversing"})
	must(err)
	// A custom checklist configured from the X-ray template, for the ICU's units.
	icu := append([]domain.Step{}, sample.Steps...)
	icu = append(icu, domain.Step{ID: "icubay", Text: "ICU bays: unit parks clear of the ventilator and monitor cables", Kind: domain.StepCheck, Required: true})
	_, _, err = e.app.PublishChecklist(e.ctx, mid, domain.ProcedurePublished{Name: "Portable X-ray PM: ICU units", Custom: true, Template: xrayProc,
		Category: sample.Category, Reference: sample.Reference, AppliesTo: []domain.EquipmentMatch{{Manufacturer: "GE", Model: "AMX 4+"}}, Steps: icu})
	must(err)
	// A checklist tess built, waiting for approval.
	_, err = e.app.ProposeChecklist(e.ctx, tessAct, domain.ProcedurePublished{Name: "Bed scale check", Category: "Performance check",
		Steps: []domain.Step{{ID: "s1", Text: "Reads 0.0 kg empty", Kind: domain.StepCheck, Required: true},
			{ID: "s2", Text: "Reading with the 50 kg test weight", Kind: domain.StepNumber, Unit: "kg", Lower: "49.5", Upper: "50.5", Required: true}}}, "")
	must(err)
	propose("Ultrasound 3", "US-1")
	must(e.app.ReviewLocation(e.ctx, e.super, propose("Closet B", "US-2"), false, "not a clinical area"))
}

var hrefRE = regexp.MustCompile(`<a\b[^>]*\bhref="([^"]*)"`)

// crawl follows every link on this site from the dashboard, as the
// signed-in person would by clicking, and reports any that fail.
func (b *browser) crawl(who string, limit int) int {
	b.e.t.Helper()
	seen := map[string]bool{"/": true}
	queue := []string{"/"}
	for len(queue) > 0 && len(seen) <= limit {
		path := queue[0]
		queue = queue[1:]
		resp, err := b.c.Get(b.e.srv.URL + path)
		if err != nil {
			b.e.t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		page := string(body)
		switch {
		case resp.StatusCode == http.StatusForbidden:
			b.e.t.Errorf("%s: GET %s is linked but forbidden to them", who, path)
		case resp.StatusCode != http.StatusOK:
			b.e.t.Errorf("%s: GET %s: %d %s", who, path, resp.StatusCode, resp.Header.Get("Location"))
		case strings.Contains(page, "something went wrong"):
			b.e.t.Errorf("%s: GET %s: error page", who, path)
		}
		if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
			continue
		}
		for _, m := range hrefRE.FindAllStringSubmatch(page, -1) {
			link := strings.ReplaceAll(m[1], "&amp;", "&")
			if i := strings.IndexByte(link, '#'); i >= 0 {
				link = link[:i]
			}
			if !strings.HasPrefix(link, "/") || strings.HasPrefix(link, "//") || link == "/logout" || strings.HasPrefix(link, "/display/") || seen[link] {
				continue
			}
			seen[link] = true
			queue = append(queue, link)
		}
	}
	if len(seen) > limit {
		b.e.t.Errorf("%s: more than %d links; raise the limit if the site really grew", who, limit)
	}
	return len(seen)
}

func TestCrawlEveryLink(t *testing.T) {
	e := newEnv(t)
	e.seedEverything()
	for _, p := range []struct{ who, pw string }{
		{"admin", "tumbleweed-gasket-42"},
		{"mona", "brass-kettle-orchard-7"},
		{"tess", "brass-kettle-orchard-7"},
	} {
		b := e.browser()
		b.login(p.who, p.pw)
		n := b.crawl(p.who, 400)
		t.Logf("%s: %d pages", p.who, n)
		if n < 15 {
			t.Errorf("%s: only %d pages reached; is the crawl broken?", p.who, n)
		}
	}
}

func TestCrawlEveryLinkOnAnEmployeePi(t *testing.T) {
	e := newEnv(t)
	e.seedEverything()
	piSrv, _, _ := e.employeePi(e.srv.URL, "pat") // served over plain HTTP on 127.0.0.1, like a real Pi
	piEnv := *e
	piEnv.srv = piSrv
	b := piEnv.browser()
	b.login("pat", "copper-ladder-sunrise")
	n := b.crawl("pat on the Pi", 400)
	t.Logf("pat: %d pages", n)
	if n < 10 {
		t.Errorf("only %d pages reached; is the crawl broken?", n)
	}
}

// record collects what the page checks report.
type record struct{ msgs []string }

func (r *record) Helper() {}
func (r *record) Errorf(format string, args ...any) {
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
}

func TestPageChecksCatchProblems(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://fleet-master.local:8443/x", nil)
	good := http.Header{
		"Content-Type":            {"text/html; charset=utf-8"},
		"Content-Security-Policy": {"default-src 'none'; style-src 'self'; form-action 'self'; frame-ancestors 'none'"},
		"X-Content-Type-Options":  {"nosniff"}, "X-Frame-Options": {"DENY"}, "Cache-Control": {"no-store"},
		"Referrer-Policy": {"same-origin"},
	}
	run := func(h http.Header, body string) []string {
		r := &record{}
		checkPage(r, req, &http.Response{StatusCode: 200, Header: h}, body)
		return r.msgs
	}
	fine := `<link rel="stylesheet" href="/static/style.css"><a href="https://example.org">x</a>
<form method="post" action="/a"><input type="hidden" name="csrf" value="t"></form>
<form method="post" action="/login"><input name="username"></form>
<form method="post" action="/up" enctype="multipart/form-data"><input type="hidden" name="csrf" value="t"><input type="file" name="f"></form>
<form method="get" action="/search"><input name="q"></form>`
	if msgs := run(good, fine); len(msgs) != 0 {
		t.Fatalf("a good page was reported: %q", msgs)
	}
	for _, c := range []struct{ body, want string }{
		{`<script>alert(1)</script>`, "<script>"},
		{`<p style="color:red">`, "inline style"},
		{`<button onclick="x()">`, "inline event handler"},
		{`<a href="javascript:x()">`, "javascript: URL"},
		{`<img src="https://cdn.example/x.png">`, "another origin"},
		{`<link rel="stylesheet" href="//cdn.example/x.css">`, "another origin"},
		{`<form method="post" action="/a"></form>`, "no csrf field"},
		{`<form method="post" action="https://evil.example/a"><input name="csrf"></form>`, "not on this site"},
		{`<form method="post" action="/up"><input name="csrf"><input type="file" name="f"></form>`, "file input"},
	} {
		if msgs := run(good, c.body); len(msgs) != 1 || !strings.Contains(msgs[0], c.want) {
			t.Errorf("%s: got %q, want one report containing %q", c.body, msgs, c.want)
		}
	}
	bad := good.Clone()
	bad.Set("Referrer-Policy", "no-referrer")
	if msgs := run(bad, ""); len(msgs) != 1 || !strings.Contains(msgs[0], "Origin: null") {
		t.Errorf("no-referrer: %q", msgs)
	}
	bad = good.Clone()
	bad.Del("X-Frame-Options")
	if msgs := run(bad, ""); len(msgs) != 1 {
		t.Errorf("missing header: %q", msgs)
	}
}
