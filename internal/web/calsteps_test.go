package web

import (
	"net/url"
	"regexp"
	"strings"
	"testing"

	"pi-fleet/internal/domain"
)

// Calibration modules through the screens: a repair template built with a
// reference standards step and a calibration point; the technician ticks
// the standard, enters as-found and as-left, and sees the results; the
// spreadsheet has them too.
func TestCalibrationModulesThroughTheUI(t *testing.T) {
	e := newEnv(t)
	e.user("mona", "Mona Mid", domain.RoleMidTier)
	tessID := e.user("tess", "Tess Tech", domain.RoleUser)
	pump, _ := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "PUMP-1", LocationID: e.loc, Manufacturer: "Baxter", Model: "Sigma"})
	std, _ := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "STD-1", LocationID: e.loc, Manufacturer: "Fluke", Model: "IDA-5", IsReferenceStandard: true})
	mona, tess := e.browser(), e.browser()
	mona.login("mona", "brass-kettle-orchard-7")
	tess.login("tess", "brass-kettle-orchard-7")

	if _, _, page := mona.get("/procedures/new"); !strings.Contains(page, ">Calibration point (as found / as left)</option>") ||
		!strings.Contains(page, ">Reference standards used</option>") || !strings.Contains(page, `name="nominal_0"`) {
		t.Fatalf("builder lacks the calibration modules:\n%s", page)
	}
	_, page := mona.post("/procedures", url.Values{"name": {"Pump repair"}, "count": {"3"}, "category": {"Repair"},
		"text_0": {"Fault found"}, "kind_0": {"longtext"}, "required_0": {"yes"},
		"text_1": {"Standards used"}, "kind_1": {"standards"}, "required_1": {"yes"},
		"text_2": {"Flow at 25 mL/h"}, "kind_2": {"calibration"}, "unit_2": {"mL/h"}, "nominal_2": {"25"}, "tolkind_2": {"pct"}, "tolvalue_2": {"5"},
		"lower_2": {"ignored"}, "required_2": {"yes"}})
	if !strings.Contains(page, "Published Pump repair version 1") || !strings.Contains(page, "in mL/h, nominal 25, passes from 23.75 to 26.25") {
		t.Fatalf("publish:\n%s", page)
	}
	proc := regexp.MustCompile(`/procedures/([0-9a-f-]{36})/retire`).FindStringSubmatch(page)[1]

	_, page = mona.post("/work-orders", url.Values{"asset": {pump}, "type": {"corrective"}, "priority": {"normal"}, "title": {"Not pumping"}, "procedure": {proc}})
	wo := "/work-orders/" + regexp.MustCompile(`/work-orders/([0-9a-f-]{36})/assign`).FindStringSubmatch(page)[1]
	mona.post(wo+"/assign", url.Values{"assignee": {tessID}})
	tess.post(wo+"/status", url.Values{"to": {"in_progress"}})
	cl := wo + "/checklists/" + proc
	if _, _, page = tess.get(cl); !strings.Contains(page, `name="l_s3"`) || !strings.Contains(page, `value="`+std+`"`) || !strings.Contains(page, "no calibration schedule") {
		t.Fatalf("fill page:\n%s", page)
	}
	_, page = tess.post(wo+"/checklist", url.Values{"procedure": {proc}, "v_s1": {"Cracked guide"}, "v_s2": {std}, "v_s3": {"26.5"}, "l_s3": {"25.1"}})
	if !strings.Contains(page, "Saved 3 answers.") || !strings.Contains(page, "As found <strong>26.5 mL/h</strong> <span class=\"fail\">fail</span>") ||
		!strings.Contains(page, "as left <strong>25.1 mL/h</strong> <span class=\"pass\">pass</span>") || !strings.Contains(page, "<strong>STD-1</strong>") {
		t.Fatalf("answered:\n%s", page)
	}
	// Saving again unchanged saves nothing.
	if _, page = tess.post(wo+"/checklist", url.Values{"procedure": {proc}, "v_s1": {"Cracked guide"}, "v_s2": {std}, "v_s3": {"26.5"}, "l_s3": {"25.1"}}); !strings.Contains(page, "Nothing new to save.") {
		t.Fatalf("resaved:\n%s", page)
	}
	if _, _, csv := tess.get(cl + "/checklist.csv"); !strings.Contains(csv, "as found 26.5 mL/h (fail); as left 25.1 mL/h (pass)") || !strings.Contains(csv, "23.75 to 26.25 mL/h") {
		t.Fatalf("spreadsheet:\n%s", csv)
	}
	if _, _, page = tess.get(wo); strings.Contains(page, "Record calibration") {
		t.Fatalf("separate calibration form offered:\n%s", page)
	}
}
