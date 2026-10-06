package app

import (
	"context"

	"pi-fleet/internal/domain"
	"pi-fleet/internal/store"
)

// backupActor records backups and restores on central.
var backupActor = Actor{UserID: domain.SystemBackup, SessionID: "backup"}

// RecordBackup records a verified backup write and returns its id.
func (a *App) RecordBackup(ctx context.Context, typ string, p domain.BackupWritten) (string, error) {
	id := newID()
	return id, a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, backupActor, typ, domain.EntityBackup, id, 0, "", p)
	})
}

// ConfirmOffsite records that an off-site copy has left the building,
// advancing the durable watermark.
func (a *App) ConfirmOffsite(ctx context.Context, actor Actor, backupID string) error {
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeBackupOffsiteConfirmed, domain.EntityBackup, backupID, 0, "",
			domain.BackupOffsiteConfirmed{BackupID: backupID})
	})
}

// RecordRestore records a completed restore.
func (a *App) RecordRestore(ctx context.Context, p domain.BackupRestored) error {
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, backupActor, domain.TypeBackupRestored, domain.EntityBackup, newID(), 0, "", p)
	})
}
