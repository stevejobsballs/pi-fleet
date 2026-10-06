package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"

	"pi-fleet/internal/blobs"
	"pi-fleet/internal/store"
)

// Attachment files are immutable and content-addressed, so each is
// encrypted once into <backup>/blobs/<sha256>.age and copied as-is to
// off-site disks. Purged files are removed from both (DESIGN.md §3.6).

const blobsDir = "blobs"

// blobStates returns the hashes of live and of purged attachment files.
func blobStates(ctx context.Context, st *store.Store) (live, purged []string, err error) {
	rows, err := st.DB().QueryContext(ctx, `SELECT sha256, sum(status != 'purged') > 0 FROM attachments GROUP BY sha256`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sha string
		var isLive bool
		if err := rows.Scan(&sha, &isLive); err != nil {
			return nil, nil, err
		}
		if isLive {
			live = append(live, sha)
		} else {
			purged = append(purged, sha)
		}
	}
	return live, purged, rows.Err()
}

// backupBlobs encrypts live files not yet in dir and removes purged ones.
// It returns how many files it added.
func backupBlobs(ctx context.Context, st *store.Store, src *blobs.Store, dir string, recipients []age.Recipient) (int, error) {
	live, purged, err := blobStates(ctx, st)
	if err != nil {
		return 0, err
	}
	out := filepath.Join(dir, blobsDir)
	if err := os.MkdirAll(out, 0o700); err != nil {
		return 0, err
	}
	added := 0
	for _, sha := range live {
		dst := filepath.Join(out, sha+".age")
		if _, err := os.Stat(dst); err == nil {
			continue
		}
		data, err := src.Get(sha)
		if errors.Is(err, blobs.ErrNotFound) {
			continue // not uploaded from its Pi yet
		}
		if err != nil {
			return added, err
		}
		if _, err := writeEncrypted(bytes.NewReader(data), dst, recipients); err != nil {
			return added, err
		}
		added++
	}
	for _, sha := range purged {
		if err := os.Remove(filepath.Join(out, sha+".age")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return added, err
		}
	}
	return added, nil
}

// copyBlobs mirrors dir's encrypted files onto an off-site disk, adding
// new ones (checked by reading back) and removing ones no longer there.
func copyBlobs(dir string, d Disk) error {
	src := filepath.Join(dir, blobsDir)
	dst := filepath.Join(d.Path, blobsDir)
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	want := map[string]bool{}
	entries, err := os.ReadDir(src)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".age") {
			continue
		}
		want[e.Name()] = true
		target := filepath.Join(dst, e.Name())
		if _, err := os.Stat(target); err == nil {
			continue
		}
		if err := copyFile(filepath.Join(src, e.Name()), target); err != nil {
			return err
		}
		a, err1 := fileSHA256(filepath.Join(src, e.Name()))
		b, err2 := fileSHA256(target)
		if err1 != nil || err2 != nil || a != b {
			return fmt.Errorf("backup: %s did not copy correctly to %s", e.Name(), d.Label)
		}
	}
	have, err := os.ReadDir(dst)
	if err != nil {
		return err
	}
	for _, e := range have {
		if strings.HasSuffix(e.Name(), ".age") && !want[e.Name()] {
			os.Remove(filepath.Join(dst, e.Name()))
		}
	}
	return nil
}

// RestoreBlobs decrypts the files in a backup's blobs directory into dst,
// checking each against its hash. It returns how many it restored.
func RestoreBlobs(dir string, identities []age.Identity, dst *blobs.Store) (int, error) {
	entries, err := os.ReadDir(filepath.Join(dir, blobsDir))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		sha, ok := strings.CutSuffix(e.Name(), ".age")
		if !ok {
			continue
		}
		f, err := os.Open(filepath.Join(dir, blobsDir, e.Name()))
		if err != nil {
			return n, err
		}
		r, err := age.Decrypt(f, identities...)
		if err != nil {
			f.Close()
			return n, fmt.Errorf("backup: decrypt %s: %w", e.Name(), err)
		}
		data, err := io.ReadAll(io.LimitReader(r, blobs.MaxSize+1))
		f.Close()
		if err != nil {
			return n, err
		}
		if err := dst.PutExpected(sha, data); err != nil {
			return n, fmt.Errorf("backup: %s: %w", e.Name(), err)
		}
		n++
	}
	return n, nil
}
