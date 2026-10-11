package app

import (
	"context"

	"pi-fleet/internal/domain"
	"pi-fleet/internal/store"
)

// underLease emits an event recording work on a work order, under the
// actor's lease.
func (a *App) underLease(ctx context.Context, actor Actor, woID, typ string, payload any) error {
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		w, err := domain.GetWorkOrder(ctx, tx, woID)
		if err != nil {
			return err
		}
		if w.AssignedTo != actor.UserID {
			return ErrNotLeaseHolder
		}
		return a.emit(ctx, tx, actor, typ, domain.EntityWorkOrder, woID, w.Version, w.LeaseID, payload)
	})
}

// RecordWorkDone replaces the description of the work done.
func (a *App) RecordWorkDone(ctx context.Context, actor Actor, woID, text string) error {
	return a.underLease(ctx, actor, woID, domain.TypeWorkOrderWorkDone, domain.WorkOrderWorkDone{Text: text})
}

// AddTestEquipment lists test equipment (a reference standard) as used.
func (a *App) AddTestEquipment(ctx context.Context, actor Actor, woID, assetID string) error {
	return a.underLease(ctx, actor, woID, domain.TypeWorkOrderTestEquipmentAdded, domain.WorkOrderTestEquipment{AssetID: assetID})
}

// RemoveTestEquipment takes test equipment off the list.
func (a *App) RemoveTestEquipment(ctx context.Context, actor Actor, woID, assetID string) error {
	return a.underLease(ctx, actor, woID, domain.TypeWorkOrderTestEquipmentRemoved, domain.WorkOrderTestEquipment{AssetID: assetID})
}
