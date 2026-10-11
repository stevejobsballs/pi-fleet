package web

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"pi-fleet/internal/domain"
)

// The catalogue through the screens: a mid-tier user adds a model from
// the equipment not yet catalogued; a technician registers one by
// choosing it and its schedules are set up; older equipment of the model
// gets them from its page or the model's.
func TestModelCatalogueThroughTheUI(t *testing.T) {
	e := newEnv(t)
	e.user("mona", "Mona Mid", domain.RoleMidTier)
	e.user("tess", "Tess Tech", domain.RoleUser)
	old, _ := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "PUMP-1", LocationID: e.loc, Manufacturer: "Baxter", Model: "Sigma Spectrum"})
	mona, tess := e.browser(), e.browser()
	mona.login("mona", "brass-kettle-orchard-7")
	tess.login("tess", "brass-kettle-orchard-7")

	_, _, page := mona.get("/models")
	add := `/models/new?manufacturer=Baxter&amp;model=Sigma&#43;Spectrum`
	if !strings.Contains(page, "Equipment not in the catalogue yet") || !strings.Contains(page, add) {
		t.Fatalf("uncatalogued:\n%s", page)
	}
	if _, _, page = mona.get("/models/new?manufacturer=Baxter&model=Sigma+Spectrum"); !strings.Contains(page, `value="Sigma Spectrum"`) {
		t.Fatalf("form not filled:\n%s", page)
	}
	_, page = mona.post("/models", url.Values{"manufacturer": {"Baxter"}, "model": {"Sigma Spectrum"}, "kind": {"Infusion pump"}, "risk_class": {"high"},
		"rows": {"1"}, "sched_0_title": {"Yearly PM"}, "sched_0_type": {"pm"}, "sched_0_every": {"365"}, "sched_0_grace": {"14"}})
	if !strings.Contains(page, "Added Baxter Sigma Spectrum to the catalogue.") || !strings.Contains(page, "1 piece of this model's equipment is missing") {
		t.Fatalf("added:\n%s", page)
	}
	model := regexp.MustCompile(`action="/models/([0-9a-f-]{36})/apply"`).FindStringSubmatch(page)[1]
	if code, _, _ := tess.get("/models/new"); code != http.StatusForbidden {
		t.Fatalf("a user could add models: %d", code)
	}

	// Registering by choosing the model: details filled, schedules set up.
	if _, _, page = tess.get("/assets/new?modelq=sigma"); !strings.Contains(page, `href="/assets/new?model=`+model+`"`) {
		t.Fatalf("model search:\n%s", page)
	}
	if _, _, page = tess.get("/assets/new?model=" + model); !strings.Contains(page, `name="manufacturer" value="Baxter"`) || !strings.Contains(page, "Yearly PM: pm every 365 days") {
		t.Fatalf("chosen model:\n%s", page)
	}
	reg := url.Values{"tag": {"PUMP-2"}, "location": {e.loc}, "manufacturer": {"Baxter"}, "model": {"Sigma Spectrum"}, "model_id": {model},
		"model_schedules_offered": {"yes"}, "model_schedules": {"yes"}}
	if _, page = tess.post("/assets", reg); !strings.Contains(page, "1 schedule from the catalogue set up.") || !strings.Contains(page, "Next scheduled: <strong>Yearly PM</strong>") {
		t.Fatalf("registered:\n%s", page)
	}
	// Unticked: none.
	reg.Set("tag", "PUMP-3")
	reg.Del("model_schedules")
	if _, page = tess.post("/assets", reg); strings.Contains(page, "from the catalogue set up") || !strings.Contains(page, "should also have: Yearly PM") {
		t.Fatalf("unticked:\n%s", page)
	}
	// The older pump gets it from its own page.
	if _, page = tess.post("/assets/"+old+"/model-schedules", nil); !strings.Contains(page, "Set up 1 schedule from the catalogue.") {
		t.Fatalf("from its page:\n%s", page)
	}
	// And the rest from the model's page.
	if _, page = mona.post("/models/"+model+"/apply", nil); !strings.Contains(page, "Set up 1 schedule.") || strings.Contains(page, "missing some of these") {
		t.Fatalf("from the model:\n%s", page)
	}
	if _, _, page = tess.get("/search?q=infusion"); !strings.Contains(page, `href="/models/`+model+`"`) {
		t.Fatalf("model not found by search:\n%s", page)
	}
}
