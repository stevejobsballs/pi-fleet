package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"pi-fleet/internal/domain"
)

// The work order as a job sheet: what the technician needs at a glance
// (where the equipment is and who to ask, its recent history), a step
// bar showing how far the work has got, the parts used, and before
// signing, what is still missing.

// woStep is one step of the job in the step bar.
type woStep struct {
	ID, Label, Badge string
	Done             bool // nothing left to do there
	Need             bool // something there stops completion
}

// readyItem is one line of "ready to finish?".
type readyItem struct {
	Text, Href string
	State      string // ok, todo (recommended) or need (stops completion)
}

// partUse is a part issued for, or put back from, the work order.
type partUse struct {
	TxnID, PartID, LocationID string
	PartNo, Description, Unit string
	Location, Kind            string // issue or return
	Qty                       int64
}

// partChoice is a part in stock that can be used.
type partChoice struct {
	Value, Label string // Value: part id|location id
}

// HoldReasons are the usual reasons for putting work on hold.
var HoldReasons = []string{"Waiting for parts", "Waiting for the vendor", "Equipment in use", "Needs another visit"}

func (s *Server) jobSheet(ctx context.Context, q domain.Querier, d *woData, sess *session) error {
	wo := d.WO
	// Where the equipment is, and who to ask there.
	var details string
	q.QueryRowContext(ctx, `SELECT s.path || ' › ' || l.name, l.details FROM locations l JOIN sites s ON s.id = l.site_id WHERE l.id = ?`,
		d.Asset.LocationID).Scan(&d.Place, &details)
	json.Unmarshal([]byte(details), &d.Where)
	// Its recent history: the last few other work orders on it.
	rows, err := q.QueryContext(ctx, `SELECT w.id, w.number, w.type, w.title, w.status, w.priority, w.due_at, '',
			coalesce((SELECT legal_name FROM users u WHERE u.id = w.assigned_to), ''), w.opened_at
		FROM work_orders w WHERE w.asset_id = ? AND w.id != ? ORDER BY w.opened_at DESC, w.number DESC LIMIT 5`, wo.AssetID, wo.ID)
	if d.History, err = scanAll(rows, err, func(r *sql.Rows) (woRow, error) {
		var x woRow
		return x, r.Scan(&x.ID, &x.Number, &x.Type, &x.Title, &x.Status, &x.Priority, &x.DueAt, &x.Asset, &x.AssignedTo, &x.OpenedAt)
	}); err != nil {
		return err
	}
	q.QueryRowContext(ctx, `SELECT count(*) FROM attachments WHERE target_type = ? AND target_id = ? AND status = 'attached'`,
		domain.EntityAsset, wo.AssetID).Scan(&d.AssetFiles)
	// Parts used.
	rows, err = q.QueryContext(ctx, `SELECT t.txn_id, t.part_id, t.location_id, p.part_no, p.description, p.unit, l.name, t.kind, abs(t.delta)
		FROM stock_txns t JOIN parts p ON p.id = t.part_id JOIN stock_locations l ON l.id = t.location_id
		WHERE t.wo_id = ? AND t.kind IN ('issue', 'return') AND t.reversed_by = '' ORDER BY t.event_id`, wo.ID)
	if d.Parts, err = scanAll(rows, err, func(r *sql.Rows) (partUse, error) {
		var p partUse
		return p, r.Scan(&p.TxnID, &p.PartID, &p.LocationID, &p.PartNo, &p.Description, &p.Unit, &p.Location, &p.Kind, &p.Qty)
	}); err != nil {
		return err
	}
	used := map[string]int64{}
	for _, p := range d.Parts {
		if p.Kind == domain.StockIssue {
			used[p.PartID] += p.Qty
		} else {
			used[p.PartID] -= p.Qty
		}
	}
	for _, n := range used {
		if n > 0 {
			d.PartsUsed++
		}
	}
	isMid := roleRank[sess.User.Role] >= roleRank[domain.RoleMidTier]
	if d.CanParts = d.EditWork || (isMid && wo.Status != domain.WOClosed && wo.Status != domain.WOCancelled); d.CanParts {
		rows, err := q.QueryContext(ctx, `SELECT sl.part_id || '|' || sl.location_id,
				p.part_no || ' · ' || p.description || ' — ' || sl.qty || ' ' || p.unit || ' in ' || l.name || coalesce(' (' || u.legal_name || ')', '')
			FROM stock_levels sl JOIN parts p ON p.id = sl.part_id JOIN stock_locations l ON l.id = sl.location_id
			LEFT JOIN users u ON u.id = l.owner_user_id AND l.owner_user_id != ''
			WHERE sl.qty > 0 ORDER BY l.owner_user_id != ?, p.part_no, l.name`, sess.User.ID)
		if d.PartChoices, err = scanAll(rows, err, func(r *sql.Rows) (partChoice, error) {
			var c partChoice
			return c, r.Scan(&c.Value, &c.Label)
		}); err != nil {
			return err
		}
	}
	d.HoldReasons = HoldReasons

	// The step bar.
	answered, total, missing := 0, 0, 0
	var missingIn []readyItem
	criticalFail := false
	for _, c := range d.Checklists {
		answered += c.Answered
		total += c.Total
		n := 0
		for _, st := range c.Steps {
			if domain.Inputless(st.Kind) || !st.Applies {
				continue
			}
			if st.Required && !st.Recorded {
				n++
			}
			if st.Critical && st.Recorded && st.Pass != nil && !*st.Pass {
				criticalFail = true
			}
		}
		if n > 0 {
			missing += n
			missingIn = append(missingIn, readyItem{State: "need", Href: "/work-orders/" + wo.ID + "/checklists/" + c.ID,
				Text: fmt.Sprintf("%s: %d required answer%s missing", c.Name, n, plural(n))})
		}
	}
	timeTotal := d.LabourTotal + d.TravelTotal
	files := 0
	for _, a := range d.Attachments {
		if a.Status == "attached" {
			files++
		}
	}
	clockHere := d.Clock != nil && d.Clock.Here
	d.Steps = []woStep{
		{ID: "checklist", Label: "Checklists", Badge: fmt.Sprintf("%d/%d", answered, total), Done: total > 0 && missing == 0, Need: missing > 0},
		{ID: "work-done", Label: "Work done", Done: wo.WorkDone != ""},
		{ID: "test-equipment", Label: "Test equipment", Badge: countBadge(len(d.TestEquipment)), Done: len(d.TestEquipment) > 0},
		{ID: "parts", Label: "Parts", Badge: countBadge(d.PartsUsed), Done: d.PartsUsed > 0},
		{ID: "time", Label: "Time", Badge: hhmmIf(timeTotal), Done: timeTotal > 0 || clockHere},
		{ID: "files", Label: "Files", Badge: countBadge(files), Done: files > 0},
		{ID: "finish", Label: "Finish", Done: wo.Status == domain.WOCompleted || wo.Status == domain.WOReviewed || wo.Status == domain.WOClosed},
	}
	if total == 0 {
		d.Steps[0].Badge = ""
	}

	// Ready to finish? (for the person doing the work, before signing)
	if d.SignAs != domain.MeaningPerformed {
		return nil
	}
	switch {
	case missingIn != nil:
		d.Ready = append(d.Ready, missingIn...)
	case total > 0:
		d.Ready = append(d.Ready, readyItem{State: "ok", Text: "Every required checklist answer is recorded", Href: "#checklist"})
	default:
		d.Ready = append(d.Ready, readyItem{State: "ok", Text: "No checklist on this work", Href: "#checklist"})
	}
	if criticalFail && d.Asset.Status == domain.AssetInService {
		d.Ready = append(d.Ready, readyItem{State: "need", Href: "#equipment-status",
			Text: "A critical checklist step failed: take the equipment out of service below"})
	}
	if wo.Type == "calibration" && !d.CalPoints && len(d.Calibrations) == 0 {
		d.Ready = append(d.Ready, readyItem{State: "need", Href: "#checklist", Text: "Calibration work needs readings: add a checklist with calibration points"})
	}
	if wo.WorkDone == "" {
		d.Ready = append(d.Ready, readyItem{State: "todo", Href: "#work-done", Text: "Describe the work done"})
	} else {
		d.Ready = append(d.Ready, readyItem{State: "ok", Href: "#work-done", Text: "Work done described"})
	}
	switch {
	case clockHere:
		d.Ready = append(d.Ready, readyItem{State: "ok", Href: "#time", Text: "Your clock is running: signing stops it and records the time"})
	case timeTotal > 0:
		d.Ready = append(d.Ready, readyItem{State: "ok", Href: "#time", Text: "Time recorded: " + hhmm(timeTotal)})
	default:
		d.Ready = append(d.Ready, readyItem{State: "todo", Href: "#time", Text: "Record your time"})
	}
	readings := false
	for _, c := range d.Checklists {
		for _, st := range c.Steps {
			readings = readings || st.Kind == domain.StepCalibration || st.Kind == domain.StepNumber
		}
	}
	if readings && len(d.TestEquipment) == 0 {
		d.Ready = append(d.Ready, readyItem{State: "todo", Href: "#test-equipment", Text: "List the test equipment used for the readings"})
	}
	for _, r := range d.Ready {
		d.NotReady = d.NotReady || r.State == "need"
		d.Todo = d.Todo || r.State == "todo"
	}
	return nil
}

func countBadge(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

func hhmmIf(m int) string {
	if m == 0 {
		return ""
	}
	return hhmm(m)
}

// partsUse issues a part from stock for the work order.
func (s *Server) partsUse(w http.ResponseWriter, r *http.Request, sess *session) error {
	id := r.PathValue("id")
	back := "/work-orders/" + id + "#parts"
	part, loc, _ := strings.Cut(r.PostFormValue("part"), "|")
	n, err := strconv.ParseInt(strings.TrimSpace(r.PostFormValue("quantity")), 10, 64)
	if err != nil || n < 1 {
		return s.done(w, r, sess, back, "Not saved: the quantity must be a whole number, 1 or more.")
	}
	if _, err := s.App.RecordStock(r.Context(), s.actor(sess), domain.StockTxnRecorded{Kind: domain.StockIssue, PartID: part,
		StockLocationID: loc, Quantity: n, WorkOrderID: id}); err != nil {
		return s.failed(w, r, sess, back, err)
	}
	return s.done(w, r, sess, back, "Part added: taken from stock.")
}

// partsReturn puts a part back into stock (used by mistake, or not
// needed after all).
func (s *Server) partsReturn(w http.ResponseWriter, r *http.Request, sess *session) error {
	id := r.PathValue("id")
	back := "/work-orders/" + id + "#parts"
	n, err := strconv.ParseInt(r.PostFormValue("quantity"), 10, 64)
	if err != nil || n < 1 {
		return s.done(w, r, sess, back, "Not saved: the quantity must be a whole number, 1 or more.")
	}
	if _, err := s.App.RecordStock(r.Context(), s.actor(sess), domain.StockTxnRecorded{Kind: domain.StockReturn, PartID: r.PostFormValue("part"),
		StockLocationID: r.PostFormValue("location"), Quantity: n, WorkOrderID: id, Reason: "put back from the work order"}); err != nil {
		return s.failed(w, r, sess, back, err)
	}
	return s.done(w, r, sess, back, "Put back into stock.")
}
