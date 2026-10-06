package app

import (
	"context"
	"errors"
	"time"

	"pi-fleet/internal/blobs"
	"pi-fleet/internal/domain"
	"pi-fleet/internal/store"
)

var ErrNoBlobStore = errors.New("app: no blob store configured")

// AddAttachment sanitises a file, stores it by hash, and attaches it to a
// work order or piece of equipment. On an employee Pi the file is queued
// for upload to central.
func (a *App) AddAttachment(ctx context.Context, actor Actor, targetType, targetID, filename, description string, data []byte) (string, error) {
	if a.Blobs == nil {
		return "", ErrNoBlobStore
	}
	mime, clean, err := blobs.Sanitize(data)
	if err != nil {
		return "", err
	}
	sha, err := a.Blobs.Put(clean)
	if err != nil {
		return "", err
	}
	id := newID()
	return id, a.Store.Update(ctx, func(tx *store.Tx) error {
		lease := ""
		if targetType == domain.EntityWorkOrder {
			if w, err := domain.GetWorkOrder(ctx, tx, targetID); err == nil && w.AssignedTo == actor.UserID {
				lease = w.LeaseID
			}
		}
		if err := a.emit(ctx, tx, actor, domain.TypeAttachmentAdded, domain.EntityAttachment, id, 0, lease, domain.AttachmentAdded{
			SHA256: sha, MIME: mime, Size: int64(len(clean)), Filename: domain.CleanFilename(filename),
			Description: description, TargetType: targetType, TargetID: targetID,
		}); err != nil {
			return err
		}
		if !a.QueueUploads {
			return nil
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO blob_uploads (sha256, added_at) VALUES (?, ?) ON CONFLICT DO NOTHING`,
			sha, a.now().UTC().Format(time.RFC3339))
		return err
	})
}

// DetachAttachment removes an attachment from its record; with purge, the
// file is deleted from this Pi too (and from central and backups once
// they see the event).
func (a *App) DetachAttachment(ctx context.Context, actor Actor, id, reason string, purge bool) error {
	err := a.Store.Update(ctx, func(tx *store.Tx) error {
		lease := ""
		if att, err := domain.GetAttachment(ctx, tx, id); err == nil && att.TargetType == domain.EntityWorkOrder {
			if wo, err := domain.GetWorkOrder(ctx, tx, att.TargetID); err == nil && wo.AssignedTo == actor.UserID {
				lease = wo.LeaseID
			}
		}
		return a.emit(ctx, tx, actor, domain.TypeAttachmentDetached, domain.EntityAttachment, id, 0, lease,
			domain.AttachmentDetached{Reason: reason, Purge: purge})
	})
	if err != nil || !purge {
		return err
	}
	_, err = a.PurgeBlobs(ctx)
	return err
}

// PurgeBlobs deletes stored files whose every attachment has been purged.
func (a *App) PurgeBlobs(ctx context.Context) ([]string, error) {
	if a.Blobs == nil {
		return nil, nil
	}
	rows, err := a.Store.DB().QueryContext(ctx, `SELECT sha256 FROM attachments GROUP BY sha256
		HAVING sum(status = 'purged') > 0 AND sum(status != 'purged') = 0`)
	if err != nil {
		return nil, err
	}
	var shas []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		shas = append(shas, s)
	}
	rows.Close()
	for _, s := range shas {
		if err := a.Blobs.Remove(s); err != nil {
			return shas, err
		}
		if err := a.Store.Update(ctx, func(tx *store.Tx) error {
			_, err := tx.ExecContext(ctx, `DELETE FROM blob_uploads WHERE sha256 = ?`, s)
			return err
		}); err != nil {
			return shas, err
		}
	}
	return shas, nil
}
