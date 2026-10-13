package web

import (
	"bytes"
	"encoding/csv"
	"html"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"pi-fleet/internal/app"
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
	if _, _, page := mona.get("/procedures/new"); !strings.Contains(page, `name="text_0"`) || strings.Contains(page, `name="text_1"`) || !strings.Contains(page, ">Tick all that apply</option>") {
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
	tess.post(wo+"/steps", url.Values{"step": {"s2"}, "value": {"120"}, "note": {"re-measured"}})
	if _, _, page = tess.get(wo + "/checklists/" + procID); !strings.Contains(page, "<strong>120 µA</strong> <span class=\"pass\">pass</span>") {
		t.Fatalf("re-recorded reading:\n%s", page)
	}

	// Labour in different formats.
	if _, page = tess.post(wo+"/labor", url.Values{"time": {"lots"}, "date": {"2026-10-06"}}); !strings.Contains(page, "Not saved: enter time as") {
		t.Fatalf("bad time accepted:\n%s", page)
	}
	tess.post(wo+"/labor", url.Values{"time": {"1:30"}, "date": {"2026-10-06"}, "note": {"PM"}})
	_, page = tess.post(wo+"/labor", url.Values{"time": {"0.25h"}, "date": {"2026-10-06"}})
	if !strings.Contains(page, "Labour <strong>1:45</strong>") {
		t.Fatalf("labour total:\n%s", page)
	}

	_, page = tess.post(wo+"/sign", url.Values{"meaning": {"performed"}, "password": {"brass-kettle-orchard-7"}, "ack_clock": {"yes"}})
	if !strings.Contains(page, "Signed.") {
		t.Fatalf("sign:\n%s", page)
	}
	_, _, print := tess.get(wo + "/print")
	for _, want := range []string{"Checklist: Infusion pump PM v1", "120 µA pass", "Time (labour 1:45, travel 0:00)", "Battery runtime</td><td>not recorded"} {
		if !strings.Contains(print, want) {
			t.Errorf("printable record lacks %q", want)
		}
	}
	if code, _, _ := tess.get("/procedures"); code != http.StatusOK {
		t.Fatal("users can't view checklists")
	}
	// A user's checklist isn't published: it is sent for approval.
	if _, page := tess.post("/procedures", url.Values{"name": {"Tess's PM"}, "count": {"1"}, "text_0": {"Check it"}, "kind_0": {"check"}}); !strings.Contains(page, "Sent for approval.") {
		t.Fatalf("user's checklist:\n%s", page)
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
	if code, page := mona.post("/procedures", bad); code != http.StatusUnprocessableEntity || !strings.Contains(page, "Not saved: step 3: &#34;Broken&#34; counts as a fail") ||
		!strings.Contains(page, `value="Battery voltage"`) {
		t.Fatalf("mistake: %d\n%s", code, page)
	}
	// Adding or removing one step keeps what was typed, and keeps "only
	// ask when step N" pointing at the same step.
	edit := func(k, v string) string {
		f := url.Values{}
		for k, v := range form {
			f[k] = v
		}
		f.Set(k, v)
		code, page := mona.post("/procedures", f)
		if code != http.StatusOK || !strings.Contains(page, `value="Delivered energy"`) {
			t.Fatalf("%s %s: %d\n%s", k, v, code, page)
		}
		return page
	}
	if page := edit("add", "2"); !strings.Contains(page, `name="text_8"`) || strings.Contains(page, `name="text_9"`) ||
		!strings.Contains(page, `name="text_2" value=""`) || !strings.Contains(page, `name="showif_step_5" value="5"`) {
		t.Fatalf("step added after step 2:\n%s", page)
	}
	if page := edit("remove", "1"); strings.Contains(page, `name="text_7"`) || strings.Contains(page, "Before you start") ||
		!strings.Contains(page, `name="showif_step_3" value="3"`) {
		t.Fatalf("step 1 removed:\n%s", page)
	}
	if page := edit("remove", "4"); strings.Contains(page, "Battery fitted?") || !strings.Contains(page, `name="showif_step_3" value=""`) ||
		!strings.Contains(page, "Step 4 removed. Step 4 was asked only when it had a certain answer") {
		t.Fatalf("step 4 removed:\n%s", page)
	}
	if page := edit("stay", "1"); !strings.Contains(page, `name="text_7"`) || strings.Contains(page, `name="text_8"`) {
		t.Fatalf("Enter in a field:\n%s", page)
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
	if _, _, page = mona.get("/procedures/new?from=" + procID); !strings.Contains(page, "Amending the generic template: publishing makes its next version") || !strings.Contains(page, `value="Parts replaced"`) {
		t.Fatalf("next version:\n%s", page)
	}

	mona.get("/work-orders/new")
	_, page = mona.post("/work-orders", url.Values{"asset": {asset}, "type": {"inspection"}, "priority": {"normal"}, "title": {"Yearly"}, "procedure": {procID}})
	wo := "/work-orders/" + regexp.MustCompile(`/work-orders/([0-9a-f-]{36})/assign`).FindStringSubmatch(page)[1]
	mona.post(wo+"/assign", url.Values{"assignee": {tessID}})
	tess.get(wo)
	tess.post(wo+"/status", url.Values{"to": {"in_progress"}})
	_, _, page = tess.get(wo + "/checklists/" + procID)
	for _, want := range []string{`<h2 class="cl-heading">Before you start</h2>`, `<p class="cl-info">Discharge into the test load only.</p>`,
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
	iTpl, iAny, iPhilips, iZoll := strings.Index(page, "<h2>Generic templates</h2>"), strings.Index(page, "<h3>For any equipment</h3>"), strings.Index(page, "<h3>Philips</h3>"), strings.Index(page, "<h3>Zoll</h3>")
	if iTpl < 0 || iAny < iTpl || iPhilips < iAny || iZoll < iPhilips || !strings.Contains(page[iZoll:], "<h4>Every model</h4>") || !strings.Contains(page[iZoll:], "<h4>R Series</h4>") ||
		!strings.Contains(page, "<h2>Custom checklists</h2>") {
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
	for _, m := range regexp.MustCompile(`<option value="([0-9a-f-]{36})">([^<]+) \(v1, generic template\)</option>`).FindAllStringSubmatch(sel, -1) {
		ids[m[2]] = m[1]
	}
	_, page = tess.post(wo+"/checklists", url.Values{"procedure": {ids["Zoll R PM"]}})
	_, page = tess.post(wo+"/checklists", url.Values{"procedure": {ids["Electrical safety"]}})
	_, page = tess.post(wo+"/checklists", url.Values{"procedure": {ids["Zoll cleaning"]}})
	if !strings.Contains(page, "Checklist added.") || strings.Count(page, `class="cl-card"`) != 3 || strings.Count(page, "Remove this checklist") != 3 {
		t.Fatalf("added:\n%s", page)
	}
	_, page = tess.post(wo+"/checklists/remove", url.Values{"procedure": {ids["Zoll cleaning"]}})
	if strings.Count(page, `class="cl-card"`) != 2 {
		t.Fatalf("removed:\n%s", page)
	}
	tess.post(wo+"/status", url.Values{"to": {"in_progress"}})
	// Each checklist is saved by its own button; both have a step "s1".
	if _, page = tess.post(wo+"/checklist", url.Values{"procedure": {ids["Zoll R PM"]}, "v_s1": {"pass"}}); !strings.Contains(page, "<strong>pass</strong>") ||
		!strings.Contains(page, "Every step is answered: go back to the work order to finish it.") {
		t.Fatalf("first checklist:\n%s", page)
	}
	if _, page = tess.post(wo+"/checklist", url.Values{"procedure": {ids["Electrical safety"]}, "v_s1": {"fail"}, "n_s1": {"leakage high"}}); !strings.Contains(page, "<strong>fail</strong>") ||
		strings.Contains(page, "<strong>pass</strong>") {
		t.Fatalf("two checklists kept apart:\n%s", page)
	}
	// Answered checklists can't be removed.
	if _, _, page = tess.get(wo); strings.Contains(page, "Remove this checklist") {
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

// The checklist page: followed step by step during the visit, with photos.
func TestChecklistPageDuringAVisit(t *testing.T) {
	e := newEnv(t)
	tessID := e.user("tess", "Tess Tech", domain.RoleUser)
	asset, _ := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "XRAY-1", LocationID: e.loc, Manufacturer: "GE", Model: "AMX 4+"})
	proc, _, err := e.app.PublishChecklist(e.ctx, e.super, domain.ProcedurePublished{Name: "Portable X-ray PM", Steps: []domain.Step{
		{ID: "plate", Text: "Photo of the dataplate", Kind: domain.StepPhoto, Required: true},
		{ID: "kv", Text: "kVp accuracy at 80 kV", Kind: domain.StepNumber, Unit: "kV", Lower: "76", Upper: "84", Required: true},
		{ID: "stop", Text: "Emergency stop works", Kind: domain.StepCheck, Required: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	wo, _, _ := e.app.OpenWorkOrder(e.ctx, e.super, app.NewWorkOrder{Type: "pm", AssetID: asset, Priority: "normal", Title: "PM", ProcedureID: proc})
	e.app.AssignWorkOrder(e.ctx, e.super, wo, tessID)
	tess := e.browser()
	tess.login("tess", "brass-kettle-orchard-7")
	tess.post("/work-orders/"+wo+"/status", url.Values{"to": {"in_progress"}})

	// The work order shows a card that opens the checklist page.
	_, _, page := tess.get("/work-orders/" + wo)
	if !strings.Contains(page, `<a class="cl-open" href="/work-orders/`+wo+`/checklists/`+proc+`">`) || !strings.Contains(page, `<span class="button">Start</span>`) {
		t.Fatalf("card:\n%s", page)
	}
	_, _, page = tess.get("/work-orders/" + wo + "/checklists/" + proc)
	for _, want := range []string{`<a class="back" href="/work-orders/` + wo + `#checklist">Back to work order NYC-WO-`, `<progress value="0" max="3">`,
		`enctype="multipart/form-data"`, `type="file" name="p_plate" accept="image/jpeg,image/png,image/webp" multiple`, "XRAY-1</strong> · GE AMX 4&#43;",
		`<button>Save and continue</button>`, `id="step-kv"`} {
		if !strings.Contains(page, want) {
			t.Fatalf("checklist page lacks %s:\n%s", want, page)
		}
	}
	// A reading, then Save and continue: back at the next step to answer.
	code, loc := postNoFollow(t, tess, "/work-orders/"+wo+"/checklist", url.Values{"procedure": {proc}, "v_kv": {"81.2"}})
	if code != http.StatusSeeOther || loc != "/work-orders/"+wo+"/checklists/"+proc+"#step-plate" {
		t.Fatalf("after saving: %d %s", code, loc)
	}
	// An iPhone photo (taken sideways), and an iPhone HEIC file.
	heic := append([]byte{0, 0, 0, 24}, []byte("ftypheic\x00\x00\x00\x00mif1heic")...)
	_, page = tess.postMultipart("/work-orders/"+wo+"/checklist", url.Values{"procedure": {proc}, "v_stop": {"pass"}},
		upFile{"p_plate", "IMG_4012.JPG", photoWithGPS()}, upFile{"p_stop", "IMG_4013.HEIC", heic})
	if !strings.Contains(page, "Saved 1 answer and added 1 photo.") || !strings.Contains(page, "Most Compatible") ||
		!strings.Contains(page, `<progress value="3" max="3">`) || !strings.Contains(page, "<strong>1 photo</strong>") || !strings.Contains(page, `?view=1" alt="Photo: IMG_4012.JPG"`) {
		t.Fatalf("photos:\n%s", page)
	}
	// Save and go back to the work order.
	if code, loc := postNoFollow(t, tess, "/work-orders/"+wo+"/checklist", url.Values{"procedure": {proc}, "after": {"back"}}); loc != "/work-orders/"+wo+"#checklist" {
		t.Fatalf("back: %d %s", code, loc)
	}
}

// postNoFollow posts a form and returns the redirect, without following it.
func postNoFollow(t *testing.T, b *browser, path string, form url.Values) (int, string) {
	t.Helper()
	form.Set("csrf", b.csrf)
	req, _ := http.NewRequest("POST", b.e.srv.URL+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := b.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Location")
}

// Anyone builds checklists; a user's wait for a mid-tier or super user.
func TestChecklistApprovalThroughTheUI(t *testing.T) {
	e := newEnv(t)
	e.user("mona", "Mona Mid", domain.RoleMidTier)
	e.user("tess", "Tess Tech", domain.RoleUser)
	mona, tess := e.browser(), e.browser()
	mona.login("mona", "brass-kettle-orchard-7")
	tess.login("tess", "brass-kettle-orchard-7")
	if _, _, page := tess.get("/procedures/new"); !strings.Contains(page, "<button>Send for approval</button>") || !strings.Contains(page, `<a class="back" href="/procedures">Back to checklists</a>`) {
		t.Fatalf("user's builder:\n%s", page)
	}
	_, page := tess.post("/procedures", url.Values{"name": {"Portable X-ray PM"}, "count": {"1"}, "acount": {"1"}, "mfr_0": {"GE"}, "model_0": {"AMX 4+"},
		"text_0": {"Collimator light works"}, "kind_0": {"check"}, "required_0": {"yes"}})
	if !strings.Contains(page, "Sent for approval.") || !strings.Contains(page, "Waiting for approval by a mid-tier or super user") || strings.Contains(page, "Approve and publish") {
		t.Fatalf("sent:\n%s", page)
	}
	if _, _, page = mona.get("/"); !strings.Contains(page, "1 checklist waiting for your approval") {
		t.Fatalf("dashboard:\n%s", page)
	}
	_, _, page = mona.get("/procedures")
	id := regexp.MustCompile(`<a href="/checklist-proposals/([0-9a-f-]{36})">Portable X-ray PM</a> <span class="hint">by Tess Tech`).FindStringSubmatch(page)
	if id == nil {
		t.Fatalf("library waiting list:\n%s", page)
	}
	if _, _, page = mona.get("/checklist-proposals/" + id[1]); !strings.Contains(page, "Approve and publish") || !strings.Contains(page, "GE AMX 4&#43;") {
		t.Fatalf("proposal page:\n%s", page)
	}
	if _, page = mona.post("/checklist-proposals/"+id[1]+"/review", url.Values{"decision": {"approve"}}); !strings.Contains(page, "Approved and published.") || !strings.Contains(page, "Portable X-ray PM v1") {
		t.Fatalf("approved:\n%s", page)
	}
	if _, _, page = tess.get("/procedures"); !strings.Contains(page, "Your checklists, decided") || !strings.Contains(page, "approved") {
		t.Fatalf("tess's library:\n%s", page)
	}
	if code, _ := tess.post("/checklist-proposals/"+id[1]+"/review", url.Values{"decision": {"approve"}}); code != http.StatusForbidden {
		t.Fatalf("a user reviewed: %d", code)
	}
}

// The sample portable X-ray checklist opens in the builder and publishes
// as it is.
func TestSamplePortableXRayChecklist(t *testing.T) {
	e := newEnv(t)
	file, err := os.ReadFile("../../deploy/sample-checklists/portable-x-ray.checklist.json")
	if err != nil {
		t.Fatal(err)
	}
	admin := e.browser()
	admin.login("admin", "tumbleweed-gasket-42")
	admin.get("/procedures")
	_, page := admin.postMultipart("/procedures/import", url.Values{}, upFile{"file", "portable-x-ray.checklist.json", file})
	if !strings.Contains(page, `value="Portable X-ray: preventive maintenance and quality control"`) {
		t.Fatalf("import:\n%s", page)
	}
	// Submit the builder exactly as it came back.
	form := url.Values{}
	for _, m := range regexp.MustCompile(`<input[^>]*name="([^"]+)"[^>]*value="([^"]*)"`).FindAllStringSubmatch(page, -1) {
		form.Set(m[1], html.UnescapeString(m[2]))
	}
	for _, m := range regexp.MustCompile(`(?s)<textarea name="([^"]+)"[^>]*>(.*?)</textarea>`).FindAllStringSubmatch(page, -1) {
		form.Set(m[1], html.UnescapeString(m[2]))
	}
	for _, m := range regexp.MustCompile(`(?s)<select name="([^"]+)">(.*?)</select>`).FindAllStringSubmatch(page, -1) {
		if sel := regexp.MustCompile(`<option(?: value="([^"]*)")? selected>([^<]*)</option>`).FindStringSubmatch(m[2]); sel != nil {
			v := sel[1]
			if v == "" {
				v = sel[2]
			}
			form.Set(m[1], html.UnescapeString(v))
		}
	}
	form.Del("csrf")
	if _, page = admin.post("/procedures", form); !strings.Contains(page, "Published Portable X-ray: preventive maintenance and quality control version 1") {
		t.Fatalf("publish:\n%s", page)
	}
	for _, want := range []string{"Half-value layer at 80 kVp", "only when “What powers the drive and generator?” is “Battery”", "Fail: removed from service (fails)"} {
		if !strings.Contains(html.UnescapeString(page), want) {
			t.Errorf("published checklist lacks %q", want)
		}
	}
}

// From the library: fill a checklist in on a chosen piece of equipment,
// with measured values, and download it to print, blank and filled in.
func TestFillAChecklistFromTheLibraryAndPrintIt(t *testing.T) {
	e := newEnv(t)
	e.user("tess", "Tess Tech", domain.RoleUser)
	asset, _ := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "XRAY-1", LocationID: e.loc, Manufacturer: "GE", Model: "AMX 4+", Serial: "S-451"})
	e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "PUMP-1", LocationID: e.loc, Manufacturer: "Baxter", Model: "Sigma"})
	proc, _, err := e.app.PublishChecklist(e.ctx, e.super, domain.ProcedurePublished{Name: "Portable X-ray PM", Reference: "21 CFR 1020.31",
		AppliesTo: []domain.EquipmentMatch{{Manufacturer: "GE", Model: "AMX 4+"}}, Steps: []domain.Step{
			{ID: "h1", Text: "Mechanical", Kind: domain.StepHeading},
			{ID: "cord", Text: "Exposure switch cord at least 1.8 m", Kind: domain.StepCheck, Unit: "m", Required: true},
			{ID: "kv80", Text: "kVp at 80 kV", Kind: domain.StepNumber, Unit: "kV", Lower: "76", Upper: "84", Required: true},
			{ID: "cond", Text: "Housing", Kind: domain.StepChoice, Options: []domain.StepOption{{Label: "Good"}, {Label: "Cracked", Fails: true}}},
		}})
	if err != nil {
		t.Fatal(err)
	}
	tess := e.browser()
	tess.login("tess", "brass-kettle-orchard-7")
	_, _, page := tess.get("/procedures/" + proc)
	sel := page[strings.Index(page, `name="asset" required>`):]
	if !strings.Contains(sel[:strings.Index(sel, "Other equipment")], ">XRAY-1 · GE AMX 4&#43;") || !strings.Contains(page, `href="/procedures/`+proc+`/checklist.csv">Download to print (spreadsheet)`) {
		t.Fatalf("checklist page:\n%s", page)
	}
	// The blank spreadsheet: lines for the visit, and a row per step.
	resp, _ := tess.c.Get(e.srv.URL + "/procedures/" + proc + "/checklist.csv")
	var blank bytes.Buffer
	blank.ReadFrom(resp.Body)
	resp.Body.Close()
	cr := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(blank.Bytes(), []byte("\xef\xbb\xbf"))))
	cr.FieldsPerRecord = -1 // the lines about the visit are shorter than the step rows
	rows, err := cr.ReadAll()
	if err != nil || resp.Header.Get("Content-Type") != "text/csv; charset=utf-8" {
		t.Fatalf("blank CSV: %v %s", err, resp.Header.Get("Content-Type"))
	}
	text := blank.String()
	for _, want := range []string{"Portable X-ray PM (version 1)", "Serial number,", "Signature,", "No.,Section,Step,How to do it,Kind of answer,Passes if,Required,Measured value / answer,Result,Note,Initials",
		"1,Mechanical,Exposure switch cord at least 1.8 m,,\"Pass / fail, measured in m\",Pass,yes,,,,", "2,Mechanical,kVp at 80 kV,,Reading (a number) (kV),76 to 84 kV,yes,,,,", "One of: Good"} {
		if !strings.Contains(text, want) {
			t.Errorf("blank CSV lacks %q:\n%s", want, text)
		}
	}
	_ = rows

	// Start filling it in on the X-ray unit: a work order is opened, taken and started.
	_, page = tess.post("/procedures/"+proc+"/fill", url.Values{"asset": {asset}})
	if !strings.Contains(page, "Started: a work order was opened for this checklist") || !strings.Contains(page, "<button>Save and continue</button>") ||
		!strings.Contains(page, `name="m_cord"`) || !strings.Contains(page, "Measured value in m (if one was measured)") {
		t.Fatalf("started:\n%s", page)
	}
	wo := regexp.MustCompile(`/work-orders/([0-9a-f-]{36})/checklist"`).FindStringSubmatch(page)[1]
	_, page = tess.post("/work-orders/"+wo+"/checklist", url.Values{"procedure": {proc}, "v_cord": {"pass"}, "m_cord": {"2.1"}, "v_kv80": {"81.3"}, "n_kv80": {"Piranha 657"}})
	if !strings.Contains(page, "<strong>pass</strong> · measured <strong>2.1 m</strong>") {
		t.Fatalf("measured value:\n%s", page)
	}
	resp, _ = tess.c.Get(e.srv.URL + "/work-orders/" + wo + "/checklists/" + proc + "/checklist.csv")
	var filled bytes.Buffer
	filled.ReadFrom(resp.Body)
	resp.Body.Close()
	for _, want := range []string{"Equipment (asset tag),XRAY-1", "Serial number,S-451", ",2.1,pass,,TT", ",81.3,pass,Piranha 657,TT"} {
		if !strings.Contains(filled.String(), want) {
			t.Errorf("filled CSV lacks %q:\n%s", want, filled.String())
		}
	}
}

// Generic templates: configure a custom checklist from one, or suggest an
// amendment to it.
func TestCustomChecklistFromAGenericTemplate(t *testing.T) {
	e := newEnv(t)
	e.user("mona", "Mona Mid", domain.RoleMidTier)
	e.user("tess", "Tess Tech", domain.RoleUser)
	tpl, _, err := e.app.PublishChecklist(e.ctx, e.super, domain.ProcedurePublished{Name: "Portable X-ray PM",
		Steps: []domain.Step{{ID: "s1", Text: "Collimator light works", Kind: domain.StepCheck, Required: true}}})
	if err != nil {
		t.Fatal(err)
	}
	tess := e.browser()
	tess.login("tess", "brass-kettle-orchard-7")
	_, _, page := tess.get("/procedures/" + tpl)
	if !strings.Contains(page, "<dt>Kind</dt><dd>Generic template</dd>") || !strings.Contains(page, `href="/procedures/new?from=`+tpl+`&amp;mode=custom">Configure custom checklist</a>`) ||
		!strings.Contains(page, ">Suggest amendment to Generic Template</a>") {
		t.Fatalf("template page:\n%s", page)
	}
	// Configure a custom checklist: its own name, the template untouched.
	_, _, page = tess.get("/procedures/new?from=" + tpl + "&mode=custom")
	if !strings.Contains(page, "Configuring a custom checklist from the generic template <strong>Portable X-ray PM</strong>") || !strings.Contains(page, `value="Portable X-ray PM (custom)"`) {
		t.Fatalf("custom builder:\n%s", page)
	}
	_, page = tess.post("/procedures", url.Values{"mode": {"custom"}, "custom": {"yes"}, "template": {tpl}, "name": {"Portable X-ray PM: ICU"}, "count": {"2"},
		"text_0": {"Collimator light works"}, "kind_0": {"check"}, "required_0": {"yes"}, "text_1": {"ICU bay clearance checked"}, "kind_1": {"check"}})
	if !strings.Contains(page, "Sent for approval.") || !strings.Contains(page, "Custom checklist, configured from <a href=\"/procedures/"+tpl+"\">a generic template</a>") {
		t.Fatalf("sent:\n%s", page)
	}
	// Suggest an amendment to the template.
	if _, _, page = tess.get("/procedures/new?from=" + tpl); !strings.Contains(page, "your amendment is sent for approval") || !strings.Contains(page, "<button>Send amendment for approval</button>") {
		t.Fatalf("amend builder:\n%s", page)
	}
	_, page = tess.post("/procedures", url.Values{"mode": {"amend"}, "custom": {"no"}, "from": {tpl}, "name": {"Portable X-ray PM"}, "count": {"1"},
		"text_0": {"Collimator light works and is bright"}, "kind_0": {"check"}, "required_0": {"yes"}})
	if !strings.Contains(page, "Amendment to a generic template") {
		t.Fatalf("amendment:\n%s", page)
	}
	// Approved, the custom checklist is listed under Custom checklists.
	mona := e.browser()
	mona.login("mona", "brass-kettle-orchard-7")
	_, _, page = mona.get("/procedures")
	for _, m := range regexp.MustCompile(`/checklist-proposals/([0-9a-f-]{36})`).FindAllStringSubmatch(page, -1) {
		mona.post("/checklist-proposals/"+m[1]+"/review", url.Values{"decision": {"approve"}})
	}
	_, _, page = mona.get("/procedures")
	iCustom := strings.Index(page, "<h2>Custom checklists</h2>")
	if iCustom < 0 || !strings.Contains(page[iCustom:], "Portable X-ray PM: ICU") || strings.Contains(page[:iCustom], "Portable X-ray PM: ICU") ||
		!strings.Contains(page[:iCustom], "Portable X-ray PM</a> <span class=\"hint\">v2") {
		t.Fatalf("library after approval:\n%s", page)
	}
}

func TestEveryStepKindHasALabel(t *testing.T) {
	for _, k := range domain.StepKinds {
		if kindLabels[k] == "" {
			t.Errorf("step kind %q has no label in the builder", k)
		}
	}
}
