package domain

import (
	"context"
	"time"

	"pi-fleet/internal/store"
)

// The department's acknowledgement that equipment was handed back after
// the work: the person there types their name on the technician's device.
// It isn't an electronic signature (they have no account); it records who
// said they received it, and the technician who recorded that.
const TypeWorkOrderHandedOver = "workorder.handed_over"

type WorkOrderHandedOver struct {
	Name string `json:"name"`
	Note string `json:"note,omitempty"`
}

func init() {
	payloadTypes[TypeWorkOrderHandedOver] = struct {
		entity string
		new    func() any
	}{EntityWorkOrder, func() any { return &WorkOrderHandedOver{} }}
}

func (ap *applier) workOrderHandedOver(p *WorkOrderHandedOver) error {
	w, err := ap.targetWorkOrder()
	if err != nil {
		return err
	}
	if w.AssignedTo != ap.actor.id && !ap.actor.atLeast(RoleMidTier) {
		return store.Reject(FlagNotAuthorized, "only the person doing the work, or a mid-tier user, can record the handover")
	}
	switch w.Status {
	case WOInProgress, WOCompleted, WOReviewed:
	default:
		return invalid("%s is %s: the handover is recorded while the work is in progress or done", w.Number, w.Status)
	}
	if blank(p.Name) || len(p.Name) > 100 || len(p.Note) > 500 {
		return invalid("the handover needs the receiving person's name (up to 100 characters)")
	}
	return ap.bumpWorkOrder(`handover_name = ?, handover_note = ?, handover_at = ?, handover_by = ?`,
		p.Name, p.Note, ap.e.WallTime.UTC().Format(time.RFC3339), ap.actor.id)
}

// Handover is a work order's recorded handover, if any.
type Handover struct {
	Name, Note, By string // By: the name of who recorded it
	At             time.Time
}

// WorkOrderLinks are a work order's follow-up links and handover.
type WorkOrderLinks struct {
	FollowUpOf WorkOrderRef   // the work order it follows up (ID empty: none)
	FollowUps  []WorkOrderRef // work orders opened as its follow-ups
	Handover   Handover
}

// WorkOrderRef names a work order.
type WorkOrderRef struct{ ID, Number, Title, Status string }

// GetWorkOrderLinks returns a work order's follow-ups and handover.
func GetWorkOrderLinks(ctx context.Context, q Querier, woID string) (WorkOrderLinks, error) {
	var l WorkOrderLinks
	var at string
	err := q.QueryRowContext(ctx, `SELECT w.handover_name, w.handover_note, w.handover_at, coalesce(u.legal_name, w.handover_by),
			coalesce(p.id, ''), coalesce(p.number, ''), coalesce(p.title, ''), coalesce(p.status, '')
		FROM work_orders w LEFT JOIN users u ON u.id = w.handover_by LEFT JOIN work_orders p ON p.id = w.follow_up_of AND w.follow_up_of != ''
		WHERE w.id = ?`, woID).Scan(&l.Handover.Name, &l.Handover.Note, &at, &l.Handover.By,
		&l.FollowUpOf.ID, &l.FollowUpOf.Number, &l.FollowUpOf.Title, &l.FollowUpOf.Status)
	if err != nil {
		return l, err
	}
	l.Handover.At, _ = time.Parse(time.RFC3339, at)
	rows, err := q.QueryContext(ctx, `SELECT id, number, title, status FROM work_orders WHERE follow_up_of = ? ORDER BY opened_at, number`, woID)
	if err != nil {
		return l, err
	}
	defer rows.Close()
	for rows.Next() {
		var r WorkOrderRef
		if err := rows.Scan(&r.ID, &r.Number, &r.Title, &r.Status); err != nil {
			return l, err
		}
		l.FollowUps = append(l.FollowUps, r)
	}
	return l, rows.Err()
}

// RecentRepairs counts the corrective work orders on the equipment opened
// in the days before now, other than the work order given.
func RecentRepairs(ctx context.Context, q Querier, assetID, exceptWO string, since time.Time) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM work_orders WHERE asset_id = ? AND id != ? AND type = 'corrective'
		AND status != 'cancelled' AND opened_at >= ?`, assetID, exceptWO, since.UTC().Format(time.RFC3339)).Scan(&n)
	return n, err
}
