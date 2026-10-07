package web

import (
	"fmt"
	"io"
	"net/http"
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
			if !strings.HasPrefix(link, "/") || strings.HasPrefix(link, "//") || link == "/logout" || seen[link] {
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
