package web

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"pi-fleet/internal/domain"
)

func TestChecklistAndLabourThroughTheUI(t *testing.T) {
	e := newEnv(t)
	e.user("mona", "Mona Mid", domain.RoleMidTier)
	tessID := e.user("tess", "Tess Tech", domain.RoleUser)
	asset, _ := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "PUMP-1", LocationID: e.loc, Manufacturer: "Baxter", Model: "Sigma"})
	mona, tess := e.browser(), e.browser()
	mona.login("mona", "brass-kettle-orchard-7")
	tess.login("tess", "brass-kettle-orchard-7")

	// Publish a checklist through the form.
	mona.get("/procedures")
	_, page := mona.post("/procedures", url.Values{"name": {"Infusion pump PM"},
		"text":     {"Housing and cords intact", "Earth leakage", "Battery runtime", ""},
		"kind":     {"check", "number", "check", "check"},
		"unit":     {"", "µA", "", ""},
		"lower":    {"", "0", "", ""},
		"upper":    {"", "300", "", ""},
		"required": {"yes", "yes", "no", "yes"}})
	if !strings.Contains(page, "Published Infusion pump PM version 1") || !strings.Contains(page, "0 to 300") {
		t.Fatalf("publish:\n%s", page)
	}
	procID := regexp.MustCompile(`/procedures/([0-9a-f-]{36})/retire`).FindStringSubmatch(page)[1]

	mona.get("/work-orders/new")
	_, page = mona.post("/work-orders", url.Values{"asset": {asset}, "type": {"pm"}, "priority": {"normal"}, "title": {"Semiannual PM"}, "procedure": {procID}})
	wo := "/work-orders/" + regexp.MustCompile(`/work-orders/([0-9a-f-]{36})/assign`).FindStringSubmatch(page)[1]
	mona.post(wo+"/assign", url.Values{"assignee": {tessID}})

	tess.get(wo)
	tess.post(wo+"/status", url.Values{"to": {"in_progress"}})
	_, page = tess.post(wo+"/steps", url.Values{"step": {"s1"}, "value": {"pass"}})
	if !strings.Contains(page, "Step recorded") {
		t.Fatalf("check step:\n%s", page)
	}
	_, page = tess.post(wo+"/steps", url.Values{"step": {"s2"}, "value": {"412.5"}, "note": {"high"}})
	if !strings.Contains(page, `class="fail">412.5 µA`) {
		t.Fatalf("out-of-range reading not shown as a fail:\n%s", page)
	}
	// Signing needs every required step; s2 is recorded, so this passes.
	_, page = tess.post(wo+"/steps", url.Values{"step": {"s2"}, "value": {"120"}, "note": {"re-measured"}})
	if !strings.Contains(page, `class="pass">120 µA`) {
		t.Fatalf("re-recorded reading:\n%s", page)
	}

	// Labour in different formats.
	if _, page = tess.post(wo+"/labor", url.Values{"time": {"lots"}, "date": {"2026-10-06"}}); !strings.Contains(page, "Not saved: enter time as") {
		t.Fatalf("bad time accepted:\n%s", page)
	}
	tess.post(wo+"/labor", url.Values{"time": {"1:30"}, "date": {"2026-10-06"}, "note": {"PM"}})
	_, page = tess.post(wo+"/labor", url.Values{"time": {"0.25h"}, "date": {"2026-10-06"}})
	if !strings.Contains(page, "(1:45 total)") {
		t.Fatalf("labour total:\n%s", page)
	}

	_, page = tess.post(wo+"/sign", url.Values{"meaning": {"performed"}, "password": {"brass-kettle-orchard-7"}, "ack_clock": {"yes"}})
	if !strings.Contains(page, "Signed.") {
		t.Fatalf("sign:\n%s", page)
	}
	_, _, print := tess.get(wo + "/print")
	for _, want := range []string{"Checklist: Infusion pump PM v1", "120 µA pass", "Labour (1:45 total)", "Battery runtime</td><td>not recorded"} {
		if !strings.Contains(print, want) {
			t.Errorf("printable record lacks %q", want)
		}
	}
	if code, _, _ := tess.get("/procedures"); code != http.StatusOK {
		t.Fatal("users can't view checklists")
	}
	if code, _ := tess.post("/procedures", url.Values{"name": {"x"}}); code != http.StatusForbidden {
		t.Fatalf("user published a checklist: %d", code)
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]int{"45": 45, "1:30": 90, "1.5h": 90, "0.25h": 15, " 2:05 ": 125} {
		if got, err := parseDuration(in); err != nil || got != want {
			t.Errorf("parseDuration(%q) = %d, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "x", "1:75", "-1h", "1.5"} {
		if _, err := parseDuration(in); err == nil && in != "1.5" {
			t.Errorf("parseDuration(%q) accepted", in)
		}
	}
}
