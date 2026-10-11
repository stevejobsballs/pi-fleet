package app

import (
	"reflect"
	"testing"

	"pi-fleet/internal/domain"
)

// Calibration as checklist modules: any checklist (a repair's, a PM's)
// can include calibration points and the reference standards used. The
// master works out pass or fail from the readings, flags standards that
// are overdue or untracked, and a calibration work order completes on
// the checklist's readings.
func TestCalibrationModules(t *testing.T) {
	e := newEnv(t)
	mid, tech := e.activeUser("mona", domain.RoleMidTier), e.activeUser("tess", domain.RoleUser)
	pump := e.asset("PUMP-1")
	std := e.must2(e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "STD-1", LocationID: e.loc, Manufacturer: "Fluke", Model: "IDA-5", IsReferenceStandard: true}))
	e.must2(e.app.CreateSchedule(e.ctx, mid, domain.PMScheduleCreated{AssetID: std, WOType: "calibration", Title: "Cal", IntervalDays: 365, FirstDue: "2026-09-01"}))

	bad := []struct {
		name string
		st   domain.Step
	}{
		{"no unit", domain.Step{ID: "f", Text: "Flow", Kind: domain.StepCalibration, Nominal: "25", Tolerance: &domain.Tolerance{Kind: domain.TolAbsolute, Value: "0.5"}}},
		{"no tolerance", domain.Step{ID: "f", Text: "Flow", Kind: domain.StepCalibration, Unit: "mL/h", Nominal: "25"}},
		{"bad nominal", domain.Step{ID: "f", Text: "Flow", Kind: domain.StepCalibration, Unit: "mL/h", Nominal: "x", Tolerance: &domain.Tolerance{Kind: domain.TolAbsolute, Value: "1"}}},
		{"tolerance on a check", domain.Step{ID: "f", Text: "Flow", Kind: domain.StepCheck, Tolerance: &domain.Tolerance{Kind: domain.TolAbsolute, Value: "1"}}},
	}
	for _, b := range bad {
		_, _, err := e.app.PublishProcedure(e.ctx, mid, "Bad "+b.name, []domain.Step{b.st})
		wantRejection(t, err, domain.FlagInvalid)
	}

	// A repair checklist with the calibration modules in it.
	proc, _, err := e.app.PublishProcedure(e.ctx, mid, "Pump repair", []domain.Step{
		{ID: "fault", Text: "Fault found", Kind: domain.StepLongText, Required: true},
		{ID: "cal", Text: "Calibration after repair", Kind: domain.StepHeading},
		{ID: "stds", Text: "Reference standards used", Kind: domain.StepStandards, Required: true},
		{ID: "flow", Text: "Flow at 25 mL/h", Kind: domain.StepCalibration, Unit: "mL/h", Nominal: "25",
			Tolerance: &domain.Tolerance{Kind: domain.TolPercent, Value: "5"}, Required: true},
		{ID: "occl", Text: "Occlusion pressure", Kind: domain.StepCalibration, Unit: "kPa",
			Tolerance: &domain.Tolerance{Kind: domain.TolLimits, Lower: "60", Upper: "100"}},
	})
	e.must(err)
	wo, _, err := e.app.OpenWorkOrder(e.ctx, mid, NewWorkOrder{Type: "calibration", AssetID: pump, Priority: "normal", Title: "Repair and calibrate", ProcedureID: proc})
	e.must(err)
	_, err = e.app.AssignWorkOrder(e.ctx, mid, wo, tech.UserID)
	e.must(err)
	e.must(e.app.ChangeWorkOrderStatus(e.ctx, tech, wo, domain.WOInProgress, ""))
	answer := func(step, value, asLeft string) error {
		return e.app.RecordAnswer(e.ctx, tech, wo, "", step, value, "", "", asLeft)
	}

	wantRejection(t, answer("fault", "Cracked tubing guide", "x"), domain.FlagInvalid) // as left only on a calibration point
	wantRejection(t, answer("flow", "fast", ""), domain.FlagInvalid)
	wantRejection(t, answer("stds", `["`+pump+`"]`, ""), domain.FlagInvalid) // itself, and not a standard
	wantRejection(t, answer("stds", `[]`, ""), domain.FlagInvalid)           // required
	e.must(answer("fault", "Cracked tubing guide", ""))
	e.must(answer("stds", `["`+std+`","`+std+`"]`, ""))
	// Out as found (26.5 > 26.25), adjusted, in as left.
	e.must(answer("flow", "26.5", "25.1"))
	// A calibration WO can't complete while its readings fail as left.
	e.must(answer("occl", "55", ""))
	wantRejection(t, e.app.ChangeWorkOrderStatus(e.ctx, tech, wo, domain.WOCompleted, ""), domain.FlagInvalid)

	w, err := domain.GetWorkOrder(e.ctx, e.st.DB(), wo)
	e.must(err)
	cs, err := domain.Checklists(e.ctx, e.st.DB(), w)
	e.must(err)
	steps := map[string]domain.StepResult{}
	for _, s := range cs[0].Steps {
		steps[s.ID] = s
	}
	flow, occl, stds := steps["flow"], steps["occl"], steps["stds"]
	if *flow.FoundPass || !*flow.Pass || flow.AsLeft != "25.1" || flow.Summary() != "as found 26.5 mL/h (fail); as left 25.1 mL/h (pass)" {
		t.Fatalf("flow: %+v %q", flow, flow.Summary())
	}
	if *occl.Pass || occl.Summary() != "as found 55 kPa (fail); not adjusted" || occl.CalLimits() != "60 to 100" {
		t.Fatalf("occlusion: %q, limits %q", occl.Summary(), occl.CalLimits())
	}
	if len(stds.Standards) != 1 || stds.Standards[0].Tag != "STD-1" || stds.Standards[0].Due != "2026-09-01" || stds.Value != `["`+std+`"]` {
		t.Fatalf("standards: %+v", stds)
	}
	var evID string
	e.must(e.st.DB().QueryRowContext(e.ctx, `SELECT event_id FROM checklist_results WHERE wo_id = ? AND step_id = 'stds'`, wo).Scan(&evID))
	fs, err := e.st.Flags(e.ctx, evID)
	e.must(err)
	if len(fs) != 1 || fs[0].Flag != domain.FlagStandardOverdue {
		t.Fatalf("flags = %+v", fs)
	}

	// The as-left reading is part of what's signed.
	before, err := domain.WorkOrderContentHash(e.ctx, e.st.DB(), wo)
	e.must(err)
	e.must(answer("occl", "55", "80"))
	after, err := domain.WorkOrderContentHash(e.ctx, e.st.DB(), wo)
	e.must(err)
	if before == after {
		t.Fatal("as-left reading not in the signed content")
	}
	e.must(e.sign(tech, wo, domain.MeaningPerformed))
	if w, _ = domain.GetWorkOrder(e.ctx, e.st.DB(), wo); w.Status != domain.WOCompleted {
		t.Fatalf("status %s, want completed", w.Status)
	}

	// Rebuilding from the events gives the same records.
	snap := snapshot(t, e.st.DB())
	e.must(e.st.Rebuild(e.ctx))
	if !reflect.DeepEqual(snap, snapshot(t, e.st.DB())) {
		t.Fatal("rebuild differs")
	}
}
