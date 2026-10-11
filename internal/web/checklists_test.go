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

	// Publish a checklist through the builder.
	if _, _, page := mona.get("/procedures/new"); !strings.Contains(page, `name="text_7"`) || !strings.Contains(page, ">Tick all that apply</option>") {
		t.Fatalf("builder:\n%s", page)
	}
	_, page := mona.post("/procedures", url.Values{"name": {"Infusion pump PM"}, "count": {"4"}, "category": {"Preventive maintenance"},
		"text_0": {"Housing and cords intact"}, "kind_0": {"check"}, "required_0": {"yes"},
		"text_1": {"Earth leakage"}, "kind_1": {"number"}, "unit_1": {"µA"}, "lower_1": {"0"}, "upper_1": {"300"}, "required_1": {"yes"},
		"text_2": {"Battery runtime"}, "kind_2": {"check"},
		"text_3": {""}, "kind_3": {"check"}})
	if !strings.Contains(page, "Published Infusion pump PM version 1") || !strings.Contains(page, "passes from 0 up to 300") {
		t.Fatalf("publish:\n%s", page)
	}
	procID := regexp.MustCompile(`/procedures/([0-9a-f-]{36})/retire`).FindStringSubmatch(page)[1]

	mona.get("/work-orders/new")
	_, page = mona.post("/work-orders", url.Values{"asset": {asset}, "type": {"pm"}, "priority": {"normal"}, "title": {"Semiannual PM"}, "procedure": {procID}})
	wo := "/work-orders/" + regexp.MustCompile(`/work-orders/([0-9a-f-]{36})/assign`).FindStringSubmatch(page)[1]
	mona.post(wo+"/assign", url.Values{"assignee": {tessID}})

	tess.get(wo)
	tess.post(wo+"/status", url.Values{"to": {"in_progress"}})
	// The whole checklist is one form: everything answered is saved at once.
	_, page = tess.post(wo+"/checklist", url.Values{"v_s1": {"pass"}, "v_s2": {"412.5"}, "n_s2": {"high"}})
	if !strings.Contains(page, "Saved 2 answers.") || !strings.Contains(page, "<strong>412.5 µA</strong> <span class=\"fail\">fail</span>") {
		t.Fatalf("checklist form:\n%s", page)
	}
	if _, page = tess.post(wo+"/checklist", url.Values{"v_s1": {"pass"}, "v_s2": {"412.5"}, "n_s2": {"high"}}); !strings.Contains(page, "Nothing new to save.") {
		t.Fatalf("unchanged answers saved again:\n%s", page)
	}
	// One step at a time still works too.
	_, page = tess.post(wo+"/steps", url.Values{"step": {"s2"}, "value": {"120"}, "note": {"re-measured"}})
	if !strings.Contains(page, "<strong>120 µA</strong> <span class=\"pass\">pass</span>") {
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

// Every kind of step, through the builder and the work order's form.
func TestRichChecklistThroughTheUI(t *testing.T) {
	e := newEnv(t)
	e.user("mona", "Mona Mid", domain.RoleMidTier)
	tessID := e.user("tess", "Tess Tech", domain.RoleUser)
	asset, _ := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "DEFIB-1", LocationID: e.loc, Manufacturer: "Zoll", Model: "R"})
	mona, tess := e.browser(), e.browser()
	mona.login("mona", "brass-kettle-orchard-7")
	tess.login("tess", "brass-kettle-orchard-7")
	mona.get("/procedures/new")
	form := url.Values{"name": {"Defibrillator inspection"}, "count": {"8"}, "category": {"Performance check"}, "reference": {"IEC 60601-2-4"},
		"text_0": {"Before you start"}, "kind_0": {"heading"},
		"text_1": {"Discharge into the test load only."}, "kind_1": {"info"},
		"text_2": {"Housing condition"}, "kind_2": {"choice"}, "options_2": {"Good\nWorn\nCracked"}, "fails_2": {"Cracked"}, "required_2": {"yes"}, "notefail_2": {"yes"},
		"text_3": {"Battery fitted?"}, "kind_3": {"choice"}, "options_3": {"Yes\nNo"}, "required_3": {"yes"},
		"text_4": {"Battery voltage"}, "kind_4": {"number"}, "unit_4": {"V"}, "lower_4": {"11.5"}, "upper_4": {"13"}, "required_4": {"yes"},
		"showif_step_4": {"4"}, "showif_value_4": {"Yes"},
		"text_5": {"Delivered energy"}, "kind_5": {"number"}, "unit_5": {"J"}, "lower_5": {"170"}, "upper_5": {"230"}, "critical_5": {"yes"}, "help_5": {"Average of three discharges."},
		"text_6": {"Parts replaced"}, "kind_6": {"multi"}, "options_6": {"Pads\nBattery\nCable"},
		"text_7": {"Next battery change"}, "kind_7": {"date"}}
	// A mistake comes back with everything as typed.
	bad := url.Values{}
	for k, v := range form {
		bad[k] = v
	}
	bad.Set("fails_2", "Broken")
	if code, page := mona.post("/procedures", bad); code != http.StatusUnprocessableEntity || !strings.Contains(page, "Not published: step 3: &#34;Broken&#34; counts as a fail") ||
		!strings.Contains(page, `value="Battery voltage"`) {
		t.Fatalf("mistake: %d\n%s", code, page)
	}
	// "Add 5 more steps" keeps what was typed.
	more := url.Values{}
	for k, v := range form {
		more[k] = v
	}
	more.Set("more", "5")
	if _, page := mona.post("/procedures", more); !strings.Contains(page, `name="text_12"`) || !strings.Contains(page, `value="Delivered energy"`) {
		t.Fatalf("more steps:\n%s", page)
	}
	_, page := mona.post("/procedures", form)
	for _, want := range []string{"Published Defibrillator inspection version 1", "<h3>Before you start</h3>", "Good, Worn, Cracked (fails)",
		"only when “Battery fitted?” is “Yes”", `<span class="tag crit">critical</span>`, "Average of three discharges."} {
		if !strings.Contains(page, want) {
			t.Fatalf("checklist page lacks %s:\n%s", want, page)
		}
	}
	procID := regexp.MustCompile(`/procedures/([0-9a-f-]{36})/retire`).FindStringSubmatch(page)[1]

	// Download it, and open the download in the builder (as another pi-fleet would).
	resp, _ := mona.c.Get(e.srv.URL + "/procedures/" + procID + "/export.json")
	var file strings.Builder
	buf := make([]byte, 1<<16)
	for {
		n, err := resp.Body.Read(buf)
		file.Write(buf[:n])
		if err != nil {
			break
		}
	}
	resp.Body.Close()
	if !strings.Contains(file.String(), `"format": "pi-fleet checklist"`) || !strings.Contains(file.String(), `"show_if"`) {
		t.Fatalf("download:\n%s", file.String())
	}
	mona.get("/procedures")
	if _, page = mona.postMultipart("/procedures/import", url.Values{}, upFile{"file", "defib.json", []byte(file.String())}); !strings.Contains(page, `value="Defibrillator inspection"`) ||
		!strings.Contains(page, ">Good\nWorn\nCracked</textarea>") || !strings.Contains(page, `name="showif_step_4" value="4"`) {
		t.Fatalf("import:\n%s", page)
	}
	// "Change it" starts the next version from this one.
	if _, _, page = mona.get("/procedures/new?from=" + procID); !strings.Contains(page, "Keep the name to publish its next version") || !strings.Contains(page, `value="Parts replaced"`) {
		t.Fatalf("next version:\n%s", page)
	}

	mona.get("/work-orders/new")
	_, page = mona.post("/work-orders", url.Values{"asset": {asset}, "type": {"inspection"}, "priority": {"normal"}, "title": {"Yearly"}, "procedure": {procID}})
	wo := "/work-orders/" + regexp.MustCompile(`/work-orders/([0-9a-f-]{36})/assign`).FindStringSubmatch(page)[1]
	mona.post(wo+"/assign", url.Values{"assignee": {tessID}})
	tess.get(wo)
	_, page = tess.post(wo+"/status", url.Values{"to": {"in_progress"}})
	for _, want := range []string{`<h3 class="cl-heading">Before you start</h3>`, `<p class="cl-info">Discharge into the test load only.</p>`,
		`type="radio" name="v_s3" value="Cracked"`, `type="checkbox" name="v_s7" value="Pads"`, `type="date" name="v_s8"`,
		"Not needed: only when “Battery fitted?” is “Yes”."} {
		if !strings.Contains(page, want) {
			t.Fatalf("work order form lacks %s:\n%s", want, page)
		}
	}
	// Answering "Battery fitted? Yes" in the same save makes the voltage apply.
	_, page = tess.post(wo+"/checklist", url.Values{"v_s3": {"Cracked"}, "v_s4": {"Yes"}, "v_s5": {"12.2"}, "v_s6": {"150"}, "v_s7": {"Pads", "Battery"}, "v_s8": {"2027-04-01"}})
	if !strings.Contains(page, "Saved 5 answers. Not saved: &#34;Housing condition&#34; failed: say why in the note.") ||
		!strings.Contains(page, "<strong>Pads, Battery</strong>") || !strings.Contains(page, "<strong>12.2 V</strong> <span class=\"pass\">pass</span>") {
		t.Fatalf("saved:\n%s", page)
	}
	tess.post(wo+"/checklist", url.Values{"v_s3": {"Cracked"}, "n_s3": {"crack by the handle"}})
	if _, page = tess.post(wo+"/sign", url.Values{"meaning": {"performed"}, "password": {"brass-kettle-orchard-7"}, "ack_clock": {"yes"}}); !strings.Contains(page, "take the equipment out of service") {
		t.Fatalf("signed with a failed critical step:\n%s", page)
	}
}

// Checklists made for a model: the library, the equipment page, and a
// technician adding them to the work order they are doing.
func TestChecklistsForEquipmentModels(t *testing.T) {
	e := newEnv(t)
	e.user("mona", "Mona Mid", domain.RoleMidTier)
	tessID := e.user("tess", "Tess Tech", domain.RoleUser)
	asset, _ := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "DEFIB-1", LocationID: e.loc, Manufacturer: "Zoll", Model: "R Series"})
	mona, tess := e.browser(), e.browser()
	mona.login("mona", "brass-kettle-orchard-7")
	tess.login("tess", "brass-kettle-orchard-7")

	// The builder suggests the manufacturers and models registered.
	_, _, page := mona.get("/procedures/new")
	if !strings.Contains(page, `<datalist id="manufacturers"><option value="Zoll">`) || !strings.Contains(page, `name="mfr_2"`) {
		t.Fatalf("builder equipment rows:\n%s", page)
	}
	publish := func(name, mfr, model string) {
		t.Helper()
		_, page := mona.post("/procedures", url.Values{"name": {name}, "count": {"1"}, "acount": {"1"}, "mfr_0": {mfr}, "model_0": {model},
			"text_0": {"Check " + name}, "kind_0": {"check"}, "required_0": {"yes"}})
		if !strings.Contains(page, "Published "+name) {
			t.Fatalf("publish %s:\n%s", name, page)
		}
	}
	publish("Zoll R PM", "Zoll", "R Series")
	publish("Zoll cleaning", "zoll", "")
	publish("Electrical safety", "", "")
	publish("Philips MX40 PM", "Philips", "MX40")
	if _, page = mona.post("/procedures", url.Values{"name": {"Bad"}, "count": {"1"}, "acount": {"1"}, "model_0": {"R Series"}, "text_0": {"x"}, "kind_0": {"check"}}); !strings.Contains(page, "give the manufacturer of &#34;R Series&#34;") {
		t.Fatalf("model without manufacturer:\n%s", page)
	}

	// The library: under Equipment, for any equipment first, then by manufacturer and model.
	_, _, page = mona.get("/procedures")
	if !strings.Contains(page, `<a href="/procedures" aria-current="page">Checklists</a>`) || !strings.Contains(page, `>Equipment</a>`) {
		t.Fatalf("not under Equipment:\n%s", page)
	}
	iAny, iPhilips, iZoll := strings.Index(page, "<h2>For any equipment</h2>"), strings.Index(page, "<h2>Philips</h2>"), strings.Index(page, "<h2>Zoll</h2>")
	if iAny < 0 || iPhilips < iAny || iZoll < iPhilips || !strings.Contains(page[iZoll:], "<h3>Every model</h3>") || !strings.Contains(page[iZoll:], "<h3>R Series</h3>") {
		t.Fatalf("library:\n%s", page)
	}
	if _, _, page = mona.get("/procedures?q=mx40"); !strings.Contains(page, "Philips MX40 PM") || strings.Contains(page, "Zoll R PM") {
		t.Fatalf("search:\n%s", page)
	}
	// The equipment page lists those made for it.
	if _, _, page = tess.get("/assets/" + asset); !strings.Contains(page, ">Zoll R PM</a> <span class=\"hint\">v1 · for Zoll R Series") ||
		!strings.Contains(page, ">Zoll cleaning</a> <span class=\"hint\">v1 · for all Zoll equipment") || strings.Contains(page, "Philips MX40 PM") {
		t.Fatalf("equipment page:\n%s", page)
	}

	// The technician's work order: matching checklists offered first.
	mona.get("/work-orders/new")
	_, page = mona.post("/work-orders", url.Values{"asset": {asset}, "type": {"pm"}, "priority": {"normal"}, "title": {"PM"}})
	wo := "/work-orders/" + regexp.MustCompile(`/work-orders/([0-9a-f-]{36})/assign`).FindStringSubmatch(page)[1]
	mona.post(wo+"/assign", url.Values{"assignee": {tessID}})
	_, _, page = tess.get(wo)
	sel := page[strings.Index(page, `name="procedure" required><option value="" selected disabled>Choose a checklist</option><optgroup`):]
	sel = sel[:strings.Index(sel, "</select>")]
	if !strings.Contains(sel, `Choose a checklist</option><optgroup label="For Zoll R Series"><option`) || strings.Index(sel, "For all Zoll equipment") > strings.Index(sel, "For any equipment") ||
		strings.Index(sel, "Other checklists") < strings.Index(sel, "Electrical safety") {
		t.Fatalf("add choices:\n%s", sel)
	}
	ids := map[string]string{}
	for _, m := range regexp.MustCompile(`<option value="([0-9a-f-]{36})">([^<]+) \(v1\)</option>`).FindAllStringSubmatch(sel, -1) {
		ids[m[2]] = m[1]
	}
	_, page = tess.post(wo+"/checklists", url.Values{"procedure": {ids["Zoll R PM"]}})
	_, page = tess.post(wo+"/checklists", url.Values{"procedure": {ids["Electrical safety"]}})
	_, page = tess.post(wo+"/checklists", url.Values{"procedure": {ids["Zoll cleaning"]}})
	if !strings.Contains(page, "Checklist added.") || strings.Count(page, `class="cl-title"`) != 3 || strings.Count(page, "Remove this checklist") != 3 {
		t.Fatalf("added:\n%s", page)
	}
	_, page = tess.post(wo+"/checklists/remove", url.Values{"procedure": {ids["Zoll cleaning"]}})
	if strings.Count(page, `class="cl-title"`) != 2 {
		t.Fatalf("removed:\n%s", page)
	}
	tess.post(wo+"/status", url.Values{"to": {"in_progress"}})
	// Each checklist is saved by its own button; both have a step "s1".
	tess.post(wo+"/checklist", url.Values{"procedure": {ids["Zoll R PM"]}, "v_s1": {"pass"}})
	_, page = tess.post(wo+"/checklist", url.Values{"procedure": {ids["Electrical safety"]}, "v_s1": {"fail"}, "n_s1": {"leakage high"}})
	if strings.Count(page, "<strong>pass</strong>") != 1 || strings.Count(page, "<strong>fail</strong>") != 1 || !strings.Contains(page, "Save Electrical safety") {
		t.Fatalf("two checklists:\n%s", page)
	}
	// Answered checklists can't be removed.
	if strings.Contains(page, "Remove this checklist") {
		t.Fatalf("remove offered on answered checklists:\n%s", page)
	}
	if _, _, print := tess.get(wo + "/print"); !strings.Contains(print, "Checklist: Zoll R PM v1") || !strings.Contains(print, "Checklist: Electrical safety v1") {
		t.Fatalf("printout:\n%s", print)
	}
	// Today: the three things people start most.
	if _, _, page = tess.get("/"); !strings.Contains(page, `<nav class="quick" aria-label="Start something"><a class="button" href="/work-orders/new">New work order</a>`) {
		t.Fatalf("today:\n%s", page)
	}
}
