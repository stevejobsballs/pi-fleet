package app

import (
	"context"
	"errors"
	"time"

	"pi-fleet/internal/domain"
	"pi-fleet/internal/store"
)

// DefineModel adds a model to the catalogue.
func (a *App) DefineModel(ctx context.Context, actor Actor, m domain.ModelDefined) (string, error) {
	id := newID()
	return id, a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeModelDefined, domain.EntityModel, id, 0, "", m)
	})
}

// ChangeModel replaces a model's details, as last seen at version base.
func (a *App) ChangeModel(ctx context.Context, actor Actor, id string, base int64, m domain.ModelChanged) error {
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeModelChanged, domain.EntityModel, id, base, "", m)
	})
}

// MissingModelSchedules are the catalogue schedules of an asset's model
// that it has no active schedule like (same kind of work and title).
func MissingModelSchedules(ctx context.Context, q domain.Querier, m domain.Model, assetID string) ([]domain.ModelSchedule, error) {
	var out []domain.ModelSchedule
	for _, s := range m.Schedules {
		var n int
		if err := q.QueryRowContext(ctx, `SELECT count(*) FROM pm_schedules WHERE asset_id = ? AND status = 'active'
			AND wo_type = ? AND title = ?`, assetID, s.WOType, s.Title).Scan(&n); err != nil {
			return nil, err
		}
		if n == 0 {
			out = append(out, s)
		}
	}
	return out, nil
}

// SetUpModelSchedules gives each asset the schedules its catalogue model
// says it should have and it hasn't got, first due one interval from
// today. It returns how many schedules it set up.
func (a *App) SetUpModelSchedules(ctx context.Context, actor Actor, modelID string, assetIDs ...string) (int, error) {
	n := 0
	err := a.Store.Update(ctx, func(tx *store.Tx) error {
		m, err := domain.GetModel(ctx, tx, modelID)
		if err != nil {
			return err
		}
		for _, assetID := range assetIDs {
			missing, err := MissingModelSchedules(ctx, tx, m, assetID)
			if err != nil {
				return err
			}
			for _, s := range missing {
				proc := ""
				if s.Checklist != "" {
					if proc, err = domain.NewestProcedure(ctx, tx, s.Checklist); err != nil && !errors.Is(err, domain.ErrNotFound) {
						return err
					}
				}
				first := a.now().AddDate(0, 0, s.IntervalDays).Format(time.DateOnly)
				if err := a.emit(ctx, tx, actor, domain.TypePMScheduleCreated, domain.EntitySchedule, newID(), 0, "", domain.PMScheduleCreated{
					AssetID: assetID, WOType: s.WOType, Title: s.Title, IntervalDays: s.IntervalDays, GraceDays: s.GraceDays,
					FirstDue: first, ProcedureID: proc, FromModel: m.ID,
				}); err != nil {
					return err
				}
				n++
			}
		}
		return nil
	})
	return n, err
}
