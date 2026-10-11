package app

import (
	"context"
	"fmt"
	"strings"

	"pi-fleet/internal/domain"
	"pi-fleet/internal/store"
)

// reporter is who records problems reported through the report page,
// where no one is signed in.
var reporter = Actor{UserID: domain.SystemReport, SessionID: "report"}

// SubmitRequest records a reported problem and returns its R- number.
func (a *App) SubmitRequest(ctx context.Context, p domain.RequestSubmitted) (id, number string, err error) {
	id = newID()
	err = a.Store.Update(ctx, func(tx *store.Tx) error {
		if err := a.emit(ctx, tx, reporter, domain.TypeRequestSubmitted, domain.EntityRequest, id, 0, "", p); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT number FROM service_requests WHERE id = ?`, id).Scan(&number)
	})
	return id, number, err
}

// ConvertRequest opens a work order for a reported problem, with the
// report in its description, and links the two, in one step.
func (a *App) ConvertRequest(ctx context.Context, actor Actor, requestID, woType, priority string) (woID, number string, err error) {
	r, err := domain.GetRequest(ctx, a.Store.DB(), requestID)
	if err != nil {
		return "", "", err
	}
	woID = newID()
	err = a.Store.Update(ctx, func(tx *store.Tx) error {
		problem := fmt.Sprintf("%s\n\nReported %s (%s) by %s, %s, %s.", strings.TrimSpace(r.Description),
			r.Number, r.SubmittedAt.UTC().Format("2006-01-02 15:04 UTC"), r.Name, r.Department, r.Phone)
		if number, err = a.openWorkOrder(ctx, tx, actor, woID, NewWorkOrder{Type: woType, AssetID: r.AssetID, Priority: priority,
			Title: r.Category, Problem: problem}); err != nil {
			return err
		}
		return a.emit(ctx, tx, actor, domain.TypeRequestConverted, domain.EntityRequest, requestID, 0, "", domain.RequestConverted{WorkOrderID: woID})
	})
	return woID, number, err
}

// CloseRequest closes a reported problem without a work order.
func (a *App) CloseRequest(ctx context.Context, actor Actor, requestID, reason string) error {
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeRequestClosed, domain.EntityRequest, requestID, 0, "", domain.RequestClosed{Reason: reason})
	})
}
