package app

import (
	"reflect"
	"testing"

	"pi-fleet/internal/domain"
)

// The work done and the test equipment used: recorded by the person doing
// the work while it is in progress, checked like reference standards, and
// part of what is signed.
func TestWorkDoneAndTestEquipment(t *testing.T) {
	e := newEnv(t)
	mid, tech := e.activeUser("mona", domain.RoleMidTier), e.activeUser("tess", domain.RoleUser)
	pump := e.asset("PUMP-1")
	meter := e.must2(e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "IDA-1", LocationID: e.loc, Manufacturer: "Fluke", Model: "IDA-5", IsReferenceStandard: true}))
	e.must2(e.app.CreateSchedule(e.ctx, mid, domain.PMScheduleCreated{AssetID: meter, WOType: "calibration", Title: "Cal", IntervalDays: 365, FirstDue: "2027-01-10"}))
	notTest := e.asset("PUMP-2")
	wo, _, err := e.app.OpenWorkOrder(e.ctx, mid, NewWorkOrder{Type: "corrective", AssetID: pump, Priority: "normal", Title: "Not pumping"})
	e.must(err)
	_, err = e.app.AssignWorkOrder(e.ctx, mid, wo, tech.UserID)
	e.must(err)

	wantRejection(t, e.app.RecordWorkDone(e.ctx, tech, wo, "Replaced the motor."), domain.FlagInvalid) // not started yet
	e.must(e.app.ChangeWorkOrderStatus(e.ctx, tech, wo, domain.WOInProgress, ""))
	if err := e.app.RecordWorkDone(e.ctx, mid, wo, "x"); err != ErrNotLeaseHolder {
		t.Fatalf("someone else recorded the work done: %v", err)
	}
	before, err := domain.WorkOrderContentHash(e.ctx, e.st.DB(), wo)
	e.must(err)
	e.must(e.app.RecordWorkDone(e.ctx, tech, wo, "  Replaced the motor and the occlusion sensor. Tested OK.  "))
	wantRejection(t, e.app.AddTestEquipment(e.ctx, tech, wo, notTest), domain.FlagInvalid) // not test equipment
	wantRejection(t, e.app.AddTestEquipment(e.ctx, tech, wo, pump), domain.FlagInvalid)    // the equipment itself
	e.must(e.app.AddTestEquipment(e.ctx, tech, wo, meter))
	wantRejection(t, e.app.AddTestEquipment(e.ctx, tech, wo, meter), domain.FlagInvalid) // already listed

	w, err := domain.GetWorkOrder(e.ctx, e.st.DB(), wo)
	e.must(err)
	used, err := domain.WorkOrderTestEquipmentUsed(e.ctx, e.st.DB(), wo)
	e.must(err)
	if w.WorkDone != "Replaced the motor and the occlusion sensor. Tested OK." || len(used) != 1 || used[0].Tag != "IDA-1" || used[0].Due != "2027-01-10" || used[0].Overdue() {
		t.Fatalf("work done %q, used %+v", w.WorkDone, used)
	}
	after, err := domain.WorkOrderContentHash(e.ctx, e.st.DB(), wo)
	e.must(err)
	if before == after {
		t.Fatal("the work done and test equipment aren't in the signed content")
	}
	e.must(e.app.RemoveTestEquipment(e.ctx, tech, wo, meter))
	wantRejection(t, e.app.RemoveTestEquipment(e.ctx, tech, wo, meter), domain.FlagInvalid)
	e.must(e.app.AddTestEquipment(e.ctx, tech, wo, meter))

	snap := snapshot(t, e.st.DB())
	e.must(e.st.Rebuild(e.ctx))
	if !reflect.DeepEqual(snap, snapshot(t, e.st.DB())) {
		t.Fatal("rebuild differs")
	}
}
