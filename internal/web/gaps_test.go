package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"pi-fleet/internal/app"
	"pi-fleet/internal/domain"
	"pi-fleet/internal/export"
)

func TestConcurrentAssetEditsMerge(t *testing.T) {
	e := newEnv(t)
	e.user("tess", "Tess Tech", domain.RoleUser)
	e.user("tom", "Tom Tech", domain.RoleUser)
	asset, _ := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "A1", LocationID: e.loc, Manufacturer: "Fluke", Model: "ESA615", Serial: "S1"})
	tess, tom := e.browser(), e.browser()
	tess.login("tess", "brass-kettle-orchard-7")
	tom.login("tom", "brass-kettle-orchard-7")
	tess.get("/assets/" + asset)
	tom.get("/assets/" + asset)

	form := func(serial, risk string) url.Values {
		return url.Values{"version": {"1"}, "orig_manufacturer": {"Fluke"}, "orig_model": {"ESA615"}, "orig_serial": {"S1"},
			"orig_risk_class": {""}, "manufacturer": {"Fluke"}, "model": {"ESA615"}, "serial": {serial}, "risk_class": {risk}}
	}
	// Both saw version 1: Tess fixes the serial, Tom sets the risk class.
	if _, page := tess.post("/assets/"+asset+"/edit", form("S1-CORRECTED", "")); !strings.Contains(page, "Details saved") {
		t.Fatalf("tess:\n%s", page)
	}
	if _, page := tom.post("/assets/"+asset+"/edit", form("S1", "high")); !strings.Contains(page, "Details saved") {
		t.Fatalf("tom's edit to another field should merge:\n%s", page)
	}
	a, _ := domain.GetAsset(e.ctx, e.app.Store.DB(), asset)
	if a.Serial != "S1-CORRECTED" || a.RiskClass != "high" {
		t.Fatalf("asset = %+v", a)
	}
	// Tom, still on version 1, also changes the serial: a conflict.
	if _, page := tom.post("/assets/"+asset+"/edit", form("S1-TOM", "")); !strings.Contains(page, "Not saved") || !strings.Contains(page, "changed since version 1") {
		t.Fatalf("conflicting edit:\n%s", page)
	}
}

func TestScheduleChangeAndEnd(t *testing.T) {
	e := newEnv(t)
	e.user("mona", "Mona Mid", domain.RoleMidTier)
	asset, _ := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "A1", LocationID: e.loc, Manufacturer: "F", Model: "M"})
	id, err := e.app.CreateSchedule(e.ctx, e.super, domain.PMScheduleCreated{AssetID: asset, WOType: "pm", Title: "PM", IntervalDays: 180, FirstDue: "2026-12-01"})
	if err != nil {
		t.Fatal(err)
	}
	b := e.browser()
	b.login("mona", "brass-kettle-orchard-7")
	b.get("/schedules")
	if _, page := b.post("/schedules/"+id+"/change", url.Values{"interval": {"90"}, "next_due": {"2026-11-01"}}); !strings.Contains(page, "requires a reason") {
		t.Fatalf("change without reason:\n%s", page)
	}
	b.post("/schedules/"+id+"/change", url.Values{"interval": {"90"}, "next_due": {"2026-11-01"}, "reason": {"manufacturer bulletin"}})
	s, _ := domain.GetSchedule(e.ctx, e.app.Store.DB(), id)
	if s.IntervalDays != 90 || s.NextDue != "2026-11-01" {
		t.Fatalf("schedule = %+v", s)
	}
	b.post("/schedules/"+id+"/end", url.Values{"reason": {"asset retired"}})
	if s, _ = domain.GetSchedule(e.ctx, e.app.Store.DB(), id); s.Status != "ended" {
		t.Fatalf("status = %s", s.Status)
	}
}

// flaggedCalibration records a calibration against an untracked
// reference standard, which is applied but flagged for review.
func (e *env) flaggedCalibration() (wo, asset string) {
	e.t.Helper()
	monaID := e.user("mona", "Mona Mid", domain.RoleMidTier)
	tessID := e.user("tess", "Tess Tech", domain.RoleUser)
	mona, tess := app.Actor{UserID: monaID, SessionID: "m"}, app.Actor{UserID: tessID, SessionID: "t"}
	asset, _ = e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "INST-1", LocationID: e.loc, Manufacturer: "Fluke", Model: "ESA615"})
	std, _ := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "STD-1", LocationID: e.loc, Manufacturer: "Fluke", Model: "5522A", IsReferenceStandard: true})
	wo, _, err := e.app.OpenWorkOrder(e.ctx, mona, app.NewWorkOrder{Type: "calibration", AssetID: asset, Priority: "normal", Title: "Cal"})
	if err != nil {
		e.t.Fatal(err)
	}
	e.app.AssignWorkOrder(e.ctx, mona, wo, tessID)
	e.app.ChangeWorkOrderStatus(e.ctx, tess, wo, domain.WOInProgress, "")
	if _, err := e.app.RecordCalibration(e.ctx, tess, domain.CalibrationRecorded{WorkOrderID: wo, Procedure: "v3", StandardsUsed: []string{std},
		Points: []domain.CalPoint{{Parameter: "V", Unit: "V", Nominal: "120", Tolerance: domain.Tolerance{Kind: domain.TolPercent, Value: "2"}, AsFound: "121"}}}); err != nil {
		e.t.Fatal(err)
	}
	if err := e.app.Sign(e.ctx, tess, wo, domain.MeaningPerformed, "brass-kettle-orchard-7", true); err != nil {
		e.t.Fatal(err)
	}
	return wo, asset
}

func TestReviewQueueDecisions(t *testing.T) {
	e := newEnv(t)
	wo, _ := e.flaggedCalibration()
	tech := e.browser()
	tech.login("tess", "brass-kettle-orchard-7")
	if code, _, _ := tech.get("/review"); code != http.StatusForbidden {
		t.Fatalf("user on review queue: %d", code)
	}
	b := e.browser()
	b.login("mona", "brass-kettle-orchard-7")
	_, _, page := b.get("/review")
	m := regexp.MustCompile(`/review/([0-9a-f-]{36})/resolve`).FindStringSubmatch(page)
	if m == nil || !strings.Contains(page, "standard untracked") {
		t.Fatalf("flag not listed:\n%s", page)
	}
	if !strings.Contains(page, "/work-orders/") && !strings.Contains(page, "calibration") {
		t.Fatal("no context for the flag")
	}
	_, page = b.post("/review/"+m[1]+"/resolve", url.Values{"resolution": {"acknowledged"}, "note": {"STD-1 schedule being set up; cert on file"}})
	if !strings.Contains(page, "Decision recorded") || !strings.Contains(page, "Nothing to review") {
		t.Fatalf("after deciding:\n%s", page)
	}
	if _, page = b.post("/review/"+m[1]+"/resolve", url.Values{"resolution": {"rejected"}, "note": {"again"}}); !strings.Contains(page, "already resolved") {
		t.Fatalf("second decision:\n%s", page)
	}
	if _, _, page = b.get("/review?all=1"); !strings.Contains(page, "<strong>acknowledged</strong> by Mona Mid") {
		t.Fatalf("decided list:\n%s", page)
	}
	// The decision shows in the work order's audit trail.
	if _, _, page = b.get("/work-orders/" + wo + "/audit"); !strings.Contains(page, "standard_untracked: ") || !strings.Contains(page, "acknowledged: STD-1 schedule being set up") {
		t.Fatalf("audit trail lacks the flag decision:\n%s", page)
	}
}

func TestRecordCopies(t *testing.T) {
	e := newEnv(t)
	wo, asset := e.flaggedCalibration()
	b := e.browser()
	b.login("mona", "brass-kettle-orchard-7")

	_, _, audit := b.get("/work-orders/" + wo + "/audit")
	for _, want := range []string{"Mona Mid (mona)", "Tess Tech (tess)", "workorder.opened", "calibration.recorded", "signature.applied", "&#34;as_found&#34;: &#34;121&#34;"} {
		if !strings.Contains(audit, want) {
			t.Errorf("audit trail lacks %q", want)
		}
	}
	_, _, print := b.get("/work-orders/" + wo + "/print")
	for _, want := range []string{"<strong>Tess Tech</strong> (tess)", "I performed this work as recorded.", "Content hash", "Audit trail", "pi-fleet verify-export", "121 pass"} {
		if !strings.Contains(print, want) {
			t.Errorf("printable record lacks %q", want)
		}
	}
	if strings.Contains(print, "<form") {
		t.Error("printable record contains forms")
	}

	for _, path := range []string{"/work-orders/" + wo + "/export.json", "/assets/" + asset + "/export.json"} {
		resp, err := b.c.Get(e.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") {
			t.Errorf("%s not a download", path)
		}
		var bundle export.Bundle
		if err := json.Unmarshal(body, &bundle); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		rep := export.Verify(bundle)
		if len(rep.Problems) != 0 || rep.Verified != rep.Events || rep.Events < 6 || !bundle.Complete || len(bundle.Flags) != 1 {
			t.Fatalf("%s: %+v flags %d", path, rep, len(bundle.Flags))
		}
		// Change one reading in the export: verification fails.
		tampered := strings.Replace(string(body), `"as_found":"121"`, `"as_found":"120"`, 1)
		var tb export.Bundle
		json.Unmarshal([]byte(tampered), &tb)
		for i, ev := range tb.Events {
			if ev.Type == domain.TypeCalibrationRecorded {
				tb.Events[i].Payload = []byte(strings.Replace(string(ev.Payload), `"as_found":"121"`, `"as_found":"120"`, 1))
			}
		}
		if rep := export.Verify(tb); len(rep.Problems) != 1 {
			t.Fatalf("%s: tampered export verified: %+v", path, rep)
		}
	}
	_, _, assetAudit := b.get("/assets/" + asset + "/audit")
	if !strings.Contains(assetAudit, "asset.registered") || !strings.Contains(assetAudit, "calibration.recorded") {
		t.Error("asset audit trail incomplete")
	}
	if code, _, _ := b.get("/assets/" + asset + "/print"); code != http.StatusOK {
		t.Errorf("asset print: %d", code)
	}
}
