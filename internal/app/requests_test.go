package app

import (
	"errors"
	"strings"
	"testing"

	"pi-fleet/internal/domain"
	"pi-fleet/internal/store"
)

func report(assetID string) domain.RequestSubmitted {
	return domain.RequestSubmitted{AssetID: assetID, Category: "Alarm or error message", Description: "Occlusion alarm keeps sounding",
		Name: "Nora Nurse", Department: "Ward 3B", Phone: "x4410"}
}

func TestServiceRequests(t *testing.T) {
	e := newEnv(t)
	pump := e.asset("PUMP-1")
	id, number, err := e.app.SubmitRequest(e.ctx, report(pump))
	e.must(err)
	_, second, err := e.app.SubmitRequest(e.ctx, report(pump))
	e.must(err)
	if number != "R-00001" || second != "R-00002" {
		t.Fatalf("numbers %s %s", number, second)
	}
	var rej *store.Rejection
	bad := report(pump)
	bad.Phone = " "
	if _, _, err := e.app.SubmitRequest(e.ctx, bad); !errors.As(err, &rej) {
		t.Fatalf("no phone: %v", err)
	}
	bad = report(pump)
	bad.Category = "Make me coffee"
	if _, _, err := e.app.SubmitRequest(e.ctx, bad); !errors.As(err, &rej) {
		t.Fatalf("unknown problem: %v", err)
	}
	// Only the report page records reports: not a signed-in person, and
	// not a forged system event.
	if err := e.app.Store.Update(e.ctx, func(tx *store.Tx) error {
		return e.app.emit(e.ctx, tx, e.super, domain.TypeRequestSubmitted, domain.EntityRequest, newID(), 0, "", report(pump))
	}); !errors.As(err, &rej) {
		t.Fatalf("report by a user: %v", err)
	}

	tess := e.activeUser("tess", domain.RoleUser)
	woID, woNumber, err := e.app.ConvertRequest(e.ctx, tess, id, "corrective", "high")
	e.must(err)
	r, err := domain.GetRequest(e.ctx, e.app.Store.DB(), number)
	e.must(err)
	if r.Status != domain.RequestStatusConverted || r.WorkOrderID != woID || r.WorkOrderNumber != woNumber || r.WorkOrderStatus != "open" {
		t.Fatalf("converted: %+v", r)
	}
	wo, err := domain.GetWorkOrder(e.ctx, e.app.Store.DB(), woID)
	e.must(err)
	if wo.Title != "Alarm or error message" || !strings.Contains(wo.Problem, "Occlusion alarm") || !strings.Contains(wo.Problem, "R-00001") ||
		!strings.Contains(wo.Problem, "Nora Nurse, Ward 3B, x4410") || wo.Priority != "high" {
		t.Fatalf("work order: %+v", wo)
	}
	if _, _, err := e.app.ConvertRequest(e.ctx, tess, id, "corrective", "high"); !errors.As(err, &rej) {
		t.Fatalf("converted twice: %v", err)
	}
	if err := e.app.CloseRequest(e.ctx, tess, r.ID, " "); !errors.As(err, &rej) {
		t.Fatalf("closed without a reason: %v", err)
	}
	r2, _ := domain.GetRequest(e.ctx, e.app.Store.DB(), second)
	e.must(e.app.CloseRequest(e.ctx, tess, r2.ID, "Same as R-00001"))
	if open, _ := domain.ListRequests(e.ctx, e.app.Store.DB(), domain.RequestStatusNew, 10); len(open) != 0 {
		t.Fatalf("still new: %+v", open)
	}
	e.must(e.st.Rebuild(e.ctx))
	if r, _ := domain.GetRequest(e.ctx, e.app.Store.DB(), second); r.Status != domain.RequestStatusClosed || r.ClosedReason != "Same as R-00001" {
		t.Fatalf("after rebuild: %+v", r)
	}
}

func TestReportTarget(t *testing.T) {
	e := newEnv(t)
	plain := e.asset("PLAIN-1")
	kept := e.assetWithMasterID("PUMP-1", "M-1")
	dup := e.assetWithMasterID("PROV-1", "M-1")
	e.must(e.app.MergeAssets(e.ctx, e.super, kept, 1, dup, "same pump", true))
	for code, want := range map[string]string{"M-1": kept, "PLAIN-1": plain} {
		a, err := domain.ReportTarget(e.ctx, e.app.Store.DB(), code)
		e.must(err)
		if a.ID != want {
			t.Errorf("%s -> %s", code, a.Tag)
		}
	}
	for _, code := range []string{"PUMP-1", "PROV-1", "nope"} { // a tag when there's a MasterID, a merged record, nothing
		if _, err := domain.ReportTarget(e.ctx, e.app.Store.DB(), code); !errors.Is(err, domain.ErrNotFound) {
			t.Errorf("%s: %v", code, err)
		}
	}
	// A report against a merged duplicate lands on the record kept.
	_, number, err := e.app.SubmitRequest(e.ctx, report(dup))
	e.must(err)
	if r, _ := domain.GetRequest(e.ctx, e.app.Store.DB(), number); r.AssetID != kept {
		t.Fatalf("report on a duplicate went to %s", r.AssetTag)
	}
}
