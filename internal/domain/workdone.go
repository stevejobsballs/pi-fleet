package domain

import (
	"context"
	"strings"
	"time"
)

// What was done on a work order, and the test equipment used to verify
// readings. Both are recorded by the person doing the work, under the
// work order's lease while it is in progress, and are part of what a
// signature attests to.

const (
	TypeWorkOrderWorkDone             = "workorder.work_done_recorded"
	TypeWorkOrderTestEquipmentAdded   = "workorder.test_equipment_added"
	TypeWorkOrderTestEquipmentRemoved = "workorder.test_equipment_removed"
)

// WorkOrderWorkDone replaces the description of the work done.
type WorkOrderWorkDone struct {
	Text string `json:"text"`
}

// WorkOrderTestEquipment adds or removes a piece of test equipment (an
// in-service reference standard) used on the work order.
type WorkOrderTestEquipment struct {
	AssetID string `json:"asset_id"`
}

func init() {
	reg := func(typ string, f func() any) {
		payloadTypes[typ] = struct {
			entity string
			new    func() any
		}{EntityWorkOrder, f}
	}
	reg(TypeWorkOrderWorkDone, func() any { return &WorkOrderWorkDone{} })
	reg(TypeWorkOrderTestEquipmentAdded, func() any { return &WorkOrderTestEquipment{} })
	reg(TypeWorkOrderTestEquipmentRemoved, func() any { return &WorkOrderTestEquipment{} })
}

// workInProgress is the work order an event records work on: in
// progress, under the actor's lease.
func (ap *applier) workInProgress(what string) (WorkOrder, error) {
	w, err := ap.targetWorkOrder()
	if err != nil {
		return w, err
	}
	if w.Status != WOInProgress {
		return w, invalid("%s is recorded while %s is in progress", what, w.Number)
	}
	return w, ap.checkLease(w)
}

func (ap *applier) workOrderWorkDone(p *WorkOrderWorkDone) error {
	if _, err := ap.workInProgress("the work done"); err != nil {
		return err
	}
	text := strings.TrimSpace(p.Text)
	if len(text) > 8000 {
		return invalid("the description of the work done is too long (at most 8000 characters)")
	}
	return ap.bumpWorkOrder(`work_done = ?`, text)
}

func (ap *applier) workOrderTestEquipmentAdded(p *WorkOrderTestEquipment) error {
	w, err := ap.workInProgress("test equipment")
	if err != nil {
		return err
	}
	if ok, err := ap.exists(`SELECT 1 FROM wo_test_equipment WHERE wo_id = ? AND asset_id = ?`, w.ID, p.AssetID); err != nil || ok {
		return orInvalid(err, "that test equipment is already listed on %s", w.Number)
	}
	a, err := GetAsset(ap.ctx, ap.tx, w.AssetID)
	if err != nil {
		return err
	}
	today, err := ap.localDate(a.SiteID)
	if err != nil {
		return err
	}
	due, err := ap.checkStandard(p.AssetID, w.AssetID, today)
	if err != nil {
		return err
	}
	if err := ap.exec(`INSERT INTO wo_test_equipment (wo_id, asset_id, due_at_time_of_use, added_by, added_at, event_id)
		VALUES (?, ?, ?, ?, ?, ?)`, w.ID, p.AssetID, due, ap.actor.id, ap.wall(), ap.e.EventID); err != nil {
		return err
	}
	return ap.bumpWorkOrder(`work_done = work_done`)
}

func (ap *applier) workOrderTestEquipmentRemoved(p *WorkOrderTestEquipment) error {
	w, err := ap.workInProgress("test equipment")
	if err != nil {
		return err
	}
	if ok, err := ap.exists(`SELECT 1 FROM wo_test_equipment WHERE wo_id = ? AND asset_id = ?`, w.ID, p.AssetID); err != nil || !ok {
		return orInvalid(err, "that test equipment isn't listed on %s", w.Number)
	}
	if err := ap.exec(`DELETE FROM wo_test_equipment WHERE wo_id = ? AND asset_id = ?`, w.ID, p.AssetID); err != nil {
		return err
	}
	return ap.bumpWorkOrder(`work_done = work_done`)
}

// TestEquipmentUse is a piece of test equipment used on a work order.
type TestEquipmentUse struct {
	AssetID, Tag, Equipment, Serial string
	Due                             string // when its calibration was due then; "" if untracked
	AddedBy                         string
	AddedAt                         time.Time
}

// Overdue reports whether it was past its calibration date when used.
func (u TestEquipmentUse) Overdue() bool {
	return u.Due != "" && u.AddedAt.Format(dateLayout) > u.Due
}

// WorkOrderTestEquipmentUsed lists the test equipment used on a work order.
func WorkOrderTestEquipmentUsed(ctx context.Context, q Querier, woID string) ([]TestEquipmentUse, error) {
	rows, err := q.QueryContext(ctx, `SELECT t.asset_id, coalesce(a.tag, t.asset_id), coalesce(trim(a.manufacturer || ' ' || a.model), ''),
			coalesce(a.serial, ''), t.due_at_time_of_use, coalesce((SELECT legal_name FROM users u WHERE u.id = t.added_by), t.added_by), t.added_at
		FROM wo_test_equipment t LEFT JOIN assets a ON a.id = t.asset_id WHERE t.wo_id = ? ORDER BY 2`, woID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TestEquipmentUse
	for rows.Next() {
		var u TestEquipmentUse
		var at string
		if err := rows.Scan(&u.AssetID, &u.Tag, &u.Equipment, &u.Serial, &u.Due, &u.AddedBy, &at); err != nil {
			return nil, err
		}
		u.AddedAt, _ = time.Parse(time.RFC3339, at)
		out = append(out, u)
	}
	return out, rows.Err()
}
