package app

import (
	"context"

	"pi-fleet/internal/domain"
	"pi-fleet/internal/store"
)

// ResolveFlag records a mid-tier decision on a flagged record.
func (a *App) ResolveFlag(ctx context.Context, actor Actor, eventID, resolution, note string) error {
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeConflictResolved, domain.EntityFlaggedEvent, eventID, 0, "",
			domain.ConflictResolved{Resolution: resolution, Note: note})
	})
}

// ClearQuarantine releases a Pi quarantined after a chain fork, once a
// super user has investigated. Run on central.
func (a *App) ClearQuarantine(ctx context.Context, actor Actor, nodeID, reason string) error {
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		if err := a.emit(ctx, tx, actor, domain.TypeNodeQuarantineCleared, domain.EntityNode, nodeID, 0, "",
			domain.NodeQuarantineCleared{Reason: reason}); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM node_config WHERE key = ?`, "quarantine:"+nodeID)
		return err
	})
}

// Quarantined reports whether central has quarantined a Pi.
func (a *App) Quarantined(ctx context.Context, nodeID string) bool {
	_, err := a.Store.Config(ctx, "quarantine:"+nodeID)
	return err == nil
}
