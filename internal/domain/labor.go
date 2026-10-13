package domain

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"pi-fleet/internal/store"
)

// Time on a work order is labour or travel, typed in (labor.logged) or
// recorded with the clock: Start work (labor.clock_started) runs a clock
// for the person until Stop work (labor.clock_stopped) turns it into a
// labour entry with the same id. Each person has at most one clock
// running. Entries can be corrected (labor.corrected) or reversed.
const (
	TypeLaborClockStarted = "labor.clock_started"
	TypeLaborClockStopped = "labor.clock_stopped"
	TypeLaborCorrected    = "labor.corrected"

	LaborKindLabor  = "labor"
	LaborKindTravel = "travel"

	// MaxLaborMinutes is the most one entry can hold: a day.
	MaxLaborMinutes = 24 * 60
)

// LaborKinds are the kinds of time, in the order offered.
var LaborKinds = []string{LaborKindLabor, LaborKindTravel}

type LaborClockStarted struct {
	WorkOrderID string `json:"work_order_id"`
	Kind        string `json:"kind"`
}

// LaborClockStopped ends the clock. Date is the day it started, in the
// work order's site's time zone; the time is from the two events' clocks.
type LaborClockStopped struct {
	Date string `json:"date"`
	Note string `json:"note,omitempty"`
}

// LaborCorrected replaces an entry's time, day, kind and note.
type LaborCorrected struct {
	Minutes int    `json:"minutes"`
	Date    string `json:"date"`
	Kind    string `json:"kind"`
	Note    string `json:"note,omitempty"`
}

func init() {
	for typ, f := range map[string]func() any{
		TypeLaborClockStarted: func() any { return &LaborClockStarted{} },
		TypeLaborClockStopped: func() any { return &LaborClockStopped{} },
		TypeLaborCorrected:    func() any { return &LaborCorrected{} },
	} {
		payloadTypes[typ] = struct {
			entity string
			new    func() any
		}{EntityLabor, f}
	}
}

func validKind(k string) bool { return k == LaborKindLabor || k == LaborKindTravel }

// ClockMinutes is the time a clock recorded, to the nearest minute, at
// most a day.
func ClockMinutes(started, stopped time.Time) int {
	m := int(stopped.Sub(started).Round(time.Minute) / time.Minute)
	return min(max(m, 0), MaxLaborMinutes)
}

func (ap *applier) laborClockStarted(p *LaborClockStarted) error {
	if err := ap.require(RoleUser); err != nil {
		return err
	}
	if err := ap.newEntity("labor_entries"); err != nil {
		return err
	}
	if ok, err := ap.exists(`SELECT 1 FROM labor_clocks WHERE id = ?`, ap.e.EntityID); err != nil || ok {
		return orConflict(err, "clock %s already exists", ap.e.EntityID)
	}
	if !validKind(p.Kind) {
		return invalid("kind must be labor or travel")
	}
	w, err := ap.workOrderFor(p.WorkOrderID)
	if err != nil {
		return err
	}
	if w.Status == WOCancelled || w.Status == WOClosed {
		return invalid("%s is %s", w.Number, w.Status)
	}
	if ok, err := ap.exists(`SELECT 1 FROM labor_clocks WHERE user_id = ?`, ap.actor.id); err != nil || ok {
		return orInvalid(err, "your clock is already running: stop it first")
	}
	return ap.exec(`INSERT INTO labor_clocks (id, wo_id, user_id, kind, started_at, event_id) VALUES (?, ?, ?, ?, ?, ?)`,
		ap.e.EntityID, w.ID, ap.actor.id, p.Kind, ap.e.WallTime.UTC().Format(time.RFC3339Nano), ap.e.EventID)
}

func (ap *applier) laborClockStopped(p *LaborClockStopped) error {
	var c LaborClock
	var started string
	err := ap.tx.QueryRowContext(ap.ctx, `SELECT wo_id, user_id, kind, started_at FROM labor_clocks WHERE id = ?`, ap.e.EntityID).
		Scan(&c.WorkOrderID, &c.UserID, &c.Kind, &started)
	if errors.Is(err, sql.ErrNoRows) {
		return invalid("that clock isn't running")
	}
	if err != nil {
		return err
	}
	if c.UserID != ap.actor.id && !ap.actor.atLeast(RoleMidTier) {
		return store.Reject(FlagNotAuthorized, "only the person whose clock it is, or a mid-tier user, can stop it")
	}
	if _, err := time.Parse(dateLayout, p.Date); err != nil {
		return invalid("date %q must be YYYY-MM-DD", p.Date)
	}
	if err := ap.exec(`DELETE FROM labor_clocks WHERE id = ?`, ap.e.EntityID); err != nil {
		return err
	}
	from, _ := time.Parse(time.RFC3339Nano, started)
	minutes := ClockMinutes(from, ap.e.WallTime)
	w, err := ap.workOrderFor(c.WorkOrderID)
	if err != nil {
		return err
	}
	// Under a minute, or on a cancelled work order: nothing to record.
	if minutes < 1 || w.Status == WOCancelled {
		return nil
	}
	return ap.exec(`INSERT INTO labor_entries (id, wo_id, user_id, minutes, work_date, note, status, reverse_reason, last_event_id, kind, started_at, ended_at)
		VALUES (?, ?, ?, ?, ?, ?, 'logged', '', ?, ?, ?, ?)`, ap.e.EntityID, c.WorkOrderID, c.UserID, minutes, p.Date, p.Note, ap.e.EventID,
		c.Kind, from.UTC().Format(time.RFC3339), ap.e.WallTime.UTC().Format(time.RFC3339))
}

func (ap *applier) laborCorrected(p *LaborCorrected) error {
	var userID, status, woID string
	var minutes int
	err := ap.tx.QueryRowContext(ap.ctx, `SELECT user_id, status, wo_id, minutes FROM labor_entries WHERE id = ?`, ap.e.EntityID).
		Scan(&userID, &status, &woID, &minutes)
	if errors.Is(err, sql.ErrNoRows) {
		return invalid("labour entry %s not found", ap.e.EntityID)
	}
	if err != nil {
		return err
	}
	if userID != ap.actor.id && !ap.actor.atLeast(RoleMidTier) {
		return store.Reject(FlagNotAuthorized, "only the person who recorded the time, or a mid-tier user, can change it")
	}
	if status != "logged" {
		return invalid("reversed time can't be changed")
	}
	if !validKind(p.Kind) {
		return invalid("kind must be labor or travel")
	}
	if p.Minutes < 1 || p.Minutes > MaxLaborMinutes {
		return invalid("minutes must be between 1 and 1440")
	}
	d, err := time.Parse(dateLayout, p.Date)
	if err != nil {
		return invalid("date %q must be YYYY-MM-DD", p.Date)
	}
	if d.After(ap.e.WallTime.Add(24 * time.Hour)) {
		return invalid("time can't be logged for a future date")
	}
	// A changed length no longer matches the clock's start and end.
	times := ""
	if p.Minutes != minutes {
		times = ", started_at = '', ended_at = ''"
	}
	return ap.exec(`UPDATE labor_entries SET minutes = ?, work_date = ?, kind = ?, note = ?, last_event_id = ?`+times+` WHERE id = ?`,
		p.Minutes, p.Date, p.Kind, p.Note, ap.e.EventID, ap.e.EntityID)
}

// LaborClock is someone's running clock.
type LaborClock struct {
	ID, WorkOrderID, Number, UserID, Kind string
	Name                                  string // the person's name (WorkOrderClocks)
	Started                               time.Time
}

// RunningClock returns the person's running clock, if any.
func RunningClock(ctx context.Context, q Querier, userID string) (LaborClock, bool, error) {
	c := LaborClock{UserID: userID}
	var started string
	err := q.QueryRowContext(ctx, `SELECT c.id, c.wo_id, coalesce(w.number, ''), c.kind, c.started_at FROM labor_clocks c
		LEFT JOIN work_orders w ON w.id = c.wo_id WHERE c.user_id = ?`, userID).Scan(&c.ID, &c.WorkOrderID, &c.Number, &c.Kind, &started)
	if errors.Is(err, sql.ErrNoRows) {
		return c, false, nil
	}
	if err != nil {
		return c, false, err
	}
	c.Started, _ = time.Parse(time.RFC3339Nano, started)
	return c, true, nil
}

// WorkOrderClocks are the clocks running on a work order.
func WorkOrderClocks(ctx context.Context, q Querier, woID string) ([]LaborClock, error) {
	rows, err := q.QueryContext(ctx, `SELECT c.id, c.user_id, coalesce(u.legal_name, c.user_id), c.kind, c.started_at FROM labor_clocks c
		LEFT JOIN users u ON u.id = c.user_id WHERE c.wo_id = ? ORDER BY c.started_at`, woID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LaborClock
	for rows.Next() {
		c := LaborClock{WorkOrderID: woID}
		var started string
		if err := rows.Scan(&c.ID, &c.UserID, &c.Name, &c.Kind, &started); err != nil {
			return nil, err
		}
		c.Started, _ = time.Parse(time.RFC3339Nano, started)
		out = append(out, c)
	}
	return out, rows.Err()
}
