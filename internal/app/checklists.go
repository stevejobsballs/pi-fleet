package app

import (
	"context"

	"pi-fleet/internal/domain"
	"pi-fleet/internal/store"
)

// PublishProcedure publishes the next version of a named checklist.
func (a *App) PublishProcedure(ctx context.Context, actor Actor, name string, steps []domain.Step) (string, int, error) {
	id := newID()
	var version int
	err := a.Store.Update(ctx, func(tx *store.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(version), 0) + 1 FROM procedures WHERE name = ?`, name).Scan(&version); err != nil {
			return err
		}
		return a.emit(ctx, tx, actor, domain.TypeProcedurePublished, domain.EntityProcedure, id, 0, "",
			domain.ProcedurePublished{Name: name, Version: version, Steps: steps})
	})
	return id, version, err
}

// RetireProcedure stops a checklist version being chosen for new work.
func (a *App) RetireProcedure(ctx context.Context, actor Actor, id, reason string) error {
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeProcedureRetired, domain.EntityProcedure, id, 0, "", domain.ProcedureRetired{Reason: reason})
	})
}

// SetWorkOrderProcedure chooses (or clears) a work order's checklist
// before work starts.
func (a *App) SetWorkOrderProcedure(ctx context.Context, actor Actor, woID, procedureID string) error {
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeWorkOrderProcedureSet, domain.EntityWorkOrder, woID, 0, "",
			domain.WorkOrderProcedureSet{ProcedureID: procedureID})
	})
}

// RecordStep records one checklist step under the actor's lease.
func (a *App) RecordStep(ctx context.Context, actor Actor, woID, stepID, value, note string) error {
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		w, err := domain.GetWorkOrder(ctx, tx, woID)
		if err != nil {
			return err
		}
		if w.AssignedTo != actor.UserID {
			return ErrNotLeaseHolder
		}
		return a.emit(ctx, tx, actor, domain.TypeWorkOrderStepRecorded, domain.EntityWorkOrder, woID, w.Version, w.LeaseID,
			domain.WorkOrderStepRecorded{StepID: stepID, Value: value, Note: note})
	})
}

// LogLabor records time the actor spent on a work order.
func (a *App) LogLabor(ctx context.Context, actor Actor, woID string, minutes int, date, note string) (string, error) {
	id := newID()
	return id, a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeLaborLogged, domain.EntityLabor, id, 0, "",
			domain.LaborLogged{WorkOrderID: woID, Minutes: minutes, Date: date, Note: note})
	})
}

// ReverseLabor cancels a labour entry.
func (a *App) ReverseLabor(ctx context.Context, actor Actor, id, reason string) error {
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeLaborReversed, domain.EntityLabor, id, 0, "", domain.LaborReversed{Reason: reason})
	})
}
