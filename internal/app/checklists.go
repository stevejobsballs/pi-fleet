package app

import (
	"context"

	"pi-fleet/internal/domain"
	"pi-fleet/internal/store"
)

// PublishProcedure publishes the next version of a named checklist.
func (a *App) PublishProcedure(ctx context.Context, actor Actor, name string, steps []domain.Step) (string, int, error) {
	return a.PublishChecklist(ctx, actor, domain.ProcedurePublished{Name: name, Steps: steps})
}

// PublishChecklist publishes the next version of the checklist named in
// p, with its description, reference and category (p.Version is set).
func (a *App) PublishChecklist(ctx context.Context, actor Actor, p domain.ProcedurePublished) (string, int, error) {
	id := newID()
	err := a.Store.Update(ctx, func(tx *store.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(version), 0) + 1 FROM procedures WHERE name = ?`, p.Name).Scan(&p.Version); err != nil {
			return err
		}
		return a.emit(ctx, tx, actor, domain.TypeProcedurePublished, domain.EntityProcedure, id, 0, "", p)
	})
	return id, p.Version, err
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

// RecordStep records one step of a work order's first checklist under
// the actor's lease.
func (a *App) RecordStep(ctx context.Context, actor Actor, woID, stepID, value, note string) error {
	return a.RecordChecklistStep(ctx, actor, woID, "", stepID, value, note)
}

// RecordChecklistStep records one step of one of a work order's
// checklists ("" for its first) under the actor's lease.
func (a *App) RecordChecklistStep(ctx context.Context, actor Actor, woID, procedureID, stepID, value, note string) error {
	return a.RecordAnswer(ctx, actor, woID, procedureID, stepID, value, note, "", "")
}

// RecordAnswer records one step, with the value measured to decide a
// pass/fail step.
// For a calibration point, value is the reading as found and asLeft the
// reading after adjustment ("" when not adjusted).
func (a *App) RecordAnswer(ctx context.Context, actor Actor, woID, procedureID, stepID, value, note, measured, asLeft string) error {
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		w, err := domain.GetWorkOrder(ctx, tx, woID)
		if err != nil {
			return err
		}
		if w.AssignedTo != actor.UserID {
			return ErrNotLeaseHolder
		}
		if procedureID == w.ProcedureID {
			procedureID = "" // the first checklist, recorded as it always was
		}
		return a.emit(ctx, tx, actor, domain.TypeWorkOrderStepRecorded, domain.EntityWorkOrder, woID, w.Version, w.LeaseID,
			domain.WorkOrderStepRecorded{ProcedureID: procedureID, StepID: stepID, Value: value, Note: note, Measured: measured, AsLeft: asLeft})
	})
}

// AddChecklist adds a checklist to a work order: by the person doing it
// (under their lease) or a mid-tier user.
func (a *App) AddChecklist(ctx context.Context, actor Actor, woID, procedureID string) error {
	return a.changeChecklists(ctx, actor, woID, domain.TypeWorkOrderChecklistAdded, domain.WorkOrderChecklistAdded{ProcedureID: procedureID})
}

// RemoveChecklist removes a checklist nothing was recorded in.
func (a *App) RemoveChecklist(ctx context.Context, actor Actor, woID, procedureID string) error {
	return a.changeChecklists(ctx, actor, woID, domain.TypeWorkOrderChecklistRemoved, domain.WorkOrderChecklistRemoved{ProcedureID: procedureID})
}

func (a *App) changeChecklists(ctx context.Context, actor Actor, woID, typ string, payload any) error {
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		w, err := domain.GetWorkOrder(ctx, tx, woID)
		if err != nil {
			return err
		}
		lease := ""
		if w.AssignedTo == actor.UserID {
			lease = w.LeaseID
		}
		return a.emit(ctx, tx, actor, typ, domain.EntityWorkOrder, woID, w.Version, lease, payload)
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

// ProposeChecklist sends a checklist for approval by a mid-tier or super
// user (basedOn: the version it was started from, if any).
func (a *App) ProposeChecklist(ctx context.Context, actor Actor, p domain.ProcedurePublished, basedOn string) (string, error) {
	id := newID()
	p.Version = 0
	return id, a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeProcedureProposed, domain.EntityProposal, id, 0, "",
			domain.ProcedureProposed{ProcedurePublished: p, BasedOn: basedOn})
	})
}

// ReviewChecklist approves (publishing it) or rejects a proposed
// checklist; it returns the published version's id when approved.
func (a *App) ReviewChecklist(ctx context.Context, actor Actor, proposalID string, approved bool, note string) (string, error) {
	pub := ""
	if approved {
		pub = newID()
	}
	return pub, a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeProcedureProposalReviewed, domain.EntityProposal, proposalID, 0, "",
			domain.ProcedureProposalReviewed{Approved: approved, Note: note, PublishedID: pub})
	})
}

// StartChecklist fills in a checklist on a piece of equipment straight
// from the library: it opens an inspection work order with the checklist,
// takes it for the actor and starts it.
func (a *App) StartChecklist(ctx context.Context, actor Actor, assetID, procedureID string) (string, error) {
	p, err := domain.GetProcedure(ctx, a.Store.DB(), procedureID)
	if err != nil {
		return "", err
	}
	id, _, err := a.OpenWorkOrder(ctx, actor, NewWorkOrder{Type: "inspection", AssetID: assetID, Priority: "normal",
		Title: "Checklist: " + p.Name, ProcedureID: procedureID})
	if err != nil {
		return "", err
	}
	if _, err := a.ClaimWorkOrder(ctx, actor, id); err != nil {
		return id, err
	}
	return id, a.ChangeWorkOrderStatus(ctx, actor, id, domain.WOInProgress, "")
}
