package domain

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"pi-fleet/internal/store"
)

// Attachment event types (DESIGN.md §5.10).
const (
	TypeAttachmentAdded    = "attachment.added"
	TypeAttachmentDetached = "attachment.detached"
	EntityAttachment       = "attachment"

	// MaxAttachmentSize matches blobs.MaxSize.
	MaxAttachmentSize = 10 << 20
)

var attachmentTypes = map[string]bool{"image/jpeg": true, "image/png": true, "application/pdf": true}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// AttachmentAdded attaches a stored file (by hash) to a work order or
// piece of equipment.
type AttachmentAdded struct {
	SHA256      string `json:"sha256"`
	MIME        string `json:"mime"`
	Size        int64  `json:"size"`
	Filename    string `json:"filename"`
	Description string `json:"description"`
	TargetType  string `json:"target_type"`
	TargetID    string `json:"target_id"`
}

// AttachmentDetached removes an attachment from its record. With Purge
// (mid-tier, e.g. a photo showing patient information), the file itself
// is deleted everywhere, including from every other record it is attached
// to, since the problem is the file's content; its hash stays in the
// audit trail.
type AttachmentDetached struct {
	Reason string `json:"reason"`
	Purge  bool   `json:"purge"`
}

func init() {
	payloadTypes[TypeAttachmentAdded] = struct {
		entity string
		new    func() any
	}{EntityAttachment, func() any { return &AttachmentAdded{} }}
	payloadTypes[TypeAttachmentDetached] = struct {
		entity string
		new    func() any
	}{EntityAttachment, func() any { return &AttachmentDetached{} }}
}

// CleanFilename keeps the base name, without control characters, at
// most 120 characters.
func CleanFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, `\`, "/"))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7F || r == '"' {
			return -1
		}
		return r
	}, name)
	for utf8.RuneCountInString(name) > 120 {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	if name == "" || name == "." || name == "/" {
		name = "file"
	}
	return name
}

// checkAttachTarget enforces who may change a record's attachments: on a
// work order, the holder while it is in progress or a mid-tier user until
// it is closed (attachments are part of what signatures cover).
func (ap *applier) checkAttachTarget(targetType, targetID string, leaseHolderOK bool) error {
	switch targetType {
	case EntityWorkOrder:
		w, err := ap.workOrderFor(targetID)
		if err != nil {
			return err
		}
		if w.Status == WOClosed || w.Status == WOCancelled {
			return invalid("work order %s is %s", w.Number, w.Status)
		}
		if ap.actor.atLeast(RoleMidTier) {
			return nil
		}
		if !leaseHolderOK || w.Status != WOInProgress {
			return store.Reject(FlagNotAuthorized, "attachments on %s are added by its holder while in progress, or by a mid-tier user", w.Number)
		}
		return ap.checkLease(w)
	case EntityAsset:
		a, err := GetAsset(ap.ctx, ap.tx, targetID)
		if errors.Is(err, ErrNotFound) {
			return invalid("asset %s not found", targetID)
		}
		if err == nil && a.Status == AssetRetired && !ap.actor.atLeast(RoleMidTier) {
			return invalid("asset %s is retired", a.Tag)
		}
		return err
	}
	return invalid("attachments go on work orders or equipment, not %q", targetType)
}

func (ap *applier) attachmentAdded(p *AttachmentAdded) error {
	if err := ap.require(RoleUser); err != nil {
		return err
	}
	if err := ap.newEntity("attachments"); err != nil {
		return err
	}
	if !sha256Hex.MatchString(p.SHA256) || !attachmentTypes[p.MIME] || p.Size < 1 || p.Size > MaxAttachmentSize {
		return invalid("attachment must be a JPEG, PNG or PDF of at most 10 MiB, identified by its SHA-256")
	}
	if p.Filename != CleanFilename(p.Filename) {
		return invalid("unsafe filename %q", p.Filename)
	}
	if err := ap.checkAttachTarget(p.TargetType, p.TargetID, true); err != nil {
		return err
	}
	return ap.exec(`INSERT INTO attachments (id, sha256, mime, size, filename, description, target_type, target_id,
			added_by, added_at, status, detach_reason, version, last_event_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'attached', '', 1, ?)`,
		ap.e.EntityID, p.SHA256, p.MIME, p.Size, p.Filename, p.Description, p.TargetType, p.TargetID,
		ap.actor.id, ap.wall(), ap.e.EventID)
}

func (ap *applier) attachmentDetached(p *AttachmentDetached) error {
	var targetType, targetID, addedBy, status string
	err := ap.tx.QueryRowContext(ap.ctx, `SELECT target_type, target_id, added_by, status FROM attachments WHERE id = ?`,
		ap.e.EntityID).Scan(&targetType, &targetID, &addedBy, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return invalid("attachment %s not found", ap.e.EntityID)
	}
	if err != nil {
		return err
	}
	if blank(p.Reason) {
		return invalid("removing an attachment requires a reason")
	}
	if p.Purge && !ap.actor.atLeast(RoleMidTier) {
		return store.Reject(FlagNotAuthorized, "deleting the file itself requires mid_tier")
	}
	switch {
	case status == "purged", status == "detached" && !p.Purge:
		return invalid("attachment is already %s", status)
	}
	if status == "attached" {
		if err := ap.checkAttachTarget(targetType, targetID, addedBy == ap.actor.id); err != nil {
			return err
		}
	}
	if p.Purge {
		return ap.exec(`UPDATE attachments SET status = 'purged', detach_reason = ?, version = version + 1, last_event_id = ?
			WHERE sha256 = (SELECT sha256 FROM attachments WHERE id = ?)`, p.Reason, ap.e.EventID, ap.e.EntityID)
	}
	return ap.exec(`UPDATE attachments SET status = 'detached', detach_reason = ?, version = version + 1, last_event_id = ? WHERE id = ?`,
		p.Reason, ap.e.EventID, ap.e.EntityID)
}

// Attachment is a projected attachment.
type Attachment struct {
	ID, SHA256, MIME, Filename, Description, TargetType, TargetID string
	AddedBy, Status, DetachReason                                 string
	Size                                                          int64
	AddedAt                                                       time.Time
}

// ListAttachments returns a record's attachments, oldest first.
func ListAttachments(ctx context.Context, q Querier, targetType, targetID string) ([]Attachment, error) {
	rows, err := q.QueryContext(ctx, `SELECT a.id, a.sha256, a.mime, a.filename, a.description, a.target_type, a.target_id,
			coalesce(u.legal_name, a.added_by), a.status, a.detach_reason, a.size, a.added_at
		FROM attachments a LEFT JOIN users u ON u.id = a.added_by
		WHERE a.target_type = ? AND a.target_id = ? ORDER BY a.added_at, a.id`, targetType, targetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Attachment
	for rows.Next() {
		var a Attachment
		var at string
		if err := rows.Scan(&a.ID, &a.SHA256, &a.MIME, &a.Filename, &a.Description, &a.TargetType, &a.TargetID,
			&a.AddedBy, &a.Status, &a.DetachReason, &a.Size, &at); err != nil {
			return nil, err
		}
		a.AddedAt, _ = time.Parse(time.RFC3339, at)
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAttachment returns one attachment.
func GetAttachment(ctx context.Context, q Querier, id string) (Attachment, error) {
	var a Attachment
	var at string
	err := q.QueryRowContext(ctx, `SELECT id, sha256, mime, filename, description, target_type, target_id, added_by, status,
		detach_reason, size, added_at FROM attachments WHERE id = ?`, id).Scan(&a.ID, &a.SHA256, &a.MIME, &a.Filename,
		&a.Description, &a.TargetType, &a.TargetID, &a.AddedBy, &a.Status, &a.DetachReason, &a.Size, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	a.AddedAt, _ = time.Parse(time.RFC3339, at)
	return a, err
}
