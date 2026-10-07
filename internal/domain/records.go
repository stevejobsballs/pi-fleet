package domain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"

	"pi-fleet/internal/store"
)

// Warning flags: the event is applied but marked for review.
const (
	FlagRecomputeMismatch = "recompute_mismatch"
	FlagStandardOverdue   = "standard_overdue"
	FlagStandardUntracked = "standard_untracked"
	FlagNegativeStock     = "negative_stock"
)

const (
	// lockDuration is how long an account stays locked after five
	// failed logins (DESIGN.md §6.5).
	lockDuration = 15 * time.Minute
	// lockoutsBeforeHold is how many lockouts within 24 hours lock the
	// account until a super user unlocks it.
	lockoutsBeforeHold = 3
)

// lockedIndefinitely is the locked_until value for an account held for a
// super user to unlock.
var lockedIndefinitely = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)

var sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (ap *applier) warn(flag, format string, args ...any) error {
	return store.Flag(ap.ctx, ap.tx, ap.e.EventID, flag, fmt.Sprintf(format, args...), true)
}

// localDate is the event's date in a site's time zone.
func (ap *applier) localDate(siteID string) (time.Time, error) {
	var tz string
	if err := ap.tx.QueryRowContext(ap.ctx, `SELECT timezone FROM sites WHERE id = ?`, siteID).Scan(&tz); err != nil {
		return time.Time{}, err
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.Time{}, err
	}
	y, m, d := ap.e.WallTime.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC), nil
}

const dateLayout = "2006-01-02"

// --- lockout ---

func (ap *applier) userLocked(p *UserLocked) error {
	if ap.actor.id != SystemAuth {
		return store.Reject(FlagNotAuthorized, "accounts are locked only by the login system")
	}
	if _, err := ap.targetUser(); err != nil {
		return err
	}
	if p.FailedAttempts < 1 {
		return invalid("failed_attempts must be positive")
	}
	if err := ap.exec(`INSERT INTO user_lockouts (user_id, event_id, at) VALUES (?, ?, ?)`, ap.e.EntityID, ap.e.EventID, ap.wall()); err != nil {
		return err
	}
	since := ap.e.WallTime.Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	n, err := ap.count(`SELECT count(*) FROM user_lockouts WHERE user_id = ? AND at > ?`, ap.e.EntityID, since)
	if err != nil {
		return err
	}
	until := ap.e.WallTime.Add(lockDuration)
	if n >= lockoutsBeforeHold {
		until = lockedIndefinitely
	}
	return ap.bumpUser(`locked_until = ?`, until.UTC().Format(time.RFC3339))
}

func (ap *applier) userUnlocked(p *UserUnlocked) error {
	if err := ap.require(RoleSuperUser); err != nil {
		return err
	}
	if _, err := ap.targetUser(); err != nil {
		return err
	}
	if blank(p.Reason) {
		return invalid("unlocking requires a reason")
	}
	return ap.bumpUser(`locked_until = ''`)
}

// --- calibrations ---

func (ap *applier) workOrderFor(id string) (WorkOrder, error) {
	w, err := GetWorkOrder(ap.ctx, ap.tx, id)
	if errors.Is(err, ErrNotFound) {
		return WorkOrder{}, invalid("work order %s not found", id)
	}
	return w, err
}

func (ap *applier) calibrationRecorded(p *CalibrationRecorded) error {
	if err := ap.require(RoleUser); err != nil {
		return err
	}
	if err := ap.newEntity("calibration_records"); err != nil {
		return err
	}
	w, err := ap.workOrderFor(p.WorkOrderID)
	if err != nil {
		return err
	}
	if w.Type != "calibration" {
		return invalid("work order %s is a %s work order, not a calibration", w.Number, w.Type)
	}
	if w.Status != WOInProgress {
		return invalid("work order %s is %s; calibrations are recorded while in progress", w.Number, w.Status)
	}
	if err := ap.checkLease(w); err != nil {
		return err
	}
	if ok, err := ap.exists(`SELECT 1 FROM calibration_records WHERE wo_id = ? AND status = 'valid'`, w.ID); err != nil || ok {
		return orConflict(err, "work order %s already has a valid calibration record; void it first", w.Number)
	}
	if blank(p.Procedure) {
		return invalid("procedure is required")
	}
	for name, v := range map[string]string{"temperature": p.Temperature, "humidity": p.Humidity} {
		if v != "" {
			if _, err := parseDecimal(v); err != nil {
				return invalid("%s: %v", name, err)
			}
		}
	}
	if p.CertificateSHA256 != "" && !sha256RE.MatchString(p.CertificateSHA256) {
		return invalid("certificate_sha256 must be 64 lowercase hex characters")
	}
	for _, r := range []string{p.AsFoundResult, p.AsLeftResult} {
		if r != "pass" && r != "fail" {
			return invalid("results must be pass or fail, not %q", r)
		}
	}

	// Recompute every point independently of the node (DESIGN.md T2).
	pts, foundResult, leftResult, err := evaluateCalibration(p)
	if err != nil {
		return invalid("%v", err)
	}
	var mismatches []string
	for i, c := range pts {
		if p.Points[i].AsFoundPass != c.foundPass || p.Points[i].AsLeftPass != c.leftPass {
			mismatches = append(mismatches, fmt.Sprintf("point %d", i+1))
		}
	}
	if p.AsFoundResult != foundResult || p.AsLeftResult != leftResult {
		mismatches = append(mismatches, "overall result")
	}

	asset, err := GetAsset(ap.ctx, ap.tx, w.AssetID)
	if err != nil {
		return err
	}
	today, err := ap.localDate(asset.SiteID)
	if err != nil {
		return err
	}
	type standard struct{ id, due string }
	var standards []standard
	seen := map[string]bool{}
	for _, sid := range p.StandardsUsed {
		if seen[sid] {
			continue
		}
		seen[sid] = true
		if sid == w.AssetID {
			return invalid("an instrument cannot be its own reference standard")
		}
		s, err := GetAsset(ap.ctx, ap.tx, sid)
		if errors.Is(err, ErrNotFound) {
			return invalid("reference standard %s not found", sid)
		}
		if err != nil {
			return err
		}
		if !s.IsReferenceStandard || s.Status != AssetInService {
			return invalid("asset %s is not an in-service reference standard", s.Tag)
		}
		var due sql.NullString
		if err := ap.tx.QueryRowContext(ap.ctx, `SELECT min(next_due) FROM pm_schedules
			WHERE asset_id = ? AND wo_type = 'calibration' AND status = 'active'`, sid).Scan(&due); err != nil {
			return err
		}
		switch {
		case !due.Valid:
			if err := ap.warn(FlagStandardUntracked, "standard %s has no active calibration schedule", s.Tag); err != nil {
				return err
			}
		case due.String < today.Format(dateLayout):
			if ap.p.BlockOverdueStandards {
				return store.Reject(FlagStandardOverdue, "standard %s was due for calibration on %s", s.Tag, due.String)
			}
			if err := ap.warn(FlagStandardOverdue, "standard %s was due for calibration on %s", s.Tag, due.String); err != nil {
				return err
			}
		}
		standards = append(standards, standard{sid, due.String})
	}
	if len(mismatches) > 0 {
		if err := ap.warn(FlagRecomputeMismatch, "node's pass/fail differs from recomputation at %s", strings.Join(mismatches, ", ")); err != nil {
			return err
		}
	}

	if err := ap.exec(`INSERT INTO calibration_records (id, wo_id, asset_id, procedure, performed_by, performed_at,
			temperature, humidity, adjusted, as_found_result, as_left_result, node_as_found_result, node_as_left_result,
			certificate_sha256, status, void_reason, version, last_event_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'valid', '', 1, ?)`,
		ap.e.EntityID, w.ID, w.AssetID, p.Procedure, ap.actor.id, ap.wall(), p.Temperature, p.Humidity, p.Adjusted,
		foundResult, leftResult, p.AsFoundResult, p.AsLeftResult, p.CertificateSHA256, ap.e.EventID); err != nil {
		return err
	}
	for i, pt := range p.Points {
		tol, _ := json.Marshal(pt.Tolerance)
		c := pts[i]
		if err := ap.exec(`INSERT INTO cal_points (record_id, idx, parameter, unit, nominal, tolerance, lower_limit,
				upper_limit, as_found, as_left, as_found_pass, as_left_pass) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			ap.e.EntityID, i, pt.Parameter, pt.Unit, pt.Nominal, string(tol), c.lo, c.hi, pt.AsFound, c.asLeft,
			c.foundPass, c.leftPass); err != nil {
			return err
		}
	}
	for _, s := range standards {
		if err := ap.exec(`INSERT INTO cal_standards (record_id, standard_asset_id, due_at_time_of_use) VALUES (?, ?, ?)`,
			ap.e.EntityID, s.id, s.due); err != nil {
			return err
		}
	}
	return nil
}

type pointResult struct {
	lo, hi, asLeft      string
	foundPass, leftPass bool
}

// evaluateCalibration computes each point's acceptance limits and
// pass/fail with exact decimal arithmetic, and the overall results.
func evaluateCalibration(p *CalibrationRecorded) ([]pointResult, string, string, error) {
	if len(p.Points) == 0 || len(p.Points) > 500 {
		return nil, "", "", errors.New("a calibration needs between 1 and 500 points")
	}
	pts := make([]pointResult, len(p.Points))
	foundAll, leftAll := true, true
	for i, pt := range p.Points {
		if blank(pt.Parameter) || blank(pt.Unit) {
			return nil, "", "", fmt.Errorf("point %d: parameter and unit are required", i+1)
		}
		lo, hi, err := pt.Tolerance.Limits(pt.Nominal)
		if err != nil {
			return nil, "", "", fmt.Errorf("point %d: %w", i+1, err)
		}
		c := pointResult{lo: formatLimit(lo), hi: formatLimit(hi)}
		if c.foundPass, err = within(pt.AsFound, lo, hi); err != nil {
			return nil, "", "", fmt.Errorf("point %d as-found: %w", i+1, err)
		}
		switch {
		case p.Adjusted && pt.AsLeft == "":
			return nil, "", "", fmt.Errorf("point %d: an adjusted calibration needs an as-left reading", i+1)
		case !p.Adjusted && pt.AsLeft != "":
			return nil, "", "", fmt.Errorf("point %d: as-left given but the instrument was not adjusted", i+1)
		case p.Adjusted:
			c.asLeft = pt.AsLeft
			if c.leftPass, err = within(pt.AsLeft, lo, hi); err != nil {
				return nil, "", "", fmt.Errorf("point %d as-left: %w", i+1, err)
			}
		default:
			c.asLeft, c.leftPass = pt.AsFound, c.foundPass
		}
		foundAll = foundAll && c.foundPass
		leftAll = leftAll && c.leftPass
		pts[i] = c
	}
	return pts, passFail(foundAll), passFail(leftAll), nil
}

// ComputeResults fills in the pass/fail fields of p as the authoring
// node states them. Central recomputes them independently.
func ComputeResults(p *CalibrationRecorded) error {
	pts, found, left, err := evaluateCalibration(p)
	if err != nil {
		return err
	}
	for i := range p.Points {
		p.Points[i].AsFoundPass, p.Points[i].AsLeftPass = pts[i].foundPass, pts[i].leftPass
	}
	p.AsFoundResult, p.AsLeftResult = found, left
	return nil
}

func passFail(ok bool) string {
	if ok {
		return "pass"
	}
	return "fail"
}

func (ap *applier) calibrationVoided(p *CalibrationVoided) error {
	if blank(p.Reason) {
		return invalid("voiding a calibration requires a reason")
	}
	var woID, status string
	err := ap.tx.QueryRowContext(ap.ctx, `SELECT wo_id, status FROM calibration_records WHERE id = ?`, ap.e.EntityID).Scan(&woID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return invalid("calibration record %s not found", ap.e.EntityID)
	}
	if err != nil {
		return err
	}
	if status != "valid" {
		return invalid("calibration record is already %s", status)
	}
	w, err := ap.workOrderFor(woID)
	if err != nil {
		return err
	}
	if w.Status == WOClosed || w.Status == WOCancelled {
		return invalid("work order %s is %s", w.Number, w.Status)
	}
	if !ap.actor.atLeast(RoleMidTier) {
		if err := ap.checkLease(w); err != nil {
			return err
		}
	}
	return ap.exec(`UPDATE calibration_records SET status = 'voided', void_reason = ?, version = version + 1, last_event_id = ?
		WHERE id = ?`, p.Reason, ap.e.EventID, ap.e.EntityID)
}

// checkCompletion enforces what a work order needs before completion.
func (ap *applier) checkCompletion(w WorkOrder) error {
	if err := ap.checkChecklist(w); err != nil {
		return err
	}
	if w.Type != "calibration" {
		return nil
	}
	var leftResult string
	err := ap.tx.QueryRowContext(ap.ctx, `SELECT as_left_result FROM calibration_records WHERE wo_id = ? AND status = 'valid'`, w.ID).Scan(&leftResult)
	if errors.Is(err, sql.ErrNoRows) {
		return invalid("calibration work order %s needs a calibration record before completion", w.Number)
	}
	if err != nil {
		return err
	}
	if leftResult == "fail" {
		a, err := GetAsset(ap.ctx, ap.tx, w.AssetID)
		if err != nil {
			return err
		}
		if a.Status != AssetOutOfService && a.Status != AssetRetired {
			return invalid("as-left failed: take asset %s out of service before completing", a.Tag)
		}
	}
	return nil
}

// --- PM schedules ---

var scheduleTypes = map[string]bool{"pm": true, "calibration": true, "inspection": true}

func checkInterval(interval, grace int) error {
	if interval < 1 || interval > 3660 {
		return invalid("interval must be 1 to 3660 days")
	}
	if grace < 0 || grace > interval {
		return invalid("grace must be between 0 and the interval")
	}
	return nil
}

func (ap *applier) scheduleCreated(p *PMScheduleCreated) error {
	if err := ap.require(RoleMidTier); err != nil {
		return err
	}
	if err := ap.newEntity("pm_schedules"); err != nil {
		return err
	}
	a, err := GetAsset(ap.ctx, ap.tx, p.AssetID)
	if errors.Is(err, ErrNotFound) {
		return invalid("asset %s not found", p.AssetID)
	}
	if err != nil {
		return err
	}
	if a.Status == AssetRetired {
		return invalid("asset %s is retired", a.Tag)
	}
	if !scheduleTypes[p.WOType] {
		return invalid("schedules generate pm, calibration or inspection work orders, not %q", p.WOType)
	}
	if blank(p.Title) {
		return invalid("schedule title is required")
	}
	if err := checkInterval(p.IntervalDays, p.GraceDays); err != nil {
		return err
	}
	if _, err := time.Parse(dateLayout, p.FirstDue); err != nil {
		return invalid("first_due %q must be YYYY-MM-DD", p.FirstDue)
	}
	if p.ProcedureID != "" {
		if err := ap.activeProcedure(p.ProcedureID); err != nil {
			return err
		}
	}
	lead, baseline, err := ap.checkMeterTrigger(p.AssetID, p.Meter, p.MeterInterval, p.MeterLead)
	if err != nil {
		return err
	}
	return ap.exec(`INSERT INTO pm_schedules (id, asset_id, wo_type, title, procedure, interval_days, grace_days,
			next_due, status, open_wo_id, procedure_id, meter, meter_interval, meter_lead, meter_baseline, version, last_event_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'active', '', ?, ?, ?, ?, ?, 1, ?)`,
		ap.e.EntityID, p.AssetID, p.WOType, p.Title, p.Procedure, p.IntervalDays, p.GraceDays, p.FirstDue, p.ProcedureID,
		p.Meter, p.MeterInterval, lead, baseline, ap.e.EventID)
}

// Schedule is a projected PM schedule.
type Schedule struct {
	ID, AssetID, WOType, Title, Procedure          string
	IntervalDays, GraceDays                        int
	NextDue, Status, OpenWorkOrderID               string
	ProcedureID                                    string
	Meter, MeterInterval, MeterLead, MeterBaseline string
	Version                                        int64
}

// GetSchedule returns a schedule by id.
func GetSchedule(ctx context.Context, q Querier, id string) (Schedule, error) {
	var s Schedule
	err := q.QueryRowContext(ctx, `SELECT id, asset_id, wo_type, title, procedure, interval_days, grace_days,
		next_due, status, open_wo_id, procedure_id, meter, meter_interval, meter_lead, meter_baseline, version
		FROM pm_schedules WHERE id = ?`, id).
		Scan(&s.ID, &s.AssetID, &s.WOType, &s.Title, &s.Procedure, &s.IntervalDays, &s.GraceDays,
			&s.NextDue, &s.Status, &s.OpenWorkOrderID, &s.ProcedureID, &s.Meter, &s.MeterInterval, &s.MeterLead, &s.MeterBaseline, &s.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return Schedule{}, ErrNotFound
	}
	return s, err
}

func (ap *applier) activeSchedule(id string) (Schedule, error) {
	s, err := GetSchedule(ap.ctx, ap.tx, id)
	if errors.Is(err, ErrNotFound) {
		return Schedule{}, invalid("schedule %s not found", id)
	}
	if err == nil && s.Status != "active" {
		return Schedule{}, invalid("schedule %s has ended", id)
	}
	return s, err
}

func (ap *applier) bumpSchedule(id, set string, args ...any) error {
	args = append(args, ap.e.EventID, id)
	return ap.exec(`UPDATE pm_schedules SET `+set+`, version = version + 1, last_event_id = ? WHERE id = ?`, args...)
}

func (ap *applier) scheduleChanged(p *PMScheduleChanged) error {
	if err := ap.require(RoleMidTier); err != nil {
		return err
	}
	s, err := ap.activeSchedule(ap.e.EntityID)
	if err != nil {
		return err
	}
	if blank(p.Reason) {
		return invalid("changing a schedule requires a reason")
	}
	if p.Title != nil {
		if blank(*p.Title) {
			return invalid("schedule title cannot be blank")
		}
		s.Title = *p.Title
	}
	if p.Procedure != nil {
		s.Procedure = *p.Procedure
	}
	if p.IntervalDays != nil {
		s.IntervalDays = *p.IntervalDays
	}
	if p.GraceDays != nil {
		s.GraceDays = *p.GraceDays
	}
	if err := checkInterval(s.IntervalDays, s.GraceDays); err != nil {
		return err
	}
	if p.NextDue != nil {
		if _, err := time.Parse(dateLayout, *p.NextDue); err != nil {
			return invalid("next_due %q must be YYYY-MM-DD", *p.NextDue)
		}
		s.NextDue = *p.NextDue
	}
	if p.ProcedureID != nil {
		if *p.ProcedureID != "" {
			if err := ap.activeProcedure(*p.ProcedureID); err != nil {
				return err
			}
		}
		s.ProcedureID = *p.ProcedureID
	}
	if p.Meter != nil || p.MeterInterval != nil || p.MeterLead != nil {
		meter, interval, lead := s.Meter, s.MeterInterval, ""
		if p.Meter != nil {
			meter = *p.Meter
		}
		if p.MeterInterval != nil {
			interval = *p.MeterInterval
		}
		if p.MeterLead != nil {
			lead = *p.MeterLead
		}
		if meter == "" {
			interval, lead = "", "" // removing the usage trigger
		}
		newLead, baseline, err := ap.checkMeterTrigger(s.AssetID, meter, interval, lead)
		if err != nil {
			return err
		}
		if meter == s.Meter && meter != "" {
			baseline = s.MeterBaseline // same meter: keep counting from the last completion
		}
		s.Meter, s.MeterInterval, s.MeterLead, s.MeterBaseline = meter, interval, newLead, baseline
	}
	return ap.bumpSchedule(s.ID, `title = ?, procedure = ?, interval_days = ?, grace_days = ?, next_due = ?, procedure_id = ?,
		meter = ?, meter_interval = ?, meter_lead = ?, meter_baseline = ?`,
		s.Title, s.Procedure, s.IntervalDays, s.GraceDays, s.NextDue, s.ProcedureID, s.Meter, s.MeterInterval, s.MeterLead, s.MeterBaseline)
}

func (ap *applier) scheduleEnded(p *PMScheduleEnded) error {
	if err := ap.require(RoleMidTier); err != nil {
		return err
	}
	if _, err := ap.activeSchedule(ap.e.EntityID); err != nil {
		return err
	}
	if blank(p.Reason) {
		return invalid("ending a schedule requires a reason")
	}
	return ap.bumpSchedule(ap.e.EntityID, `status = 'ended'`)
}

// checkMeterTrigger validates a schedule's optional usage trigger and
// returns its lead (10% of the interval by default) and the starting
// baseline (the meter's current total).
func (ap *applier) checkMeterTrigger(assetID, meter, interval, lead string) (string, string, error) {
	if meter == "" {
		if interval != "" || lead != "" {
			return "", "", invalid("a usage interval needs a meter name")
		}
		return "", "", nil
	}
	if !meterNameRE.MatchString(meter) {
		return "", "", invalid("meter name %q must be lowercase letters, digits or _", meter)
	}
	iv, err := parseDecimal(interval)
	if err != nil || iv.Sign() <= 0 {
		return "", "", invalid("usage interval %q must be a positive decimal", interval)
	}
	l := new(big.Rat).Quo(iv, big.NewRat(10, 1))
	if lead != "" {
		if l, err = parseDecimal(lead); err != nil || l.Sign() < 0 || l.Cmp(iv) >= 0 {
			return "", "", invalid("usage lead must be at least 0 and less than the interval")
		}
	}
	baseline, _, err := MeterTotal(ap.ctx, ap.tx, assetID, meter)
	return formatLimit(l), baseline, err
}

// linkSchedule ties a newly opened work order to its schedule. A
// schedule has at most one outstanding work order at a time.
func (ap *applier) linkSchedule(p *WorkOrderOpened) error {
	s, err := ap.activeSchedule(p.ScheduleID)
	if err != nil {
		return err
	}
	if s.AssetID != p.AssetID || s.WOType != p.Type {
		return invalid("work order does not match schedule %s (asset or type)", s.ID)
	}
	if s.OpenWorkOrderID != "" {
		return store.Reject(FlagConflict, "schedule %s already has outstanding work order %s", s.ID, s.OpenWorkOrderID)
	}
	return ap.bumpSchedule(s.ID, `open_wo_id = ?`, ap.e.EntityID)
}

// scheduleFollowsWorkOrder advances a schedule when its work order is
// completed (next due = completion date + interval) and frees it when the
// work order is cancelled, so the scheduler generates a replacement.
func (ap *applier) scheduleFollowsWorkOrder(w WorkOrder, to string) error {
	if w.ScheduleID == "" {
		return nil
	}
	s, err := GetSchedule(ap.ctx, ap.tx, w.ScheduleID)
	if err != nil {
		return err
	}
	switch {
	case to == WOCompleted:
		a, err := GetAsset(ap.ctx, ap.tx, w.AssetID)
		if err != nil {
			return err
		}
		today, err := ap.localDate(a.SiteID)
		if err != nil {
			return err
		}
		next := today.AddDate(0, 0, s.IntervalDays).Format(dateLayout)
		baseline := s.MeterBaseline
		if s.Meter != "" {
			// Usage restarts from the meter's total at completion.
			if baseline, _, err = MeterTotal(ap.ctx, ap.tx, s.AssetID, s.Meter); err != nil {
				return err
			}
		}
		return ap.bumpSchedule(s.ID, `next_due = ?, open_wo_id = '', meter_baseline = ?`, next, baseline)
	case to == WOCancelled && s.OpenWorkOrderID == w.ID:
		return ap.bumpSchedule(s.ID, `open_wo_id = ''`)
	case to == WOInProgress && (w.Status == WOCompleted || w.Status == WOReviewed) && s.OpenWorkOrderID == "":
		// Reopened: it is outstanding again; completion recomputes next_due.
		return ap.bumpSchedule(s.ID, `open_wo_id = ?`, w.ID)
	}
	return nil
}

// --- inventory ---

func (ap *applier) partCreated(p *PartCreated) error {
	if err := ap.require(RoleMidTier); err != nil {
		return err
	}
	if err := ap.newEntity("parts"); err != nil {
		return err
	}
	if blank(p.PartNo) || blank(p.Description) || blank(p.Unit) {
		return invalid("part number, description and unit are required")
	}
	if ok, err := ap.exists(`SELECT 1 FROM parts WHERE part_no = ?`, p.PartNo); err != nil || ok {
		return orConflict(err, "part number %s already exists", p.PartNo)
	}
	return ap.exec(`INSERT INTO parts (id, part_no, description, unit, version, last_event_id) VALUES (?, ?, ?, ?, 1, ?)`,
		ap.e.EntityID, p.PartNo, p.Description, p.Unit, ap.e.EventID)
}

func (ap *applier) stockLocationCreated(p *StockLocationCreated) error {
	if p.OwnerUserID == "" || p.OwnerUserID != ap.actor.id {
		// Shared stockrooms, and kits made for someone else, need mid-tier.
		if err := ap.require(RoleMidTier); err != nil {
			return err
		}
	}
	if err := ap.newEntity("stock_locations"); err != nil {
		return err
	}
	if blank(p.Name) {
		return invalid("stock location name is required")
	}
	if ok, err := ap.exists(`SELECT 1 FROM sites WHERE id = ?`, p.SiteID); err != nil || !ok {
		return orInvalid(err, "site %s not found", p.SiteID)
	}
	if p.OwnerUserID != "" {
		if ok, err := ap.exists(`SELECT 1 FROM users WHERE id = ? AND status = ?`, p.OwnerUserID, UserStatusActive); err != nil || !ok {
			return orInvalid(err, "owner %s is not an active user", p.OwnerUserID)
		}
	}
	return ap.exec(`INSERT INTO stock_locations (id, site_id, name, owner_user_id, version, last_event_id) VALUES (?, ?, ?, ?, 1, ?)`,
		ap.e.EntityID, p.SiteID, p.Name, p.OwnerUserID, ap.e.EventID)
}

// stockLocation returns a location's owner ("" for shared) after checking
// the actor may use it: personal stock is touched only by its owner (or
// mid-tier and above).
func (ap *applier) stockLocation(id string) (string, error) {
	var owner string
	err := ap.tx.QueryRowContext(ap.ctx, `SELECT owner_user_id FROM stock_locations WHERE id = ?`, id).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", invalid("stock location %s not found", id)
	}
	if err != nil {
		return "", err
	}
	if owner != "" && owner != ap.actor.id && !ap.actor.atLeast(RoleMidTier) {
		return "", store.Reject(FlagNotAuthorized, "stock location %s belongs to another user", id)
	}
	return owner, nil
}

func (ap *applier) stockLevel(partID, locationID string) (int64, error) {
	var q int64
	err := ap.tx.QueryRowContext(ap.ctx, `SELECT qty FROM stock_levels WHERE part_id = ? AND location_id = ?`, partID, locationID).Scan(&q)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return q, err
}

// moveStock changes a level, flagging (not refusing) a negative result:
// the physical stock moved whether or not the ledger agreed.
func (ap *applier) moveStock(partID, locationID string, delta int64) error {
	if err := ap.exec(`INSERT INTO stock_levels (part_id, location_id, qty) VALUES (?, ?, ?)
		ON CONFLICT (part_id, location_id) DO UPDATE SET qty = qty + excluded.qty`, partID, locationID, delta); err != nil {
		return err
	}
	q, err := ap.stockLevel(partID, locationID)
	if err != nil {
		return err
	}
	if q < 0 {
		return ap.warn(FlagNegativeStock, "stock of part %s at %s is now %d", partID, locationID, q)
	}
	return nil
}

type stockRow struct {
	location    string
	delta       int64
	observed    *int64
	computedQty *int64
}

func (ap *applier) stockTxnRecorded(p *StockTxnRecorded) error {
	if err := ap.require(RoleUser); err != nil {
		return err
	}
	if err := ap.newEntityIn("stock_txns", "txn_id"); err != nil {
		return err
	}
	if ok, err := ap.exists(`SELECT 1 FROM parts WHERE id = ?`, p.PartID); err != nil || !ok {
		return orInvalid(err, "part %s not found", p.PartID)
	}
	owner, err := ap.stockLocation(p.StockLocationID)
	if err != nil {
		return err
	}
	if p.WorkOrderID != "" {
		if _, err := ap.workOrderFor(p.WorkOrderID); err != nil {
			return err
		}
	}
	if p.Kind != StockCount && p.ObservedQty != nil {
		return invalid("observed_qty is only for counts")
	}
	if p.Kind != StockTransfer && p.ToLocationID != "" {
		return invalid("to_location_id is only for transfers")
	}
	positive := func() error {
		if p.Quantity <= 0 {
			return invalid("%s quantity must be positive", p.Kind)
		}
		return nil
	}
	var rows []stockRow
	switch p.Kind {
	case StockReceive, StockReturn:
		if err := positive(); err != nil {
			return err
		}
		rows = []stockRow{{location: p.StockLocationID, delta: p.Quantity}}
	case StockIssue:
		if err := positive(); err != nil {
			return err
		}
		rows = []stockRow{{location: p.StockLocationID, delta: -p.Quantity}}
	case StockAdjust:
		if err := ap.require(RoleMidTier); err != nil {
			return err
		}
		if p.Quantity == 0 || blank(p.Reason) {
			return invalid("an adjustment needs a non-zero quantity and a reason")
		}
		rows = []stockRow{{location: p.StockLocationID, delta: p.Quantity}}
	case StockTransfer:
		if err := positive(); err != nil {
			return err
		}
		if p.ToLocationID == p.StockLocationID {
			return invalid("cannot transfer to the same location")
		}
		if _, err := ap.stockLocation(p.ToLocationID); err != nil {
			return err
		}
		rows = []stockRow{{location: p.StockLocationID, delta: -p.Quantity}, {location: p.ToLocationID, delta: p.Quantity}}
	case StockCount:
		if p.ObservedQty == nil || *p.ObservedQty < 0 || p.Quantity != 0 {
			return invalid("a count needs observed_qty ≥ 0 and no quantity")
		}
		if owner == "" && !ap.p.isCentral(ap.e) {
			// Shared stock is counted where the complete ledger lives, so
			// the adjustment is computed at one serialisation point.
			return store.Reject(FlagNotAuthorized, "shared stockroom counts must be made on the master Pi")
		}
		current, err := ap.stockLevel(p.PartID, p.StockLocationID)
		if err != nil {
			return err
		}
		rows = []stockRow{{location: p.StockLocationID, delta: *p.ObservedQty - current, observed: p.ObservedQty, computedQty: &current}}
	default:
		return invalid("unknown stock transaction kind %q", p.Kind)
	}
	for _, r := range rows {
		if err := ap.exec(`INSERT INTO stock_txns (txn_id, location_id, part_id, delta, kind, wo_id, reason,
				observed_qty, computed_qty, event_id, reversed_by) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '')`,
			ap.e.EntityID, r.location, p.PartID, r.delta, p.Kind, p.WorkOrderID, p.Reason, r.observed, r.computedQty, ap.e.EventID); err != nil {
			return err
		}
		if err := ap.moveStock(p.PartID, r.location, r.delta); err != nil {
			return err
		}
	}
	return nil
}

func (ap *applier) stockTxnReversed(p *StockTxnReversed) error {
	if err := ap.require(RoleMidTier); err != nil {
		return err
	}
	if blank(p.Reason) {
		return invalid("a reversal requires a reason")
	}
	rows, err := ap.tx.QueryContext(ap.ctx, `SELECT location_id, part_id, delta, reversed_by FROM stock_txns WHERE txn_id = ?`, ap.e.EntityID)
	if err != nil {
		return err
	}
	type row struct {
		loc, part, reversedBy string
		delta                 int64
	}
	var found []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.loc, &r.part, &r.delta, &r.reversedBy); err != nil {
			rows.Close()
			return err
		}
		found = append(found, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(found) == 0 {
		return invalid("stock transaction %s not found", ap.e.EntityID)
	}
	for _, r := range found {
		if r.reversedBy != "" {
			return invalid("stock transaction %s is already reversed", ap.e.EntityID)
		}
		if err := ap.moveStock(r.part, r.loc, -r.delta); err != nil {
			return err
		}
	}
	return ap.exec(`UPDATE stock_txns SET reversed_by = ? WHERE txn_id = ?`, ap.e.EventID, ap.e.EntityID)
}

// newEntityIn is newEntity for tables keyed by a column other than id.
func (ap *applier) newEntityIn(table, column string) error {
	return ap.newEntity("(SELECT " + column + " AS id FROM " + table + ")")
}
