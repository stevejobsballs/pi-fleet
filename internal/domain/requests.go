package domain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Service requests: a problem reported by anyone on the hospital network,
// usually by scanning a piece of equipment's QR label. No one is signed
// in, so the master Pi records it as SystemReport. Someone signed in then
// turns it into a work order or closes it.

const (
	TypeRequestSubmitted = "request.submitted"
	TypeRequestConverted = "request.converted"
	TypeRequestClosed    = "request.closed"

	EntityRequest = "service_request"

	// SystemReport records problems reported through the report page.
	SystemReport = "system:report"

	RequestStatusNew       = "new"
	RequestStatusConverted = "converted"
	RequestStatusClosed    = "closed"

	maxRequestText = 2000
	maxRequestLine = 120
)

// RequestCategories are the problems a reporter chooses from.
var RequestCategories = []string{
	"Not working", "Damaged or broken part", "Alarm or error message", "Needs a check or cleaning", "Something else",
}

// RequestSubmitted is a reported problem.
type RequestSubmitted struct {
	AssetID     string `json:"asset_id"`
	Category    string `json:"category"`
	Description string `json:"description"`
	Name        string `json:"name"`
	Department  string `json:"department"`
	Phone       string `json:"phone"`
}

// RequestConverted links a request to the work order opened for it.
type RequestConverted struct {
	WorkOrderID string `json:"work_order_id"`
}

// RequestClosed closes a request without a work order (a duplicate, or
// nothing wrong).
type RequestClosed struct {
	Reason string `json:"reason"`
}

func (ap *applier) requestSubmitted(p *RequestSubmitted) error {
	if ap.actor.id != SystemReport {
		return invalid("problems are reported through the report page")
	}
	if err := ap.newEntity("service_requests"); err != nil {
		return err
	}
	var merged string
	err := ap.tx.QueryRowContext(ap.ctx, `SELECT merged_into FROM assets WHERE id = ?`, p.AssetID).Scan(&merged)
	if errors.Is(err, sql.ErrNoRows) {
		return invalid("asset %s not found", p.AssetID)
	}
	if err != nil {
		return err
	}
	if merged != "" {
		p.AssetID = merged // reported against a duplicate: the record kept
	}
	if !slices.Contains(RequestCategories, p.Category) {
		return invalid("unknown problem %q", p.Category)
	}
	if blank(p.Description) || blank(p.Name) || blank(p.Department) || blank(p.Phone) {
		return invalid("what's wrong, your name, your department and a phone number or extension are needed")
	}
	if len(p.Description) > maxRequestText || len(p.Name) > maxRequestLine || len(p.Department) > maxRequestLine || len(p.Phone) > maxRequestLine {
		return invalid("the report is too long")
	}
	n, err := ap.count(`SELECT count(*) FROM service_requests`)
	if err != nil {
		return err
	}
	t := strings.TrimSpace
	return ap.exec(`INSERT INTO service_requests (id, number, asset_id, category, description, requester_name, requester_department,
			requester_phone, submitted_at, status, version, last_event_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)`,
		ap.e.EntityID, fmt.Sprintf("R-%05d", n+1), p.AssetID, p.Category, t(p.Description), t(p.Name), t(p.Department), t(p.Phone),
		ap.wall(), RequestStatusNew, ap.e.EventID)
}

// targetRequest is the request an event acts on, which must still be new.
func (ap *applier) targetRequest() (assetID string, err error) {
	var status string
	err = ap.tx.QueryRowContext(ap.ctx, `SELECT asset_id, status FROM service_requests WHERE id = ?`, ap.e.EntityID).Scan(&assetID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", invalid("request %s not found", ap.e.EntityID)
	}
	if err != nil {
		return "", err
	}
	if status != RequestStatusNew {
		return "", orConflict(nil, "this request was already %s", status)
	}
	return assetID, nil
}

func (ap *applier) requestConverted(p *RequestConverted) error {
	if err := ap.require(RoleUser); err != nil {
		return err
	}
	assetID, err := ap.targetRequest()
	if err != nil {
		return err
	}
	if ok, err := ap.exists(`SELECT 1 FROM work_orders WHERE id = ? AND asset_id = ?`, p.WorkOrderID, assetID); err != nil || !ok {
		return orInvalid(err, "work order %s for this equipment not found", p.WorkOrderID)
	}
	return ap.exec(`UPDATE service_requests SET status = ?, work_order_id = ?, handled_by = ?, version = version + 1, last_event_id = ? WHERE id = ?`,
		RequestStatusConverted, p.WorkOrderID, ap.e.ActorUserID, ap.e.EventID, ap.e.EntityID)
}

func (ap *applier) requestClosed(p *RequestClosed) error {
	if err := ap.require(RoleUser); err != nil {
		return err
	}
	if _, err := ap.targetRequest(); err != nil {
		return err
	}
	if blank(p.Reason) {
		return invalid("closing a request needs a reason, which the person who reported it sees")
	}
	return ap.exec(`UPDATE service_requests SET status = ?, closed_reason = ?, handled_by = ?, version = version + 1, last_event_id = ? WHERE id = ?`,
		RequestStatusClosed, strings.TrimSpace(p.Reason), ap.e.ActorUserID, ap.e.EventID, ap.e.EntityID)
}

// ServiceRequest is a reported problem as listed for triage.
type ServiceRequest struct {
	ID, Number, AssetID, AssetTag, Equipment, Place string
	Category, Description                           string
	Name, Department, Phone                         string
	SubmittedAt                                     time.Time
	Status, ClosedReason                            string
	WorkOrderID, WorkOrderNumber, WorkOrderStatus   string
}

const requestColumns = `r.id, r.number, r.asset_id, a.tag, a.manufacturer || ' ' || a.model, s.path || ' › ' || l.name,
	r.category, r.description, r.requester_name, r.requester_department, r.requester_phone, r.submitted_at, r.status, r.closed_reason,
	r.work_order_id, coalesce(w.number, ''), coalesce(w.status, '')
	FROM service_requests r JOIN assets a ON a.id = r.asset_id JOIN sites s ON s.id = a.site_id JOIN locations l ON l.id = a.location_id
	LEFT JOIN work_orders w ON w.id = r.work_order_id`

func scanRequest(r interface{ Scan(...any) error }) (ServiceRequest, error) {
	var q ServiceRequest
	var at string
	err := r.Scan(&q.ID, &q.Number, &q.AssetID, &q.AssetTag, &q.Equipment, &q.Place, &q.Category, &q.Description, &q.Name, &q.Department,
		&q.Phone, &at, &q.Status, &q.ClosedReason, &q.WorkOrderID, &q.WorkOrderNumber, &q.WorkOrderStatus)
	q.SubmittedAt, _ = time.Parse(time.RFC3339, at)
	return q, err
}

// ListRequests lists requests with this status, oldest first for new
// ones, newest first otherwise, at most limit.
func ListRequests(ctx context.Context, q Querier, status string, limit int) ([]ServiceRequest, error) {
	order := "r.submitted_at DESC, r.number DESC"
	if status == RequestStatusNew {
		order = "r.submitted_at, r.number"
	}
	rows, err := q.QueryContext(ctx, `SELECT `+requestColumns+` WHERE r.status = ? ORDER BY `+order+` LIMIT ?`, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ServiceRequest
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRequest returns a request by id or by its R- number.
func GetRequest(ctx context.Context, q Querier, idOrNumber string) (ServiceRequest, error) {
	r, err := scanRequest(q.QueryRowContext(ctx, `SELECT `+requestColumns+` WHERE r.id = ? OR r.number = ?`, idOrNumber, idOrNumber))
	if errors.Is(err, sql.ErrNoRows) {
		return ServiceRequest{}, ErrNotFound
	}
	return r, err
}

// ReportTarget finds the equipment a QR label's code names: its MasterID,
// or its asset tag when it has none. Of duplicates sharing a MasterID, the
// record kept (not merged into another) is used.
func ReportTarget(ctx context.Context, q Querier, code string) (Asset, error) {
	var id string
	err := q.QueryRowContext(ctx, `SELECT id FROM assets WHERE merged_into = '' AND status != 'retired' AND (master_id = ? OR (tag = ? AND master_id = ''))
		ORDER BY master_id = ? DESC, tag LIMIT 1`, code, code, code).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Asset{}, ErrNotFound
	}
	if err != nil {
		return Asset{}, err
	}
	return GetAsset(ctx, q, id)
}
