package app

import (
	"context"
	"time"

	"pi-fleet/internal/domain"
	"pi-fleet/internal/store"
)

// RecordMeter records a meter reading taken at readAt.
func (a *App) RecordMeter(ctx context.Context, actor Actor, assetID, meter, value string, readAt time.Time, reset bool) (string, error) {
	id := newID()
	return id, a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeMeterRead, domain.EntityMeter, id, 0, "", domain.MeterRead{
			AssetID: assetID, Meter: meter, Value: value, ReadAt: readAt.UTC().Format(time.RFC3339), Reset: reset,
		})
	})
}

// VoidMeter voids a mistaken reading.
func (a *App) VoidMeter(ctx context.Context, actor Actor, id, reason string) error {
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeMeterVoided, domain.EntityMeter, id, 0, "", domain.MeterVoided{Reason: reason})
	})
}
