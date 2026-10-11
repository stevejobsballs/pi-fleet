package app

import (
	"strings"
	"testing"

	"pi-fleet/internal/domain"
)

// A checklist using every kind of step and setting, as a defibrillator
// inspection might.
var defibSteps = []domain.Step{
	{ID: "safety", Text: "Before you start", Kind: domain.StepHeading},
	{ID: "warn", Text: "Discharge into the test load only. Keep clear of the paddles.", Kind: domain.StepInfo},
	{ID: "condition", Text: "Housing condition", Kind: domain.StepChoice, Required: true, NoteOnFail: true,
		Options: []domain.StepOption{{Label: "Good"}, {Label: "Worn"}, {Label: "Cracked", Fails: true}}},
	{ID: "battery", Text: "Battery fitted?", Kind: domain.StepChoice, Required: true, Options: []domain.StepOption{{Label: "Yes"}, {Label: "No"}}},
	{ID: "volts", Text: "Battery voltage", Kind: domain.StepNumber, Unit: "V", Lower: "11.5", Upper: "13", Required: true,
		ShowIf: &domain.StepCondition{Step: "battery", Equals: "Yes"}},
	{ID: "expiry", Text: "Battery expiry date", Kind: domain.StepDate, ShowIf: &domain.StepCondition{Step: "battery", Equals: "Yes"}},
	{ID: "energy", Text: "Delivered energy at 200 J", Kind: domain.StepNumber, Unit: "J", Lower: "170", Upper: "230", Required: true, Critical: true,
		Help: "Average of three discharges into the analyser."},
	{ID: "parts", Text: "Parts replaced", Kind: domain.StepMulti, Options: []domain.StepOption{{Label: "Pads"}, {Label: "Battery"}, {Label: "Cable"}}},
	{ID: "notes", Text: "Observations", Kind: domain.StepLongText},
}

func TestRichChecklist(t *testing.T) {
	e := newEnv(t)
	mid, tech := e.activeUser("mona", domain.RoleMidTier), e.activeUser("tess", domain.RoleUser)
	asset := e.asset("DEFIB-1")
	proc, _, err := e.app.PublishChecklist(e.ctx, mid, domain.ProcedurePublished{Name: "Defibrillator inspection", Description: "Yearly.",
		Reference: "IEC 60601-2-4", Category: "Performance check", Steps: defibSteps})
	e.must(err)
	p, err := domain.GetProcedure(e.ctx, e.st.DB(), proc)
	e.must(err)
	if p.Reference != "IEC 60601-2-4" || p.Category != "Performance check" || len(p.Steps[2].Options) != 3 || p.Steps[4].ShowIf.Step != "battery" {
		t.Fatalf("published: %+v", p)
	}
	wo, _, err := e.app.OpenWorkOrder(e.ctx, mid, NewWorkOrder{Type: "inspection", AssetID: asset, Priority: "normal", Title: "Inspection", ProcedureID: proc})
	e.must(err)
	_, err = e.app.AssignWorkOrder(e.ctx, mid, wo, tech.UserID)
	e.must(err)
	e.must(e.app.ChangeWorkOrderStatus(e.ctx, tech, wo, domain.WOInProgress, ""))
	step := func(id, value, note string) error { return e.app.RecordStep(e.ctx, tech, wo, id, value, note) }

	wantRejection(t, step("warn", "ok", ""), domain.FlagInvalid)               // an instruction takes no answer
	wantRejection(t, step("condition", "Shiny", ""), domain.FlagInvalid)       // not an option
	wantRejection(t, step("condition", "Cracked", ""), domain.FlagInvalid)     // a failure needs a note
	wantRejection(t, step("volts", "12.4", ""), domain.FlagInvalid)            // battery not yet answered
	wantRejection(t, step("expiry", "next year", ""), domain.FlagInvalid)      // not a date (and not yet applicable)
	wantRejection(t, step("parts", `["Pads","Fuse"]`, ""), domain.FlagInvalid) // not an option
	e.must(step("condition", "Worn", ""))
	e.must(step("battery", "Yes", ""))
	e.must(step("volts", "12.4", ""))
	e.must(step("expiry", "2027-03-01", ""))
	e.must(step("parts", `["Pads","Battery"]`, ""))
	e.must(step("notes", "Pads were past their date.\nReplaced both.", ""))

	// A required step still missing blocks completion.
	if err := e.sign(tech, wo, domain.MeaningPerformed); err == nil || !strings.Contains(err.Error(), "Delivered energy") {
		t.Fatalf("completed without the energy test: %v", err)
	}
	// A failed critical step blocks completion while the equipment is in service.
	e.must(step("energy", "150", "low on all three discharges"))
	if err := e.sign(tech, wo, domain.MeaningPerformed); err == nil || !strings.Contains(err.Error(), "take the equipment out of service") {
		t.Fatalf("completed with a failed critical step: %v", err)
	}
	a, _ := domain.GetAsset(e.ctx, e.st.DB(), asset)
	e.must(e.app.SetAssetStatus(e.ctx, tech, asset, a.Version, domain.AssetOutOfService, "failed energy test"))
	e.must(e.sign(tech, wo, domain.MeaningPerformed))

	w, _ := domain.GetWorkOrder(e.ctx, e.st.DB(), wo)
	_, results, err := domain.Checklist(e.ctx, e.st.DB(), w)
	e.must(err)
	by := map[string]domain.StepResult{}
	for _, r := range results {
		by[r.ID] = r
	}
	if by["parts"].Display() != "Pads, Battery" || !by["parts"].Chosen("Pads") || by["parts"].Pass != nil {
		t.Fatalf("parts: %+v", by["parts"])
	}
	if by["condition"].Pass == nil || !*by["condition"].Pass || by["energy"].Outcome() != "fail" || !by["volts"].Applies || !by["warn"].Applies {
		t.Fatalf("results: %+v", results)
	}
}

func TestConditionalStepsAreSkipped(t *testing.T) {
	e := newEnv(t)
	mid, tech := e.activeUser("mona", domain.RoleMidTier), e.activeUser("tess", domain.RoleUser)
	asset := e.asset("DEFIB-2")
	proc, _, err := e.app.PublishChecklist(e.ctx, mid, domain.ProcedurePublished{Name: "Defib", Steps: defibSteps})
	e.must(err)
	wo, _, _ := e.app.OpenWorkOrder(e.ctx, mid, NewWorkOrder{Type: "inspection", AssetID: asset, Priority: "normal", Title: "x", ProcedureID: proc})
	e.app.AssignWorkOrder(e.ctx, mid, wo, tech.UserID)
	e.must(e.app.ChangeWorkOrderStatus(e.ctx, tech, wo, domain.WOInProgress, ""))
	e.must(e.app.RecordStep(e.ctx, tech, wo, "condition", "Good", ""))
	e.must(e.app.RecordStep(e.ctx, tech, wo, "battery", "No", ""))
	e.must(e.app.RecordStep(e.ctx, tech, wo, "energy", "200", ""))
	// No battery: its required voltage step doesn't apply.
	e.must(e.sign(tech, wo, domain.MeaningPerformed))
	w, _ := domain.GetWorkOrder(e.ctx, e.st.DB(), wo)
	_, results, _ := domain.Checklist(e.ctx, e.st.DB(), w)
	if results[4].Applies || results[5].Applies {
		t.Fatalf("battery steps apply without a battery: %+v", results[4:6])
	}
}

func TestChecklistDefinitionsAreChecked(t *testing.T) {
	e := newEnv(t)
	mid := e.activeUser("mona", domain.RoleMidTier)
	for name, steps := range map[string][]domain.Step{
		"only a heading":          {{ID: "h", Text: "Section", Kind: domain.StepHeading}},
		"one option":              {{ID: "c", Text: "Pick", Kind: domain.StepChoice, Options: []domain.StepOption{{Label: "Only"}}}},
		"same option twice":       {{ID: "c", Text: "Pick", Kind: domain.StepChoice, Options: []domain.StepOption{{Label: "A"}, {Label: "A"}}}},
		"required heading":        {{ID: "h", Text: "Section", Kind: domain.StepHeading, Required: true}, {ID: "x", Text: "x", Kind: domain.StepText}},
		"critical without a fail": {{ID: "t", Text: "Notes", Kind: domain.StepText, Critical: true}},
		"depends on a later step": {{ID: "a", Text: "A", Kind: domain.StepText, ShowIf: &domain.StepCondition{Step: "b", Equals: "x"}}, {ID: "b", Text: "B", Kind: domain.StepText}},
		"depends on a non-option": {{ID: "b", Text: "B", Kind: domain.StepChoice, Options: []domain.StepOption{{Label: "Yes"}, {Label: "No"}}},
			{ID: "a", Text: "A", Kind: domain.StepText, ShowIf: &domain.StepCondition{Step: "b", Equals: "Maybe"}}},
		"options on a reading": {{ID: "n", Text: "N", Kind: domain.StepNumber, Options: []domain.StepOption{{Label: "A"}, {Label: "B"}}}},
		"unknown kind":         {{ID: "n", Text: "N", Kind: "photo"}},
	} {
		if _, _, err := e.app.PublishProcedure(e.ctx, mid, "Bad "+name, steps); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSeveralChecklistsOnAWorkOrder(t *testing.T) {
	e := newEnv(t)
	mid, tech, other := e.activeUser("mona", domain.RoleMidTier), e.activeUser("tess", domain.RoleUser), e.activeUser("otto", domain.RoleUser)
	asset := e.assetMade("DEFIB-3", "Zoll", "R Series")
	pm, _, err := e.app.PublishChecklist(e.ctx, mid, domain.ProcedurePublished{Name: "Zoll R PM", AppliesTo: []domain.EquipmentMatch{{Manufacturer: "Zoll", Model: "R Series"}},
		Steps: []domain.Step{{ID: "s1", Text: "Self test passes", Kind: domain.StepCheck, Required: true}}})
	e.must(err)
	esa, _, err := e.app.PublishChecklist(e.ctx, mid, domain.ProcedurePublished{Name: "Electrical safety",
		Steps: []domain.Step{{ID: "s1", Text: "Earth leakage", Kind: domain.StepNumber, Unit: "µA", Upper: "500", Required: true}}})
	e.must(err)
	zollAll, _, err := e.app.PublishChecklist(e.ctx, mid, domain.ProcedurePublished{Name: "Zoll cleaning", AppliesTo: []domain.EquipmentMatch{{Manufacturer: "ZOLL "}},
		Steps: []domain.Step{{ID: "s1", Text: "Cleaned", Kind: domain.StepCheck}}})
	e.must(err)
	_, _, err = e.app.PublishChecklist(e.ctx, mid, domain.ProcedurePublished{Name: "Philips PM", AppliesTo: []domain.EquipmentMatch{{Manufacturer: "Philips", Model: "MX40"}},
		Steps: []domain.Step{{ID: "s1", Text: "x", Kind: domain.StepCheck}}})
	e.must(err)

	// Matching checklists come first: this model, this manufacturer, any equipment, the rest.
	matches, err := domain.ChecklistsFor(e.ctx, e.st.DB(), "zoll", " R  series")
	e.must(err)
	var order []string
	for _, m := range matches {
		order = append(order, m.Name+":"+m.Fit)
	}
	if strings.Join(order, ",") != "Zoll R PM:model,Zoll cleaning:manufacturer,Electrical safety:any,Philips PM:" {
		t.Fatalf("order: %v", order)
	}

	wo, _, err := e.app.OpenWorkOrder(e.ctx, mid, NewWorkOrder{Type: "pm", AssetID: asset, Priority: "normal", Title: "PM", ProcedureID: pm})
	e.must(err)
	_, err = e.app.AssignWorkOrder(e.ctx, mid, wo, tech.UserID)
	e.must(err)
	// The technician adds the electrical safety test; someone else can't.
	wantRejection(t, e.app.AddChecklist(e.ctx, other, wo, esa), domain.FlagNotAuthorized)
	e.must(e.app.AddChecklist(e.ctx, tech, wo, esa))
	wantRejection(t, e.app.AddChecklist(e.ctx, tech, wo, esa), domain.FlagConflict)
	e.must(e.app.AddChecklist(e.ctx, mid, wo, zollAll))
	e.must(e.app.ChangeWorkOrderStatus(e.ctx, tech, wo, domain.WOInProgress, ""))
	// Both checklists have a step "s1": answers are kept apart.
	e.must(e.app.RecordChecklistStep(e.ctx, tech, wo, pm, "s1", "pass", ""))
	e.must(e.app.RecordChecklistStep(e.ctx, tech, wo, esa, "s1", "120", ""))
	// A checklist with answers stays; one without can be removed.
	wantRejection(t, e.app.RemoveChecklist(e.ctx, tech, wo, esa), domain.FlagInvalid)
	e.must(e.app.RemoveChecklist(e.ctx, tech, wo, zollAll))
	w, _ := domain.GetWorkOrder(e.ctx, e.st.DB(), wo)
	all, err := domain.Checklists(e.ctx, e.st.DB(), w)
	e.must(err)
	if len(all) != 2 || !all[0].First || all[0].Steps[0].Value != "pass" || all[1].Name != "Electrical safety" || all[1].Steps[0].Value != "120" ||
		all[1].AddedBy != "Tess" {
		t.Fatalf("checklists: %+v", all)
	}
	// Completion needs every checklist.
	e.must(e.sign(tech, wo, domain.MeaningPerformed))
	wantRejection(t, e.app.AddChecklist(e.ctx, tech, wo, zollAll), domain.FlagInvalid) // finished
	e.must(e.st.Rebuild(e.ctx))
	if all, _ := domain.Checklists(e.ctx, e.st.DB(), w); len(all) != 2 || all[1].Steps[0].Value != "120" {
		t.Fatalf("after rebuild: %+v", all)
	}
}

func TestAddingTheOnlyRequiredChecklistBlocksCompletion(t *testing.T) {
	e := newEnv(t)
	mid, tech := e.activeUser("mona", domain.RoleMidTier), e.activeUser("tess", domain.RoleUser)
	asset := e.asset("PUMP-9")
	esa, _, _ := e.app.PublishChecklist(e.ctx, mid, domain.ProcedurePublished{Name: "Electrical safety",
		Steps: []domain.Step{{ID: "s1", Text: "Earth leakage", Kind: domain.StepNumber, Required: true}}})
	wo, _, _ := e.app.OpenWorkOrder(e.ctx, mid, NewWorkOrder{Type: "corrective", AssetID: asset, Priority: "normal", Title: "Fix"})
	e.app.AssignWorkOrder(e.ctx, mid, wo, tech.UserID)
	e.must(e.app.ChangeWorkOrderStatus(e.ctx, tech, wo, domain.WOInProgress, ""))
	e.must(e.app.AddChecklist(e.ctx, tech, wo, esa)) // no checklist yet: it becomes the first
	if err := e.sign(tech, wo, domain.MeaningPerformed); err == nil || !strings.Contains(err.Error(), "Earth leakage") {
		t.Fatalf("completed without the added checklist: %v", err)
	}
	e.must(e.app.RecordStep(e.ctx, tech, wo, "s1", "80", ""))
	e.must(e.sign(tech, wo, domain.MeaningPerformed))
}

func (e *env) assetMade(tag, manufacturer, model string) string {
	e.t.Helper()
	return e.must2(e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: tag, LocationID: e.loc, Manufacturer: manufacturer, Model: model}))
}
