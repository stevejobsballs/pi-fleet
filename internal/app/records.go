package app

import (
	"context"
	"fmt"
	"time"

	"pi-fleet/internal/domain"
	"pi-fleet/internal/store"
)

// --- calibrations ---

// RecordCalibration records a calibration under the actor's lease on a
// calibration work order. Pass/fail is computed here from the readings;
// central recomputes it independently.
func (a *App) RecordCalibration(ctx context.Context, actor Actor, c domain.CalibrationRecorded) (string, error) {
	if err := domain.ComputeResults(&c); err != nil {
		return "", err
	}
	id := newID()
	return id, a.Store.Update(ctx, func(tx *store.Tx) error {
		w, err := domain.GetWorkOrder(ctx, tx, c.WorkOrderID)
		if err != nil {
			return err
		}
		if w.AssignedTo != actor.UserID {
			return ErrNotLeaseHolder
		}
		return a.emit(ctx, tx, actor, domain.TypeCalibrationRecorded, domain.EntityCalRecord, id, 0, w.LeaseID, c)
	})
}

// VoidCalibration voids a calibration record, for example to re-record it.
func (a *App) VoidCalibration(ctx context.Context, actor Actor, recordID, reason string) error {
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		var woID string
		if err := tx.QueryRowContext(ctx, `SELECT wo_id FROM calibration_records WHERE id = ?`, recordID).Scan(&woID); err != nil {
			return err
		}
		w, err := domain.GetWorkOrder(ctx, tx, woID)
		if err != nil {
			return err
		}
		lease := ""
		if w.AssignedTo == actor.UserID {
			lease = w.LeaseID
		}
		return a.emit(ctx, tx, actor, domain.TypeCalibrationVoided, domain.EntityCalRecord, recordID, 0, lease,
			domain.CalibrationVoided{Reason: reason})
	})
}

// --- PM schedules ---

// CreateSchedule creates a recurring PM, calibration or inspection.
func (a *App) CreateSchedule(ctx context.Context, actor Actor, s domain.PMScheduleCreated) (string, error) {
	id := newID()
	return id, a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypePMScheduleCreated, domain.EntitySchedule, id, 0, "", s)
	})
}

// ChangeSchedule changes the fields present in c.
func (a *App) ChangeSchedule(ctx context.Context, actor Actor, scheduleID string, c domain.PMScheduleChanged) error {
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypePMScheduleChanged, domain.EntitySchedule, scheduleID, 0, "", c)
	})
}

// EndSchedule stops a schedule generating work.
func (a *App) EndSchedule(ctx context.Context, actor Actor, scheduleID, reason string) error {
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypePMScheduleEnded, domain.EntitySchedule, scheduleID, 0, "",
			domain.PMScheduleEnded{Reason: reason})
	})
}

// Scheduler is the actor that opens scheduled work orders.
var Scheduler = Actor{UserID: domain.SystemScheduler, SessionID: "scheduler"}

// WorkingSetWindow is how far ahead scheduled work is generated, matching
// the nodes' 31-day working set (decision D8).
const WorkingSetWindow = 31 * 24 * time.Hour

// GenerateDueWorkOrders opens a work order for every active schedule due
// within horizon that has none outstanding. Run on central.
func (a *App) GenerateDueWorkOrders(ctx context.Context, horizon time.Duration) ([]string, error) {
	cutoff := a.now().Add(horizon).UTC().Format("2006-01-02")
	rows, err := a.Store.DB().QueryContext(ctx, `SELECT id FROM pm_schedules
		WHERE status = 'active' AND open_wo_id = '' AND next_due <= ? ORDER BY next_due, id`, cutoff)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var numbers []string
	for _, id := range ids {
		s, err := domain.GetSchedule(ctx, a.Store.DB(), id)
		if err != nil {
			return numbers, err
		}
		_, number, err := a.OpenWorkOrder(ctx, Scheduler, NewWorkOrder{
			Type: s.WOType, AssetID: s.AssetID, Priority: "normal", Title: s.Title,
			Problem: fmt.Sprintf("Scheduled every %d days. Procedure: %s", s.IntervalDays, s.Procedure),
			DueAt:   s.NextDue, scheduleID: s.ID,
		})
		if err != nil {
			return numbers, fmt.Errorf("app: schedule %s: %w", s.ID, err)
		}
		numbers = append(numbers, number)
	}
	return numbers, nil
}

// --- inventory ---

// CreatePart adds a part to the catalog.
func (a *App) CreatePart(ctx context.Context, actor Actor, p domain.PartCreated) (string, error) {
	id := newID()
	return id, a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypePartCreated, domain.EntityPart, id, 0, "", p)
	})
}

// CreateStockLocation creates a shared stockroom or a personal kit.
func (a *App) CreateStockLocation(ctx context.Context, actor Actor, l domain.StockLocationCreated) (string, error) {
	id := newID()
	return id, a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeStockLocationCreated, domain.EntityStockLoc, id, 0, "", l)
	})
}

// RecordStock records one stock ledger transaction.
func (a *App) RecordStock(ctx context.Context, actor Actor, t domain.StockTxnRecorded) (string, error) {
	id := newID()
	return id, a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeStockTxnRecorded, domain.EntityStockTxn, id, 0, "", t)
	})
}

// ReverseStock cancels a stock transaction.
func (a *App) ReverseStock(ctx context.Context, actor Actor, txnID, reason string) error {
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeStockTxnReversed, domain.EntityStockTxn, txnID, 0, "",
			domain.StockTxnReversed{Reason: reason})
	})
}

// StockLevel returns the ledger quantity of a part at a location.
func (a *App) StockLevel(ctx context.Context, partID, locationID string) (int64, error) {
	var q int64
	err := a.Store.DB().QueryRowContext(ctx, `SELECT coalesce(sum(qty), 0) FROM stock_levels WHERE part_id = ? AND location_id = ?`,
		partID, locationID).Scan(&q)
	return q, err
}

// --- lockout ---

// UnlockUser lifts a lockout.
func (a *App) UnlockUser(ctx context.Context, actor Actor, userID, reason string) error {
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeUserUnlocked, domain.EntityUser, userID, 0, "", domain.UserUnlocked{Reason: reason})
	})
}
