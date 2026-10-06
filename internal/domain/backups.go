package domain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"pi-fleet/internal/store"
)

// Backup event types (DESIGN.md §8).
const (
	TypeBackupSnapshotVerified = "backup.snapshot_verified"
	TypeBackupOffsiteWritten   = "backup.offsite_written"
	TypeBackupOffsiteConfirmed = "backup.offsite_confirmed"
	TypeBackupRestored         = "backup.restored"
	EntityBackup               = "backup"

	// SystemBackup records backups and restores on central.
	SystemBackup = "system:backup"
)

// OffsiteOverdue is how long after the last confirmed off-site copy
// super users are warned (DESIGN.md §8.2).
const OffsiteOverdue = 10 * 24 * time.Hour

// BackupWritten describes a verified snapshot written to the backup disk
// or to an off-site disk.
type BackupWritten struct {
	DiskID     string           `json:"disk_id,omitempty"`
	DiskLabel  string           `json:"disk_label,omitempty"`
	File       string           `json:"file"`
	SHA256     string           `json:"sha256"`
	LocalOrder int64            `json:"local_order"`
	Heads      map[string]int64 `json:"heads"`
}

// BackupOffsiteConfirmed is a person confirming an off-site disk has left
// the building. It names the offsite_written record it confirms.
type BackupOffsiteConfirmed struct {
	BackupID string `json:"backup_id"`
}

// BackupRestored records a restore and its verification result.
type BackupRestored struct {
	Source     string `json:"source"`
	LocalOrder int64  `json:"local_order"`
	Events     int    `json:"events_replayed"`
	Verified   bool   `json:"verified"`
}

var hexHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

func init() {
	reg := func(typ string, f func() any) {
		payloadTypes[typ] = struct {
			entity string
			new    func() any
		}{EntityBackup, f}
	}
	reg(TypeBackupSnapshotVerified, func() any { return &BackupWritten{} })
	reg(TypeBackupOffsiteWritten, func() any { return &BackupWritten{} })
	reg(TypeBackupOffsiteConfirmed, func() any { return &BackupOffsiteConfirmed{} })
	reg(TypeBackupRestored, func() any { return &BackupRestored{} })
	systemActorTypes[SystemBackup] = []string{TypeBackupSnapshotVerified, TypeBackupOffsiteWritten, TypeBackupRestored}
}

func (ap *applier) backupWritten(p *BackupWritten) error {
	if ap.actor.id != SystemBackup {
		return store.Reject(FlagNotAuthorized, "backups are recorded by the backup system")
	}
	if err := ap.newEntity("backups"); err != nil {
		return err
	}
	kind := "snapshot"
	if ap.e.Type == TypeBackupOffsiteWritten {
		kind = "offsite"
		if blank(p.DiskID) || blank(p.DiskLabel) {
			return invalid("off-site backups need a disk id and label")
		}
	}
	if !hexHash.MatchString(p.SHA256) || blank(p.File) || p.LocalOrder < 1 {
		return invalid("backup record is incomplete")
	}
	heads, _ := json.Marshal(p.Heads)
	return ap.exec(`INSERT INTO backups (id, kind, disk_id, disk_label, file, sha256, local_order, heads, written_at,
			confirmed_at, confirmed_by, last_event_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', ?)`,
		ap.e.EntityID, kind, p.DiskID, p.DiskLabel, p.File, p.SHA256, p.LocalOrder, string(heads), ap.wall(), ap.e.EventID)
}

func (ap *applier) backupOffsiteConfirmed(p *BackupOffsiteConfirmed) error {
	if err := ap.require(RoleMidTier); err != nil {
		return err
	}
	var kind, confirmed, heads string
	err := ap.tx.QueryRowContext(ap.ctx, `SELECT kind, confirmed_at, heads FROM backups WHERE id = ?`, p.BackupID).Scan(&kind, &confirmed, &heads)
	if errors.Is(err, sql.ErrNoRows) {
		return invalid("backup %s not found", p.BackupID)
	}
	if err != nil {
		return err
	}
	if kind != "offsite" {
		return invalid("only off-site copies are confirmed")
	}
	if confirmed != "" {
		return invalid("this off-site copy was already confirmed")
	}
	if p.BackupID != ap.e.EntityID {
		return invalid("the confirmation must target the backup record")
	}
	var h map[string]int64
	if err := json.Unmarshal([]byte(heads), &h); err != nil {
		return err
	}
	for chain, seq := range h {
		if err := ap.exec(`INSERT INTO durable_heads (chain_id, seq) VALUES (?, ?)
			ON CONFLICT (chain_id) DO UPDATE SET seq = max(seq, excluded.seq)`, chain, seq); err != nil {
			return err
		}
	}
	return ap.exec(`UPDATE backups SET confirmed_at = ?, confirmed_by = ?, last_event_id = ? WHERE id = ?`,
		ap.wall(), ap.actor.id, ap.e.EventID, p.BackupID)
}

func (ap *applier) backupRestored(p *BackupRestored) error {
	if ap.actor.id != SystemBackup {
		return store.Reject(FlagNotAuthorized, "restores are recorded by the backup system")
	}
	return nil // the event itself is the audit record
}

// DurableSeq returns a chain's durable watermark (0 if none).
func DurableSeq(ctx context.Context, q Querier, chainID string) (int64, error) {
	var seq int64
	err := q.QueryRowContext(ctx, `SELECT seq FROM durable_heads WHERE chain_id = ?`, chainID).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return seq, err
}

// BackupRecord is a projected backup.
type BackupRecord struct {
	ID, Kind, DiskLabel, File, SHA256 string
	LocalOrder                        int64
	WrittenAt, ConfirmedAt            time.Time
}

// ListBackups returns backup records, newest first.
func ListBackups(ctx context.Context, q Querier) ([]BackupRecord, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, kind, disk_label, file, sha256, local_order, written_at, confirmed_at
		FROM backups ORDER BY written_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BackupRecord
	for rows.Next() {
		var b BackupRecord
		var w, c string
		if err := rows.Scan(&b.ID, &b.Kind, &b.DiskLabel, &b.File, &b.SHA256, &b.LocalOrder, &w, &c); err != nil {
			return nil, err
		}
		b.WrittenAt, _ = time.Parse(time.RFC3339, w)
		b.ConfirmedAt, _ = time.Parse(time.RFC3339, c)
		out = append(out, b)
	}
	return out, rows.Err()
}

// LastOffsiteConfirmed returns when an off-site copy was last confirmed
// (zero if never).
func LastOffsiteConfirmed(ctx context.Context, q Querier) (time.Time, error) {
	var s sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT max(confirmed_at) FROM backups WHERE kind = 'offsite' AND confirmed_at != ''`).Scan(&s); err != nil {
		return time.Time{}, err
	}
	t, _ := time.Parse(time.RFC3339, s.String)
	return t, nil
}
