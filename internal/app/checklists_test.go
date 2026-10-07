package app

import (
	"errors"
	"testing"

	"pi-fleet/internal/domain"
)

var pmSteps = []domain.Step{
	{ID: "visual", Text: "Housing, cords and labels intact", Kind: domain.StepCheck, Required: true},
	{ID: "leak", Text: "Earth leakage", Kind: domain.StepNumber, Unit: "µA", Lower: "0", Upper: "300", Required: true},
	{ID: "battery", Text: "Battery runtime test", Kind: domain.StepCheck},
	{ID: "notes", Text: "Observations", Kind: domain.StepText},
}

func TestChecklistOnScheduledWork(t *testing.T) {
	e := newEnv(t)
	mid, tech := e.activeUser("mona", domain.RoleMidTier), e.activeUser("tess", domain.RoleUser)
	asset := e.asset("A1")

	_, _, err := e.app.PublishProcedure(e.ctx, tech, "Infusion pump PM", pmSteps)
	wantRejection(t, err, domain.FlagNotAuthorized)
	_, _, err = e.app.PublishProcedure(e.ctx, mid, "Bad", []domain.Step{{ID: "x", Text: "x", Kind: domain.StepNumber, Lower: "5", Upper: "1"}})
	wantRejection(t, err, domain.FlagInvalid)
	v1, ver, err := e.app.PublishProcedure(e.ctx, mid, "Infusion pump PM", pmSteps)
	e.must(err)
	if ver != 1 {
		t.Fatalf("version = %d", ver)
	}
	_, ver, err = e.app.PublishProcedure(e.ctx, mid, "Infusion pump PM", pmSteps[:2])
	e.must(err)
	if ver != 2 {
		t.Fatalf("second version = %d", ver)
	}

	// The schedule's checklist carries into the generated work order.
	s := e.must2(e.app.CreateSchedule(e.ctx, mid, domain.PMScheduleCreated{AssetID: asset, WOType: "pm", Title: "PM", IntervalDays: 180, FirstDue: "2026-10-20", ProcedureID: v1}))
	_, err = e.app.GenerateDueWorkOrders(e.ctx, WorkingSetWindow)
	e.must(err)
	sched, _ := domain.GetSchedule(e.ctx, e.st.DB(), s)
	wo := sched.OpenWorkOrderID
	w, _ := domain.GetWorkOrder(e.ctx, e.st.DB(), wo)
	if w.ProcedureID != v1 {
		t.Fatalf("work order checklist = %q", w.ProcedureID)
	}
	_, err = e.app.AssignWorkOrder(e.ctx, mid, wo, tech.UserID)
	e.must(err)

	// Steps are recorded only while in progress, by the holder.
	if err := e.app.RecordStep(e.ctx, tech, wo, "visual", "pass", ""); err == nil {
		t.Fatal("step recorded before work started")
	}
	e.must(e.app.ChangeWorkOrderStatus(e.ctx, tech, wo, domain.WOInProgress, ""))
	if err := e.app.RecordStep(e.ctx, mid, wo, "visual", "pass", ""); !errors.Is(err, ErrNotLeaseHolder) {
		t.Fatalf("non-holder: %v", err)
	}
	wantRejection(t, e.app.RecordStep(e.ctx, tech, wo, "nope", "pass", ""), domain.FlagInvalid)
	wantRejection(t, e.app.RecordStep(e.ctx, tech, wo, "leak", "1e2", ""), domain.FlagInvalid)
	wantRejection(t, e.app.RecordStep(e.ctx, tech, wo, "visual", "n/a", ""), domain.FlagInvalid) // required
	e.must(e.app.RecordStep(e.ctx, tech, wo, "visual", "pass", ""))

	// Completion needs every required step.
	wantRejection(t, e.sign(tech, wo, domain.MeaningPerformed), domain.FlagInvalid)
	e.must(e.app.RecordStep(e.ctx, tech, wo, "leak", "350", "high reading"))
	e.must(e.app.RecordStep(e.ctx, tech, wo, "leak", "120", "re-measured after reseating plug"))
	_, results, err := domain.Checklist(e.ctx, e.st.DB(), w)
	e.must(err)
	if results[1].Value != "120" || results[1].Pass == nil || !*results[1].Pass {
		t.Fatalf("leak result = %+v", results[1])
	}
	e.must(e.sign(tech, wo, domain.MeaningPerformed))

	// A recorded step after signing would change the signed content: the
	// reviewer must sign over the checklist as it is.
	e.must(e.app.ChangeWorkOrderStatus(e.ctx, mid, wo, domain.WOInProgress, "battery test missing"))
	e.must(e.app.RecordStep(e.ctx, tech, wo, "battery", "fail", "runs 40 min, spec 60"))
	sigs, _ := domain.WorkOrderSignatures(e.ctx, e.st.DB(), wo)
	if !sigs[0].Stale {
		t.Fatal("signature not stale after the checklist changed")
	}

	// The checklist can't be swapped once work has started.
	wantRejection(t, e.app.SetWorkOrderProcedure(e.ctx, mid, wo, ""), domain.FlagInvalid)
	// Retired versions can't be chosen for new work.
	e.must(e.app.RetireProcedure(e.ctx, mid, v1, "superseded by v2"))
	wo2, _, err := e.app.OpenWorkOrder(e.ctx, mid, NewWorkOrder{Type: "pm", AssetID: asset, Priority: "low", Title: "x", ProcedureID: v1})
	wantRejection(t, err, domain.FlagInvalid)
	_ = wo2
}

func TestLabor(t *testing.T) {
	e := newEnv(t)
	mid, tech, other := e.activeUser("mona", domain.RoleMidTier), e.activeUser("tess", domain.RoleUser), e.activeUser("otto", domain.RoleUser)
	wo := e.openHeld(mid, tech, "corrective", e.asset("A1"))

	id, err := e.app.LogLabor(e.ctx, tech, wo, 45, "2026-10-06", "diagnosis")
	e.must(err)
	_, err = e.app.LogLabor(e.ctx, other, wo, 30, "2026-10-06", "helped lift")
	e.must(err)
	_, err = e.app.LogLabor(e.ctx, tech, wo, 0, "2026-10-06", "")
	wantRejection(t, err, domain.FlagInvalid)
	_, err = e.app.LogLabor(e.ctx, tech, wo, 30, "2026-12-01", "")
	wantRejection(t, err, domain.FlagInvalid) // future date
	wantRejection(t, e.app.ReverseLabor(e.ctx, other, id, "not mine"), domain.FlagNotAuthorized)
	e.must(e.app.ReverseLabor(e.ctx, tech, id, "logged on the wrong job"))

	entries, total, err := domain.Labor(e.ctx, e.st.DB(), wo)
	e.must(err)
	if len(entries) != 2 || total != 30 {
		t.Fatalf("entries %d total %d", len(entries), total)
	}
	// Labour is not part of the signed record.
	before, _ := domain.WorkOrderContentHash(e.ctx, e.st.DB(), wo)
	_, err = e.app.LogLabor(e.ctx, tech, wo, 15, "2026-10-06", "paperwork")
	e.must(err)
	if after, _ := domain.WorkOrderContentHash(e.ctx, e.st.DB(), wo); after != before {
		t.Fatal("logging time changed the signed content")
	}
}
