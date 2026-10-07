package app

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"pi-fleet/internal/domain"
)

func TestMeterReadingsAndTotals(t *testing.T) {
	e := newEnv(t)
	tech, mid := e.activeUser("tess", domain.RoleUser), e.activeUser("mona", domain.RoleMidTier)
	a := e.asset("VENT-1")
	at := func(h int) time.Time { return time.Date(2026, 10, 1, h, 0, 0, 0, time.UTC) }
	total := func() string {
		tot, _, err := domain.MeterTotal(e.ctx, e.st.DB(), a, "hours")
		e.must(err)
		return tot
	}

	e.must2(e.app.RecordMeter(e.ctx, tech, a, "hours", "100", at(1), false))
	e.must2(e.app.RecordMeter(e.ctx, tech, a, "hours", "150.5", at(2), false))
	e.must2(e.app.RecordMeter(e.ctx, tech, a, "hours", "220", at(4), false))
	if total() != "120" {
		t.Fatalf("total = %s, want 120", total())
	}
	// A lower reading needs the reset box.
	_, err := e.app.RecordMeter(e.ctx, tech, a, "hours", "12", at(5), false)
	wantRejection(t, err, domain.FlagInvalid)
	e.must2(e.app.RecordMeter(e.ctx, tech, a, "hours", "12", at(5), true)) // meter replaced
	if total() != "132" {
		t.Fatalf("total after replacement = %s, want 132", total())
	}

	// A backdated reading in between changes nothing overall...
	e.must2(e.app.RecordMeter(e.ctx, tech, a, "hours", "180", at(3), false))
	if total() != "132" {
		t.Fatalf("total after backdated reading = %s", total())
	}
	// ...but one higher than the later reading is flagged for review.
	odd := e.must2(e.app.RecordMeter(e.ctx, tech, a, "hours", "300", at(3).Add(30*time.Minute), false))
	var flag string
	e.st.DB().QueryRow(`SELECT flag FROM event_flags f JOIN events ev USING (event_id) WHERE ev.entity_id = ?`, odd).Scan(&flag)
	if flag != domain.FlagMeterInconsistent {
		t.Fatalf("flag = %q", flag)
	}
	// Voiding the typo restores the totals; only its author or mid-tier may.
	other := e.activeUser("otto", domain.RoleUser)
	wantRejection(t, e.app.VoidMeter(e.ctx, other, odd, "typo"), domain.FlagNotAuthorized)
	e.must(e.app.VoidMeter(e.ctx, mid, odd, "typo: meant 200"))
	if total() != "132" {
		t.Fatalf("total after void = %s", total())
	}
	_, err = e.app.RecordMeter(e.ctx, tech, a, "hours", "999", time.Now().Add(time.Hour), false)
	wantRejection(t, err, domain.FlagInvalid) // future
	_, err = e.app.RecordMeter(e.ctx, tech, a, "Run Hours", "1", at(6), false)
	wantRejection(t, err, domain.FlagInvalid) // bad meter name

	meters, err := domain.AssetMeters(e.ctx, e.st.DB(), a)
	e.must(err)
	if !reflect.DeepEqual(meters, []domain.Meter{{Name: "hours", Latest: "12", Total: "132", ReadAt: "2026-10-01T05:00:00Z"}}) {
		t.Fatalf("meters = %+v", meters)
	}
}

func TestUsageTriggeredSchedule(t *testing.T) {
	e := newEnv(t) // now is 2026-10-06 09:00
	mid, tech := e.activeUser("mona", domain.RoleMidTier), e.activeUser("tess", domain.RoleUser)
	a := e.asset("VENT-1")
	e.must2(e.app.RecordMeter(e.ctx, tech, a, "hours", "1000", e.now.Add(-time.Hour), false))

	_, err := e.app.CreateSchedule(e.ctx, mid, domain.PMScheduleCreated{AssetID: a, WOType: "pm", Title: "500-hour PM", IntervalDays: 365,
		FirstDue: "2027-09-01", MeterInterval: "500"})
	wantRejection(t, err, domain.FlagInvalid) // interval without a meter
	sched := e.must2(e.app.CreateSchedule(e.ctx, mid, domain.PMScheduleCreated{AssetID: a, WOType: "pm", Title: "500-hour PM", IntervalDays: 365,
		FirstDue: "2027-09-01", Meter: "hours", MeterInterval: "500"}))
	s, _ := domain.GetSchedule(e.ctx, e.st.DB(), sched)
	if s.MeterBaseline != "0" || s.MeterLead != "50" { // usage counts from the first reading
		t.Fatalf("baseline %s lead %s", s.MeterBaseline, s.MeterLead)
	}
	generate := func() []string {
		got, err := e.app.GenerateDueWorkOrders(e.ctx, WorkingSetWindow)
		e.must(err)
		return got
	}

	e.must2(e.app.RecordMeter(e.ctx, tech, a, "hours", "1400", e.now, false))
	if got := generate(); len(got) != 0 {
		t.Fatalf("generated at 400 of 500 hours: %v", got)
	}
	e.must2(e.app.RecordMeter(e.ctx, tech, a, "hours", "1451.5", e.now, false))
	if got := generate(); len(got) != 1 {
		t.Fatalf("not generated at 451.5 of 500 hours (lead 50): %v", got)
	}
	s, _ = domain.GetSchedule(e.ctx, e.st.DB(), sched)
	w, _ := domain.GetWorkOrder(e.ctx, e.st.DB(), s.OpenWorkOrderID)
	if w.DueAt != "2026-10-06" || !strings.Contains(w.Problem, "451.5 of 500 hours") {
		t.Fatalf("work order due %s, problem %q", w.DueAt, w.Problem)
	}
	if got := generate(); len(got) != 0 {
		t.Fatal("generated twice while one is outstanding")
	}

	// Completing it restarts the usage count from the meter's total then.
	_, err = e.app.AssignWorkOrder(e.ctx, mid, w.ID, tech.UserID)
	e.must(err)
	e.must(e.app.ChangeWorkOrderStatus(e.ctx, tech, w.ID, domain.WOInProgress, ""))
	e.must2(e.app.RecordMeter(e.ctx, tech, a, "hours", "1510", e.now, false))
	e.must(e.sign(tech, w.ID, domain.MeaningPerformed))
	s, _ = domain.GetSchedule(e.ctx, e.st.DB(), sched)
	m, err := domain.ScheduleMeterStatus(e.ctx, e.st.DB(), s)
	e.must(err)
	if s.MeterBaseline != "510" || m.Used != "0" || m.Due || s.NextDue != "2027-10-06" {
		t.Fatalf("after completion: baseline %s used %s due %v next %s", s.MeterBaseline, m.Used, m.Due, s.NextDue)
	}
	if got := generate(); len(got) != 0 {
		t.Fatal("generated right after completion")
	}

	// The calendar still acts as a backstop: whichever comes first.
	e.now = time.Date(2027, 9, 20, 9, 0, 0, 0, time.UTC)
	if got := generate(); len(got) != 1 {
		t.Fatalf("calendar backstop: %v", got)
	}

	// Removing the usage trigger keeps the calendar one.
	e.must(e.app.ChangeSchedule(e.ctx, mid, sched, domain.PMScheduleChanged{Meter: ptr(""), Reason: "meter removed from device"}))
	s, _ = domain.GetSchedule(e.ctx, e.st.DB(), sched)
	if s.Meter != "" || s.MeterInterval != "" {
		t.Fatalf("meter not removed: %+v", s)
	}
}
