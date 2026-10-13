package app

import (
	"context"

	"pi-fleet/internal/domain"
	"pi-fleet/internal/store"
)

// StartClock starts the person's clock on a work order for labour or
// travel. A clock already running (on this work order or another) is
// stopped first, so switching is one press. Starting work on a work order
// the person holds also moves it to in progress. It returns the time the
// stopped clock recorded, if one was running.
func (a *App) StartClock(ctx context.Context, actor Actor, woID, kind string) (stopped Stopped, err error) {
	id := newID()
	err = a.Store.Update(ctx, func(tx *store.Tx) error {
		if stopped, err = a.stopClock(ctx, tx, actor, actor.UserID, ""); err != nil {
			return err
		}
		w, err := domain.GetWorkOrder(ctx, tx, woID)
		if err != nil {
			return err
		}
		if w.AssignedTo == actor.UserID && (w.Status == domain.WOAssigned || w.Status == domain.WOOnHold) {
			if err := a.emit(ctx, tx, actor, domain.TypeWorkOrderStatusChanged, domain.EntityWorkOrder, woID, w.Version, w.LeaseID,
				domain.WorkOrderStatusChanged{From: w.Status, To: domain.WOInProgress}); err != nil {
				return err
			}
		}
		return a.emit(ctx, tx, actor, domain.TypeLaborClockStarted, domain.EntityLabor, id, 0, "",
			domain.LaborClockStarted{WorkOrderID: woID, Kind: kind})
	})
	return stopped, err
}

// Stopped is what a stopped clock recorded.
type Stopped struct {
	Running     bool // there was a clock
	WorkOrderID string
	Number      string
	Kind        string
	Minutes     int // 0: under a minute, nothing recorded
}

// StopClock stops the person's clock, if it is running.
func (a *App) StopClock(ctx context.Context, actor Actor, note string) (stopped Stopped, err error) {
	err = a.Store.Update(ctx, func(tx *store.Tx) error {
		stopped, err = a.stopClock(ctx, tx, actor, actor.UserID, note)
		return err
	})
	return stopped, err
}

// StopClockOf stops someone else's clock (a mid-tier user, for a clock
// left running).
func (a *App) StopClockOf(ctx context.Context, actor Actor, userID string) (stopped Stopped, err error) {
	err = a.Store.Update(ctx, func(tx *store.Tx) error {
		stopped, err = a.stopClock(ctx, tx, actor, userID, "")
		return err
	})
	return stopped, err
}

func (a *App) stopClock(ctx context.Context, tx *store.Tx, actor Actor, userID, note string) (Stopped, error) {
	c, ok, err := domain.RunningClock(ctx, tx, userID)
	if err != nil || !ok {
		return Stopped{}, err
	}
	now := a.now()
	s := Stopped{Running: true, WorkOrderID: c.WorkOrderID, Number: c.Number, Kind: c.Kind, Minutes: domain.ClockMinutes(c.Started, now)}
	return s, a.emit(ctx, tx, actor, domain.TypeLaborClockStopped, domain.EntityLabor, c.ID, 0, "",
		domain.LaborClockStopped{Date: c.Started.Local().Format("2006-01-02"), Note: note})
}

// LogTime records time typed in: labour or travel.
func (a *App) LogTime(ctx context.Context, actor Actor, woID, kind string, minutes int, date, note string) (string, error) {
	id := newID()
	return id, a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeLaborLogged, domain.EntityLabor, id, 0, "",
			domain.LaborLogged{WorkOrderID: woID, Minutes: minutes, Date: date, Note: note, Kind: kind})
	})
}

// CorrectLabor changes a labour entry's time, day, kind and note.
func (a *App) CorrectLabor(ctx context.Context, actor Actor, id string, c domain.LaborCorrected) error {
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeLaborCorrected, domain.EntityLabor, id, 0, "", c)
	})
}
