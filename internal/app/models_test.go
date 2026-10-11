package app

import (
	"testing"

	"pi-fleet/internal/domain"
)

// The catalogue: mid-tier users define models; anyone registering a
// model's equipment gets its schedules, but only exactly as catalogued.
func TestModelCatalogue(t *testing.T) {
	e := newEnv(t)
	mid := e.activeUser("mona", domain.RoleMidTier)
	tech := e.activeUser("tess", domain.RoleUser)
	proc, _, err := e.app.PublishProcedure(e.ctx, mid, "Analyser PM", []domain.Step{{ID: "s1", Text: "Leads intact", Kind: domain.StepCheck}})
	e.must(err)

	def := domain.ModelDefined{Manufacturer: "Fluke", Model: "ESA615", Kind: "Electrical safety analyser", RiskClass: "medium",
		Schedules: []domain.ModelSchedule{
			{WOType: "calibration", Title: "Yearly calibration", IntervalDays: 365, GraceDays: 30},
			{WOType: "pm", Title: "Six-monthly PM", IntervalDays: 182, Checklist: "Analyser PM"},
		}}
	_, err = e.app.DefineModel(e.ctx, tech, def)
	wantRejection(t, err, domain.FlagNotAuthorized)
	model, err := e.app.DefineModel(e.ctx, mid, def)
	e.must(err)
	// The same model again, written differently, is refused.
	_, err = e.app.DefineModel(e.ctx, mid, domain.ModelDefined{Manufacturer: " fluke", Model: "esa615 ", Kind: "x"})
	wantRejection(t, err, domain.FlagConflict)
	bad := def
	bad.Schedules = []domain.ModelSchedule{{WOType: "pm", Title: "PM", IntervalDays: 30, Checklist: "No such checklist"}}
	_, err = e.app.DefineModel(e.ctx, mid, domain.ModelDefined{Manufacturer: "GE", Model: "MAC", Kind: "ECG", Schedules: bad.Schedules})
	wantRejection(t, err, domain.FlagInvalid)

	// A technician registers one: its schedules are set up, once.
	asset := e.must2(e.app.RegisterAsset(e.ctx, tech, domain.AssetRegistered{Tag: "ESA-1", LocationID: e.loc, Manufacturer: "FLUKE", Model: "ESA615"}))
	n, err := e.app.SetUpModelSchedules(e.ctx, tech, model, asset)
	e.must(err)
	if n != 2 {
		t.Fatalf("set up %d schedules, want 2", n)
	}
	if n, err = e.app.SetUpModelSchedules(e.ctx, tech, model, asset); err != nil || n != 0 {
		t.Fatalf("again: %d, %v", n, err)
	}
	var got, due string
	e.must(e.app.Store.DB().QueryRow(`SELECT procedure_id, next_due FROM pm_schedules WHERE asset_id = ? AND wo_type = 'pm'`, asset).Scan(&got, &due))
	if got != proc || due != e.now.AddDate(0, 0, 182).Format("2006-01-02") {
		t.Fatalf("pm schedule: checklist %s, due %s", got, due)
	}
	// Anything other than the catalogue's schedule still needs mid-tier.
	other := e.must2(e.app.RegisterAsset(e.ctx, tech, domain.AssetRegistered{Tag: "ESA-2", LocationID: e.loc, Manufacturer: "Fluke", Model: "ESA615"}))
	_, err = e.app.CreateSchedule(e.ctx, tech, domain.PMScheduleCreated{AssetID: other, WOType: "calibration", Title: "Yearly calibration",
		IntervalDays: 90, GraceDays: 30, FirstDue: "2027-01-01", FromModel: model})
	wantRejection(t, err, domain.FlagNotAuthorized)
	ecg := e.must2(e.app.RegisterAsset(e.ctx, tech, domain.AssetRegistered{Tag: "ECG-1", LocationID: e.loc, Manufacturer: "GE", Model: "MAC"}))
	_, err = e.app.CreateSchedule(e.ctx, tech, domain.PMScheduleCreated{AssetID: ecg, WOType: "calibration", Title: "Yearly calibration",
		IntervalDays: 365, GraceDays: 30, FirstDue: "2027-01-01", FromModel: model})
	wantRejection(t, err, domain.FlagInvalid)

	// Changes need the version last seen.
	m, err := domain.GetModel(e.ctx, e.app.Store.DB(), model)
	e.must(err)
	changed := domain.ModelChanged(def)
	changed.RiskClass = "high"
	e.must(e.app.ChangeModel(e.ctx, mid, model, m.Version, changed))
	wantRejection(t, e.app.ChangeModel(e.ctx, mid, model, m.Version, changed), domain.FlagStaleBase)
	if m, _ = domain.ModelOf(e.ctx, e.app.Store.DB(), "fluke", "ESA615"); m.RiskClass != "high" || m.Version != 2 {
		t.Fatalf("changed model: %+v", m)
	}
}
