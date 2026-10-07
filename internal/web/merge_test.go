package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"pi-fleet/internal/app"
	"pi-fleet/internal/domain"
	"pi-fleet/internal/export"
)

func TestMergeDuplicatesThroughTheUI(t *testing.T) {
	e := newEnv(t)
	e.user("mona", "Mona Mid", domain.RoleMidTier)
	e.user("tess", "Tess Tech", domain.RoleUser)
	tess, mona := e.browser(), e.browser()
	tess.login("tess", "brass-kettle-orchard-7")
	mona.login("mona", "brass-kettle-orchard-7")

	register := func(tag string) string {
		tess.get("/assets/new")
		_, page := tess.post("/assets", url.Values{"tag": {tag}, "location": {e.loc}, "manufacturer": {"Baxter"}, "model": {"Sigma"}, "master_id": {" M-77 "}})
		m := regexp.MustCompile(`/assets/([0-9a-f-]{36})/audit`).FindStringSubmatch(page)
		if m == nil || !strings.Contains(page, "Equipment registered") {
			t.Fatalf("register %s:\n%s", tag, page)
		}
		return m[1]
	}
	keep := register("NYC-0100")
	dup := register("PROV-0100")
	wo, _, err := e.app.OpenWorkOrder(e.ctx, e.super, app.NewWorkOrder{Type: "corrective", AssetID: dup, Priority: "normal", Title: "Alarm"})
	if err != nil {
		t.Fatal(err)
	}

	// Both records warn; only a mid-tier user gets the merge link.
	if _, _, page := tess.get("/assets/" + keep); !strings.Contains(page, "Other records have the same MasterID") || !strings.Contains(page, "A mid-tier user can merge them") {
		t.Fatalf("user's view:\n%s", page)
	}
	if code, _, _ := tess.get("/assets/merge?master_id=M-77"); code != http.StatusForbidden {
		t.Fatalf("user opened the merge page: %d", code)
	}
	_, _, page := mona.get("/review")
	if !strings.Contains(page, "Duplicate equipment") || !strings.Contains(page, "/assets/merge?master_id=M-77") {
		t.Fatalf("review queue:\n%s", page)
	}
	_, _, page = mona.get("/assets/merge?master_id=M-77")
	if !strings.Contains(page, "NYC-0100") || !strings.Contains(page, "PROV-0100") {
		t.Fatalf("merge page:\n%s", page)
	}
	if !strings.Contains(page, "If you choose No, this can't be changed later") || !strings.Contains(page, `name="integrate" value="yes" required`) || !strings.Contains(page, "to the kept record's timeline?") {
		t.Fatalf("merge page doesn't ask about the service history:\n%s", page)
	}
	if _, page = mona.post("/assets/merge", url.Values{"master_id": {"M-77"}, "keep": {keep}, "reason": {""}, "integrate": {"yes"}}); !strings.Contains(page, "Not saved") {
		t.Fatalf("merge without a reason:\n%s", page)
	}
	if _, page = mona.post("/assets/merge", url.Values{"master_id": {"M-77"}, "keep": {keep}, "reason": {"twice"}}); !strings.Contains(page, "Not saved: answer whether") {
		t.Fatalf("merge without answering the question:\n%s", page)
	}
	// A later work order on the kept record: the timeline is in date order.
	e.now = e.now.Add(10 * time.Minute)
	later, _, err := e.app.OpenWorkOrder(e.ctx, e.super, app.NewWorkOrder{Type: "corrective", AssetID: keep, Priority: "normal", Title: "Later"})
	if err != nil {
		t.Fatal(err)
	}
	_, page = mona.post("/assets/merge", url.Values{"master_id": {"M-77"}, "keep": {keep}, "reason": {"PROV-0100 registered twice"}, "integrate": {"yes"}})
	if !strings.Contains(page, "Merged 1 record into this one. Their service history is now part of this record&#39;s timeline") || !strings.Contains(page, "/work-orders/"+wo) {
		t.Fatalf("kept record after merge:\n%s", page)
	}
	if i, j := strings.Index(page, "/work-orders/"+later), strings.Index(page, "/work-orders/"+wo); i < 0 || j < i {
		t.Fatalf("timeline not newest first: later at %d, merged record's at %d", i, j)
	}
	if strings.Contains(page, "Other records have the same MasterID") {
		t.Fatal("still warned about duplicates after merging")
	}

	// The merged record is read-only and points at the kept one.
	_, _, page = tess.get("/assets/" + dup)
	if !strings.Contains(page, "This record was merged into") || strings.Contains(page, `action="/assets/`+dup+`/status"`) {
		t.Fatalf("merged record:\n%s", page)
	}
	// Hidden from the equipment list, found by searching.
	if _, _, page = tess.get("/assets"); strings.Contains(page, "PROV-0100") {
		t.Fatal("merged record listed")
	}
	if _, _, page = tess.get("/assets?q=M-77"); !strings.Contains(page, "merged into NYC-0100") {
		t.Fatalf("search:\n%s", page)
	}
	// The kept record's signed export has both histories and verifies.
	resp, err := tess.c.Get(e.srv.URL + "/assets/" + keep + "/export.json")
	if err != nil {
		t.Fatal(err)
	}
	var bundle export.Bundle
	json.NewDecoder(resp.Body).Decode(&bundle)
	resp.Body.Close()
	types := map[string]bool{}
	for _, ev := range bundle.Events {
		types[ev.Type+" "+ev.EntityID] = true
	}
	if !types["asset.registered "+dup] || !types["workorder.opened "+wo] || !types["asset.merged "+keep] {
		t.Fatalf("export events: %v", types)
	}
	if rep := export.Verify(bundle); len(rep.Problems) != 0 || rep.Verified != len(bundle.Events) {
		t.Fatalf("export: %+v", rep)
	}
	// The merged record's own export includes the merge.
	resp, err = tess.c.Get(e.srv.URL + "/assets/" + dup + "/export.json")
	if err != nil {
		t.Fatal(err)
	}
	bundle = export.Bundle{}
	json.NewDecoder(resp.Body).Decode(&bundle)
	resp.Body.Close()
	if last := bundle.Events[len(bundle.Events)-1]; last.Type != "asset.merged" {
		t.Fatalf("merged record's export ends with %s", last.Type)
	}

	// The kept record's audit trail has the merged record's history.
	if _, _, page = tess.get("/assets/" + keep + "/audit"); !strings.Contains(page, "PROV-0100 registered twice") || !strings.Contains(page, "PROV-0100") {
		t.Fatalf("audit trail:\n%s", page)
	}
}

func TestMergeKeepingHistorySeparateThroughTheUI(t *testing.T) {
	e := newEnv(t)
	e.user("mona", "Mona Mid", domain.RoleMidTier)
	reg := func(tag string) string {
		id, err := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: tag, LocationID: e.loc, Manufacturer: "Baxter", Model: "Sigma", MasterID: "M-9"})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	keep, old := reg("NYC-9"), reg("OLD-9")
	wo, _, err := e.app.OpenWorkOrder(e.ctx, e.super, app.NewWorkOrder{Type: "corrective", AssetID: old, Priority: "normal", Title: "Old repair"})
	if err != nil {
		t.Fatal(err)
	}
	b := e.browser()
	b.login("mona", "brass-kettle-orchard-7")
	b.get("/assets/merge?master_id=M-9")
	_, page := b.post("/assets/merge", url.Values{"master_id": {"M-9"}, "keep": {keep}, "reason": {"old import"}, "integrate": {"no"}})
	if !strings.Contains(page, "stays with them") || !strings.Contains(page, "with their service history kept separate") || strings.Contains(page, "/work-orders/"+wo) {
		t.Fatalf("kept record:\n%s", page)
	}
	if _, _, page = b.get("/assets/" + old); !strings.Contains(page, "kept here, separate") || !strings.Contains(page, "/work-orders/"+wo) {
		t.Fatalf("merged record:\n%s", page)
	}
}
