package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"

	"pi-fleet/internal/app"
	"pi-fleet/internal/blobs"
	"pi-fleet/internal/domain"
)

// Off-site USB disk rotation (DESIGN.md §8.2, decision D9).

const (
	markerFile = ".pi-fleet-disk.json"
	// keepOnDisk is how many snapshots an off-site disk keeps.
	keepOnDisk = 4
)

var labelRE = regexp.MustCompile(`^[A-Za-z0-9-]{1,32}$`)

// Disk is a registered off-site disk.
type Disk struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Path  string `json:"-"`
}

// RegisterDisk marks a mounted disk for off-site backups.
func RegisterDisk(path, label string) (Disk, error) {
	if !labelRE.MatchString(label) {
		return Disk{}, fmt.Errorf("backup: label %q must be 1-32 letters, digits or dashes", label)
	}
	marker := filepath.Join(path, markerFile)
	if _, err := os.Stat(marker); err == nil {
		return Disk{}, fmt.Errorf("backup: %s is already registered", path)
	}
	d := Disk{ID: uuid.NewString(), Label: label, Path: path}
	return d, writeJSON(marker, d)
}

// OpenDisk reads a mounted disk's marker.
func OpenDisk(path string) (Disk, error) {
	b, err := os.ReadFile(filepath.Join(path, markerFile))
	if err != nil {
		return Disk{}, fmt.Errorf("backup: %s is not a registered off-site disk: %w", path, err)
	}
	var d Disk
	if err := json.Unmarshal(b, &d); err != nil {
		return Disk{}, err
	}
	d.Path = path
	return d, nil
}

// copySnapshot copies a snapshot and its manifest onto a disk, reads it
// back to check it, and drops the disk's older snapshots beyond keepOnDisk.
func copySnapshot(srcDir string, m Manifest, d Disk) error {
	base := strings.TrimSuffix(m.File, snapshotSuffix)
	for _, f := range []string{m.File, base + manifestSuffix} {
		if err := copyFile(filepath.Join(srcDir, f), filepath.Join(d.Path, f)); err != nil {
			return err
		}
	}
	if got, err := fileSHA256(filepath.Join(d.Path, m.File)); err != nil || got != m.CipherSHA256 {
		return fmt.Errorf("backup: copy on %s did not read back correctly (%v)", d.Label, err)
	}
	snaps, err := Snapshots(d.Path)
	if err != nil {
		return err
	}
	for i, old := range snaps {
		if i < keepOnDisk {
			continue
		}
		b := strings.TrimSuffix(old.File, snapshotSuffix)
		os.Remove(filepath.Join(d.Path, old.File))
		os.Remove(filepath.Join(d.Path, b+manifestSuffix))
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	part := dst + ".part"
	out, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Rename(part, dst); err != nil {
		return err
	}
	syncDir(filepath.Dir(dst))
	return nil
}

// Runner performs central's backup duties and records them as events.
type Runner struct {
	App        *app.App
	Dir        string // the backup disk, e.g. /srv/pi-fleet-backup
	Recipients []age.Recipient
	Version    string
	Now        func() time.Time
	// Blobs holds attachment files to back up.
	Blobs *blobs.Store
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

const configExported = "backup_exported_order"

// Nightly takes a verified snapshot to the backup disk, records it, and
// applies retention.
func (r *Runner) Nightly(ctx context.Context) (Manifest, []string, error) {
	m, err := Snapshot(ctx, r.App.Store, r.Dir, r.Recipients, r.now(), r.Version)
	if err != nil {
		return Manifest{}, nil, err
	}
	if _, err := r.App.RecordBackup(ctx, domain.TypeBackupSnapshotVerified, domain.BackupWritten{
		File: m.File, SHA256: m.CipherSHA256, LocalOrder: m.LocalOrder, Heads: m.Heads,
	}); err != nil {
		return m, nil, err
	}
	if r.Blobs != nil {
		if _, err := backupBlobs(ctx, r.App.Store, r.Blobs, r.Dir, r.Recipients); err != nil {
			return m, nil, err
		}
	}
	removed, err := Prune(r.Dir, r.now())
	return m, removed, err
}

// Export appends events stored since the last export to the backup disk.
func (r *Runner) Export(ctx context.Context) (int64, error) {
	var after int64
	if v, err := r.App.Store.Config(ctx, configExported); err == nil {
		after, _ = strconv.ParseInt(v, 10, 64)
	}
	to, err := ExportEvents(ctx, r.App.Store, filepath.Join(r.Dir, "events"), r.Recipients, after)
	if err != nil || to == after {
		return 0, err
	}
	return to - after, r.App.Store.SetConfig(ctx, configExported, strconv.FormatInt(to, 10))
}

// RegisterOffsite registers a mounted disk and remembers it, so central
// writes only to known disks.
func (r *Runner) RegisterOffsite(ctx context.Context, path, label string) (Disk, error) {
	d, err := RegisterDisk(path, label)
	if err != nil {
		return d, err
	}
	return d, r.App.Store.SetConfig(ctx, "offsite_disk:"+d.ID, d.Label)
}

// WriteOffsite writes a fresh verified snapshot to a registered disk that
// has just been plugged in and records it. The copy counts towards the
// durable watermark only once someone confirms it left the building.
func (r *Runner) WriteOffsite(ctx context.Context, path string) (string, Manifest, error) {
	d, err := OpenDisk(path)
	if err != nil {
		return "", Manifest{}, err
	}
	if label, err := r.App.Store.Config(ctx, "offsite_disk:"+d.ID); err != nil || label != d.Label {
		return "", Manifest{}, fmt.Errorf("backup: disk %s at %s is not registered with this master Pi", d.Label, path)
	}
	m, err := Snapshot(ctx, r.App.Store, r.Dir, r.Recipients, r.now(), r.Version)
	if err != nil {
		return "", m, err
	}
	if err := copySnapshot(r.Dir, m, d); err != nil {
		return "", m, err
	}
	if r.Blobs != nil {
		if _, err := backupBlobs(ctx, r.App.Store, r.Blobs, r.Dir, r.Recipients); err != nil {
			return "", m, err
		}
		if err := copyBlobs(r.Dir, d); err != nil {
			return "", m, err
		}
	}
	id, err := r.App.RecordBackup(ctx, domain.TypeBackupOffsiteWritten, domain.BackupWritten{
		DiskID: d.ID, DiskLabel: d.Label, File: m.File, SHA256: m.CipherSHA256, LocalOrder: m.LocalOrder, Heads: m.Heads,
	})
	return id, m, err
}

// ErrNothingToConfirm means the disk has no unconfirmed off-site copy.
var ErrNothingToConfirm = errors.New("backup: no unconfirmed off-site copy on that disk")

// ConfirmOffsite records that the disk with this label has been taken to
// the other building, confirming its newest unconfirmed copy.
func (r *Runner) ConfirmOffsite(ctx context.Context, actor app.Actor, label string) error {
	var id string
	err := r.App.Store.DB().QueryRowContext(ctx, `SELECT id FROM backups WHERE kind = 'offsite' AND disk_label = ?
		AND confirmed_at = '' ORDER BY written_at DESC LIMIT 1`, label).Scan(&id)
	if err != nil {
		return ErrNothingToConfirm
	}
	return r.App.ConfirmOffsite(ctx, actor, id)
}
