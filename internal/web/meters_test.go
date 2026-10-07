package web

import (
	"net/url"
	"regexp"
	"strings"
	"testing"

	"pi-fleet/internal/domain"
)

func TestMetersAndUsageSchedulesThroughTheUI(t *testing.T) {
	e := newEnv(t) // 2026-10-06 09:00 UTC; site is America/New_York
	e.user("mona", "Mona Mid", domain.RoleMidTier)
	vent, _ := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "VENT-1", LocationID: e.loc, Manufacturer: "Hamilton", Model: "C6"})
	other, _ := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "PUMP-1", LocationID: e.loc, Manufacturer: "Baxter", Model: "Sigma"})
	b := e.browser()
	b.login("mona", "brass-kettle-orchard-7")

	_, _, page := b.get("/assets/" + vent)
	if !strings.Contains(page, `value="2026-10-06T05:00"`) {
		t.Fatalf("reading time not defaulted to site local time:\n%s", page)
	}
	_, page = b.post("/assets/"+vent+"/meters", url.Values{"meter": {"hours"}, "value": {"1000"}, "read_at": {"2026-10-06T04:00"}})
	if !strings.Contains(page, "Reading recorded") || !strings.Contains(page, "2026-10-06T08:00:00Z") {
		t.Fatalf("reading (entered in New York time, stored UTC):\n%s", page)
	}
	if _, page = b.post("/assets/"+vent+"/meters", url.Values{"meter": {"hours"}, "value": {"900"}, "read_at": {"2026-10-06T04:30"}}); !strings.Contains(page, "reset or replaced") {
		t.Fatalf("lower reading accepted:\n%s", page)
	}

	b.get("/schedules")
	b.post("/schedules", url.Values{"asset": {vent}, "type": {"pm"}, "title": {"500-hour PM"}, "interval": {"365"}, "grace": {"0"},
		"first_due": {"2027-09-01"}, "meter": {"hours"}, "meter_interval": {"500"}})
	b.post("/schedules", url.Values{"asset": {other}, "type": {"pm"}, "title": {"Annual PM"}, "interval": {"365"}, "grace": {"0"}, "first_due": {"2027-05-01"}})
	_, _, page = b.get("/schedules")
	if !strings.Contains(page, "or 500 hours <small>(0 used)</small>") || strings.Count(page, " used)</small>") != 1 {
		t.Fatalf("usage shown wrongly in the schedule list:\n%s", page)
	}

	b.get("/assets/" + vent)
	b.post("/assets/"+vent+"/meters", url.Values{"meter": {"hours"}, "value": {"1455"}, "read_at": {"2026-10-06T04:45"}})
	_, _, page = b.get("/")
	if !regexp.MustCompile(`Due by usage[\s\S]*VENT-1[\s\S]*455 of 500 hours`).MatchString(page) {
		t.Fatalf("dashboard lacks the usage-due schedule:\n%s", page)
	}
	if strings.Contains(page, "PUMP-1") {
		t.Fatal("calendar-only schedule listed as due by usage")
	}
}
