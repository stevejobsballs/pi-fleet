package web

import (
	"net/url"
	"regexp"
	"strings"
	"testing"

	"pi-fleet/internal/domain"
)

// On a work order: the technician describes the work done, lists the test
// equipment used, adds test equipment that isn't registered yet without
// leaving the work order, and the printout has it all.
func TestWorkDoneAndTestEquipmentThroughTheUI(t *testing.T) {
	e := newEnv(t)
	e.user("mona", "Mona Mid", domain.RoleMidTier)
	tessID := e.user("tess", "Tess Tech", domain.RoleUser)
	pump, _ := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "PUMP-1", LocationID: e.loc, Manufacturer: "Baxter", Model: "Sigma"})
	ida, _ := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "IDA-1", LocationID: e.loc, Manufacturer: "Fluke", Model: "IDA-5", IsReferenceStandard: true})
	mona, tess := e.browser(), e.browser()
	mona.login("mona", "brass-kettle-orchard-7")
	tess.login("tess", "brass-kettle-orchard-7")
	_, page := mona.post("/work-orders", url.Values{"asset": {pump}, "type": {"corrective"}, "priority": {"normal"}, "title": {"Not pumping"}})
	wo := "/work-orders/" + regexp.MustCompile(`/work-orders/([0-9a-f-]{36})/assign`).FindStringSubmatch(page)[1]
	mona.post(wo+"/assign", url.Values{"assignee": {tessID}})
	tess.post(wo+"/status", url.Values{"to": {"in_progress"}})

	_, _, page = tess.get(wo)
	if !strings.Contains(page, `<a href="#work-done" class="step"><span>Work done</span></a><a href="#test-equipment" class="step"><span>Test equipment</span></a>`) || !strings.Contains(page, `name="work_done"`) ||
		!strings.Contains(page, `<option value="`+ida+`">IDA-1 · Fluke IDA-5 · no calibration schedule</option>`) {
		t.Fatalf("work order page:\n%s", page)
	}
	if _, page = tess.post(wo+"/work-done", url.Values{"work_done": {"Replaced the occlusion sensor.\nRan for 30 minutes, no alarms."}}); !strings.Contains(page, "Work done saved.") ||
		!strings.Contains(page, "Replaced the occlusion sensor.\nRan for 30 minutes, no alarms.</textarea>") {
		t.Fatalf("work done:\n%s", page)
	}
	if _, page = tess.post(wo+"/test-equipment", url.Values{"asset": {ida}}); !strings.Contains(page, "Test equipment added.") || !strings.Contains(page, `<a href="/assets/`+ida+`">IDA-1</a>`) {
		t.Fatalf("added:\n%s", page)
	}
	// Not registered yet: added there and then, back to the work order.
	if _, _, page = tess.get(wo + "/test-equipment/new"); !strings.Contains(page, `<option value="`+e.loc+`" selected>`) {
		t.Fatalf("new test equipment form:\n%s", page)
	}
	if _, page = tess.post(wo+"/test-equipment/new", url.Values{"tag": {"IDA-1"}, "manufacturer": {"Fluke"}, "model": {"X"}, "location": {e.loc}}); !strings.Contains(page, "Not added:") || !strings.Contains(page, `value="Fluke"`) {
		t.Fatalf("duplicate tag:\n%s", page)
	}
	_, page = tess.post(wo+"/test-equipment/new", url.Values{"tag": {"DMM-7"}, "manufacturer": {"Fluke"}, "model": {"87V"}, "serial": {"F87-1"}, "location": {e.loc}})
	if !strings.Contains(page, "Registered DMM-7 and added it to this work order. A mid-tier user sets up its calibration schedule") || !strings.Contains(page, ">DMM-7</a>") ||
		!strings.Contains(page, "<h1>NYC-WO-") {
		t.Fatalf("registered and added:\n%s", page)
	}
	if _, _, print := tess.get(wo + "/print"); !strings.Contains(print, "Ran for 30 minutes") || !strings.Contains(print, "DMM-7 · Fluke 87V · serial F87-1 · no calibration schedule") {
		t.Fatalf("printout:\n%s", print)
	}
	// Someone else sees it, but can't change it.
	if _, _, page = mona.get(wo); strings.Contains(page, `name="work_done"`) || !strings.Contains(page, "Ran for 30 minutes") || strings.Contains(page, "Not in the list?") {
		t.Fatalf("mid-tier view:\n%s", page)
	}
	if _, page = tess.post(wo+"/test-equipment/remove", url.Values{"asset": {ida}}); !strings.Contains(page, "Test equipment taken off the list.") {
		t.Fatalf("removed:\n%s", page)
	}
}
