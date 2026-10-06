package app

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"pi-fleet/internal/domain"
)

// calSetup is a calibration work order in progress, held by tech, on an
// instrument at NYC, plus an in-date reference standard.
type calSetup struct {
	mid, tech Actor
	asset     string
	standard  string
	wo        string
}

func (e *env) calSetup() calSetup {
	e.t.Helper()
	c := calSetup{mid: e.activeUser("mona", domain.RoleMidTier), tech: e.activeUser("tess", domain.RoleUser)}
	c.asset = e.asset("INST-1")
	c.standard = e.must2(e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{
		Tag: "STD-1", LocationID: e.loc, Manufacturer: "Fluke", Model: "5522A", Serial: "S1", IsReferenceStandard: true,
	}))
	e.must2(e.app.CreateSchedule(e.ctx, c.mid, domain.PMScheduleCreated{
		AssetID: c.standard, WOType: "calibration", Title: "Annual standard cal", Procedure: "OEM", IntervalDays: 365, FirstDue: "2027-03-01",
	}))
	c.wo = e.openHeld(c.mid, c.tech, "calibration", c.asset)
	return c
}

// openHeld opens a work order, assigns it to holder, and starts it.
func (e *env) openHeld(mid, holder Actor, typ, asset string) string {
	e.t.Helper()
	wo, _, err := e.app.OpenWorkOrder(e.ctx, mid, NewWorkOrder{Type: typ, AssetID: asset, Priority: "normal", Title: typ})
	e.must(err)
	_, err = e.app.AssignWorkOrder(e.ctx, mid, wo, holder.UserID)
	e.must(err)
	e.must(e.app.ChangeWorkOrderStatus(e.ctx, holder, wo, domain.WOInProgress, ""))
	return wo
}

func calibration(wo string, standards []string, adjusted bool, points ...domain.CalPoint) domain.CalibrationRecorded {
	return domain.CalibrationRecorded{
		WorkOrderID: wo, Procedure: "ESA615 performance check v3", Temperature: "21.5", Humidity: "40",
		Adjusted: adjusted, Points: points, StandardsUsed: standards,
	}
}

func voltage(asFound, asLeft string) domain.CalPoint {
	return domain.CalPoint{Parameter: "Mains voltage", Unit: "V", Nominal: "120.0",
		Tolerance: domain.Tolerance{Kind: domain.TolPercent, Value: "2"}, AsFound: asFound, AsLeft: asLeft}
}

func leakage(asFound string) domain.CalPoint {
	return domain.CalPoint{Parameter: "Earth leakage", Unit: "µA", Nominal: "0",
		Tolerance: domain.Tolerance{Kind: domain.TolLimits, Lower: "0", Upper: "300"}, AsFound: asFound}
}

func (e *env) calRecord(id string) (found, left, nodeFound string) {
	e.t.Helper()
	e.must(e.st.DB().QueryRowContext(e.ctx, `SELECT as_found_result, as_left_result, node_as_found_result
		FROM calibration_records WHERE id = ?`, id).Scan(&found, &left, &nodeFound))
	return
}

func TestCalibrationRecordAndComplete(t *testing.T) {
	e := newEnv(t)
	c := e.calSetup()

	// Can't complete a calibration work order without a record.
	wantRejection(t, e.sign(c.tech, c.wo, domain.MeaningPerformed), domain.FlagInvalid)
	// The instrument can't be its own standard; ordinary assets aren't standards.
	_, err := e.app.RecordCalibration(e.ctx, c.tech, calibration(c.wo, []string{c.asset}, false, voltage("120.5", "")))
	wantRejection(t, err, domain.FlagInvalid)
	// Someone without the lease can't record.
	other := e.activeUser("otto", domain.RoleUser)
	if _, err := e.app.RecordCalibration(e.ctx, other, calibration(c.wo, nil, false, voltage("120.5", ""))); !errors.Is(err, ErrNotLeaseHolder) {
		t.Fatalf("non-holder: %v", err)
	}
	// Adjusted calibrations need as-left readings.
	_, err = e.app.RecordCalibration(e.ctx, c.tech, calibration(c.wo, nil, true, voltage("125.0", "")))
	if err == nil {
		t.Fatal("adjusted calibration without as-left accepted")
	}

	// 122.4 V is exactly the +2% limit of 120 V: a pass.
	id, err := e.app.RecordCalibration(e.ctx, c.tech, calibration(c.wo, []string{c.standard}, false, voltage("122.4", ""), leakage("150")))
	e.must(err)
	if f, l, _ := e.calRecord(id); f != "pass" || l != "pass" {
		t.Fatalf("results = %s/%s", f, l)
	}
	var lo, hi string
	e.must(e.st.DB().QueryRowContext(e.ctx, `SELECT lower_limit, upper_limit FROM cal_points WHERE record_id = ? AND idx = 0`, id).Scan(&lo, &hi))
	if lo != "117.6" || hi != "122.4" {
		t.Fatalf("limits = [%s, %s]", lo, hi)
	}
	var due string
	e.must(e.st.DB().QueryRowContext(e.ctx, `SELECT due_at_time_of_use FROM cal_standards WHERE record_id = ?`, id).Scan(&due))
	if due != "2027-03-01" {
		t.Fatalf("standard due snapshot = %q", due)
	}

	// Only one valid record per work order; voiding allows a re-record.
	_, err = e.app.RecordCalibration(e.ctx, c.tech, calibration(c.wo, nil, false, voltage("120.0", "")))
	wantRejection(t, err, domain.FlagConflict)
	e.must(e.app.VoidCalibration(e.ctx, c.tech, id, "wrong standard serial noted"))
	_, err = e.app.RecordCalibration(e.ctx, c.tech, calibration(c.wo, []string{c.standard}, false, voltage("120.1", "")))
	e.must(err)
	e.must(e.sign(c.tech, c.wo, domain.MeaningPerformed))
}

func TestFailedAsLeftRequiresOutOfService(t *testing.T) {
	e := newEnv(t)
	c := e.calSetup()
	// Adjusted, but still out of tolerance afterwards.
	_, err := e.app.RecordCalibration(e.ctx, c.tech, calibration(c.wo, []string{c.standard}, true, voltage("125.0", "123.0")))
	e.must(err)
	wantRejection(t, e.sign(c.tech, c.wo, domain.MeaningPerformed), domain.FlagInvalid)
	a, err := domain.GetAsset(e.ctx, e.st.DB(), c.asset)
	e.must(err)
	e.must(e.app.SetAssetStatus(e.ctx, c.tech, c.asset, a.Version, domain.AssetOutOfService, "failed calibration"))
	e.must(e.sign(c.tech, c.wo, domain.MeaningPerformed))
}

func TestCentralRecomputesPassFail(t *testing.T) {
	e := newEnv(t)
	c := e.calSetup()
	var lease string
	e.must(e.st.DB().QueryRowContext(e.ctx, `SELECT lease_id FROM work_orders WHERE id = ?`, c.wo).Scan(&lease))

	// A node (modified, or buggy) claims a failing reading passed.
	cal := calibration(c.wo, []string{c.standard}, false, voltage("130.0", ""))
	cal.Points[0].AsFoundPass, cal.Points[0].AsLeftPass = true, true
	cal.AsFoundResult, cal.AsLeftResult = "pass", "pass"
	r := e.remote()
	id := newID()
	ev := r.write(e, c.tech, domain.TypeCalibrationRecorded, domain.EntityCalRecord, id, 0, lease, cal)
	e.must(e.st.Ingest(e.ctx, ev))

	if got := e.flags(ev); !reflect.DeepEqual(got, []string{"recompute_mismatch/true"}) {
		t.Fatalf("flags = %v", got)
	}
	found, left, nodeFound := e.calRecord(id)
	if found != "fail" || left != "fail" || nodeFound != "pass" {
		t.Fatalf("results = %s/%s (node said %s)", found, left, nodeFound)
	}
	// Central's result, not the node's, gates completion.
	wantRejection(t, e.sign(c.tech, c.wo, domain.MeaningPerformed), domain.FlagInvalid)
}

func TestReferenceStandardChecks(t *testing.T) {
	e := newEnv(t)
	c := e.calSetup()
	overdue := e.must2(e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{
		Tag: "STD-OLD", LocationID: e.loc, Manufacturer: "Fluke", Model: "5500A", IsReferenceStandard: true,
	}))
	e.must2(e.app.CreateSchedule(e.ctx, c.mid, domain.PMScheduleCreated{
		AssetID: overdue, WOType: "calibration", Title: "Cal", IntervalDays: 365, FirstDue: "2026-09-01",
	}))
	untracked := e.must2(e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{
		Tag: "STD-NEW", LocationID: e.loc, Manufacturer: "Fluke", Model: "5500A", IsReferenceStandard: true,
	}))

	id, err := e.app.RecordCalibration(e.ctx, c.tech, calibration(c.wo, []string{overdue, untracked}, false, voltage("120.0", "")))
	e.must(err)
	var evID string
	e.must(e.st.DB().QueryRowContext(e.ctx, `SELECT last_event_id FROM calibration_records WHERE id = ?`, id).Scan(&evID))
	fs, err := e.st.Flags(e.ctx, evID)
	e.must(err)
	var got []string
	for _, f := range fs {
		got = append(got, f.Flag)
	}
	if !reflect.DeepEqual(got, []string{"standard_overdue", "standard_untracked"}) {
		t.Fatalf("flags = %v", got)
	}

	// With the blocking policy, an overdue standard is refused outright.
	e.must(e.app.VoidCalibration(e.ctx, c.tech, id, "redo"))
	e.app.Store.Close()
	e.reopen(&domain.Projector{LocalNodeID: e.app.Author.NodeID, BlockOverdueStandards: true})
	_, err = e.app.RecordCalibration(e.ctx, c.tech, calibration(c.wo, []string{overdue}, false, voltage("120.0", "")))
	wantRejection(t, err, domain.FlagStandardOverdue)
}

func TestSchedulesGenerateWork(t *testing.T) {
	e := newEnv(t) // today is 2026-10-06
	mid, tech := e.activeUser("mona", domain.RoleMidTier), e.activeUser("tess", domain.RoleUser)
	a1, a2 := e.asset("A1"), e.asset("A2")
	_, err := e.app.CreateSchedule(e.ctx, tech, domain.PMScheduleCreated{AssetID: a1, WOType: "pm", Title: "PM", IntervalDays: 180, FirstDue: "2026-10-20"})
	wantRejection(t, err, domain.FlagNotAuthorized)
	s1 := e.must2(e.app.CreateSchedule(e.ctx, mid, domain.PMScheduleCreated{AssetID: a1, WOType: "pm", Title: "Semiannual PM", Procedure: "PM-7", IntervalDays: 180, GraceDays: 14, FirstDue: "2026-10-20"}))
	e.must2(e.app.CreateSchedule(e.ctx, mid, domain.PMScheduleCreated{AssetID: a2, WOType: "inspection", Title: "Far off", IntervalDays: 365, FirstDue: "2027-06-01"}))

	got, err := e.app.GenerateDueWorkOrders(e.ctx, WorkingSetWindow)
	e.must(err)
	if len(got) != 1 {
		t.Fatalf("generated %v, want one", got)
	}
	if again, err := e.app.GenerateDueWorkOrders(e.ctx, WorkingSetWindow); err != nil || len(again) != 0 {
		t.Fatalf("second run generated %v, %v", again, err)
	}
	s, err := domain.GetSchedule(e.ctx, e.st.DB(), s1)
	e.must(err)
	w, err := domain.GetWorkOrder(e.ctx, e.st.DB(), s.OpenWorkOrderID)
	e.must(err)
	if w.ScheduleID != s1 || w.DueAt != "2026-10-20" || w.OpenedBy != domain.SystemScheduler {
		t.Fatalf("generated work order = %+v", w)
	}

	// Completing it on 2026-10-18 schedules the next one 180 days later.
	_, err = e.app.AssignWorkOrder(e.ctx, mid, w.ID, tech.UserID)
	e.must(err)
	e.now = time.Date(2026, 10, 18, 15, 0, 0, 0, time.UTC)
	e.must(e.app.ChangeWorkOrderStatus(e.ctx, tech, w.ID, domain.WOInProgress, ""))
	e.must(e.sign(tech, w.ID, domain.MeaningPerformed))
	s, err = domain.GetSchedule(e.ctx, e.st.DB(), s1)
	e.must(err)
	if s.NextDue != "2027-04-16" || s.OpenWorkOrderID != "" {
		t.Fatalf("schedule after completion = %+v", s)
	}

	// Cancelling a generated work order frees the schedule to regenerate.
	e.must(e.app.ChangeSchedule(e.ctx, mid, s1, domain.PMScheduleChanged{NextDue: ptr("2026-10-25"), Reason: "pulled forward after repair"}))
	got, err = e.app.GenerateDueWorkOrders(e.ctx, WorkingSetWindow)
	e.must(err)
	s, _ = domain.GetSchedule(e.ctx, e.st.DB(), s1)
	e.must(e.app.ChangeWorkOrderStatus(e.ctx, mid, s.OpenWorkOrderID, domain.WOCancelled, "duplicate"))
	got2, err := e.app.GenerateDueWorkOrders(e.ctx, WorkingSetWindow)
	e.must(err)
	if len(got) != 1 || len(got2) != 1 || got[0] == got2[0] {
		t.Fatalf("regeneration: %v then %v", got, got2)
	}

	// Only this node or central may act as the scheduler.
	r := e.remote()
	ev := r.write(e, Scheduler, domain.TypeWorkOrderOpened, domain.EntityWorkOrder, newID(), 0, "",
		domain.WorkOrderOpened{Number: "X-1", Type: "pm", AssetID: a2, Priority: "normal", Title: "x"})
	e.must(e.st.Ingest(e.ctx, ev))
	if got := e.flags(ev); !reflect.DeepEqual(got, []string{"non_authorized/false"}) {
		t.Fatalf("remote scheduler flags = %v", got)
	}
}

func ptr[T any](v T) *T { return &v }

func TestInventoryLedger(t *testing.T) {
	e := newEnv(t)
	mid, tech, other := e.activeUser("mona", domain.RoleMidTier), e.activeUser("tess", domain.RoleUser), e.activeUser("otto", domain.RoleUser)
	part := e.must2(e.app.CreatePart(e.ctx, mid, domain.PartCreated{PartNo: "FUSE-2A", Description: "Fuse 2 A slow-blow", Unit: "each"}))
	shop := e.must2(e.app.CreateStockLocation(e.ctx, mid, domain.StockLocationCreated{SiteID: e.site, Name: "Biomed stockroom"}))
	kit := e.must2(e.app.CreateStockLocation(e.ctx, tech, domain.StockLocationCreated{SiteID: e.site, Name: "Tess's van", OwnerUserID: tech.UserID}))
	_, err := e.app.CreateStockLocation(e.ctx, tech, domain.StockLocationCreated{SiteID: e.site, Name: "Shared"})
	wantRejection(t, err, domain.FlagNotAuthorized)

	level := func(loc string) int64 {
		q, err := e.app.StockLevel(e.ctx, part, loc)
		e.must(err)
		return q
	}
	rec := func(a Actor, t domain.StockTxnRecorded) (string, error) {
		t.PartID = part
		return e.app.RecordStock(e.ctx, a, t)
	}

	e.must2(rec(tech, domain.StockTxnRecorded{Kind: domain.StockReceive, StockLocationID: shop, Quantity: 10}))
	issue := e.must2(rec(tech, domain.StockTxnRecorded{Kind: domain.StockIssue, StockLocationID: shop, Quantity: 3}))
	e.must2(rec(tech, domain.StockTxnRecorded{Kind: domain.StockTransfer, StockLocationID: shop, ToLocationID: kit, Quantity: 2}))
	if level(shop) != 5 || level(kit) != 2 {
		t.Fatalf("levels shop=%d kit=%d", level(shop), level(kit))
	}

	// Personal stock belongs to its owner.
	_, err = rec(other, domain.StockTxnRecorded{Kind: domain.StockIssue, StockLocationID: kit, Quantity: 1})
	wantRejection(t, err, domain.FlagNotAuthorized)
	_, err = rec(tech, domain.StockTxnRecorded{Kind: domain.StockAdjust, StockLocationID: shop, Quantity: -1, Reason: "damaged"})
	wantRejection(t, err, domain.FlagNotAuthorized)

	// A count computes its adjustment against the ledger when applied.
	cnt := e.must2(rec(tech, domain.StockTxnRecorded{Kind: domain.StockCount, StockLocationID: shop, ObservedQty: ptr[int64](4)}))
	var delta, computed int64
	e.must(e.st.DB().QueryRowContext(e.ctx, `SELECT delta, computed_qty FROM stock_txns WHERE txn_id = ?`, cnt).Scan(&delta, &computed))
	if delta != -1 || computed != 5 || level(shop) != 4 {
		t.Fatalf("count: delta %d computed %d level %d", delta, computed, level(shop))
	}

	// Reversal restores the level, once.
	e.must(e.app.ReverseStock(e.ctx, mid, issue, "issued to wrong work order"))
	wantRejection(t, e.app.ReverseStock(e.ctx, mid, issue, "again"), domain.FlagInvalid)
	if level(shop) != 7 {
		t.Fatalf("after reversal shop=%d", level(shop))
	}

	// Issuing more than the ledger holds is recorded but flagged.
	over := e.must2(rec(tech, domain.StockTxnRecorded{Kind: domain.StockIssue, StockLocationID: kit, Quantity: 5}))
	var evID string
	e.must(e.st.DB().QueryRowContext(e.ctx, `SELECT event_id FROM stock_txns WHERE txn_id = ?`, over).Scan(&evID))
	fs, _ := e.st.Flags(e.ctx, evID)
	if len(fs) != 1 || fs[0].Flag != domain.FlagNegativeStock || !fs[0].Projected || level(kit) != -3 {
		t.Fatalf("over-issue flags %+v level %d", fs, level(kit))
	}

	// Offline: counting a shared stockroom on an employee Pi is refused;
	// counting your own kit is fine.
	r := e.remote()
	shared := r.write(e, tech, domain.TypeStockTxnRecorded, domain.EntityStockTxn, newID(), 0, "",
		domain.StockTxnRecorded{Kind: domain.StockCount, PartID: part, StockLocationID: shop, ObservedQty: ptr[int64](7)})
	own := r.write(e, tech, domain.TypeStockTxnRecorded, domain.EntityStockTxn, newID(), 0, "",
		domain.StockTxnRecorded{Kind: domain.StockCount, PartID: part, StockLocationID: kit, ObservedQty: ptr[int64](0)})
	e.must(e.st.Ingest(e.ctx, shared))
	e.must(e.st.Ingest(e.ctx, own))
	if got := e.flags(shared); !reflect.DeepEqual(got, []string{"non_authorized/false"}) {
		t.Fatalf("remote shared count flags = %v", got)
	}
	if got := e.flags(own); len(got) != 0 || level(kit) != 0 {
		t.Fatalf("remote kit count flags = %v level %d", got, level(kit))
	}
}

func TestLockout(t *testing.T) {
	e := newEnv(t)
	tech := e.activeUser("tess", domain.RoleUser)
	const pw = "brass-kettle-orchard-7"
	fail := func(n int) {
		for i := 0; i < n; i++ {
			if _, err := e.app.Authenticate(e.ctx, "tess", "wrong-password"); !errors.Is(err, ErrBadCredentials) {
				t.Fatalf("failure %d: %v", i+1, err)
			}
		}
	}

	fail(4)
	_, err := e.app.Authenticate(e.ctx, "tess", pw)
	e.must(err) // success resets the count
	fail(4)
	_, err = e.app.Authenticate(e.ctx, "tess", pw)
	e.must(err)

	for round := 1; round <= 3; round++ {
		fail(5)
		if _, err := e.app.Authenticate(e.ctx, "tess", pw); !errors.Is(err, ErrLocked) {
			t.Fatalf("round %d: correct password while locked: %v", round, err)
		}
		e.now = e.now.Add(16 * time.Minute)
		_, err := e.app.Authenticate(e.ctx, "tess", pw)
		if round < 3 {
			e.must(err) // 15-minute lock has passed
		} else if !errors.Is(err, ErrLocked) {
			t.Fatalf("third lockout in 24h should hold the account: %v", err)
		}
	}
	wantRejection(t, e.app.UnlockUser(e.ctx, tech, tech.UserID, "self"), domain.FlagNotAuthorized)
	e.must(e.app.UnlockUser(e.ctx, e.super, tech.UserID, "verified by phone"))
	_, err = e.app.Authenticate(e.ctx, "tess", pw)
	e.must(err)

	// Lockouts recorded on any employee Pi are accepted.
	r := e.remote()
	ev := r.write(e, auth, domain.TypeUserLocked, domain.EntityUser, tech.UserID, 0, "", domain.UserLocked{FailedAttempts: 5, Reason: "offline"})
	e.must(e.st.Ingest(e.ctx, ev))
	if got := e.flags(ev); len(got) != 0 {
		t.Fatalf("remote lockout flags = %v", got)
	}
	if _, err := e.app.Authenticate(e.ctx, "tess", pw); !errors.Is(err, ErrLocked) {
		t.Fatalf("after remote lockout: %v", err)
	}
}
