// Package backup makes, verifies, rotates and restores encrypted backups
// of the master Pi's database (DESIGN.md §8).
//
// A snapshot is a consistent copy (VACUUM INTO) that must pass SQLite's
// integrity check and a full re-verification of every signed event chain
// before it is encrypted with age and written out. Between snapshots, new
// events are exported every few minutes as encrypted segments; since
// events are append-only and individually signed, a restore is the
// latest snapshot plus the segments after it.
//
// Central holds only age recipients (public keys). The identities that
// decrypt backups stay offline with super users and in the sealed copy in
// the off-site building, so a stolen backup disk is useless on its own.
package backup

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"filippo.io/age"

	"pi-fleet/internal/fleetsync"
	"pi-fleet/internal/store"
)

// Manifest describes one snapshot. It sits unencrypted next to the
// snapshot so backups can be listed and checked without the identity.
type Manifest struct {
	Format          int              `json:"format"`
	File            string           `json:"file"`
	Created         time.Time        `json:"created"`
	SoftwareVersion string           `json:"software_version"`
	LocalOrder      int64            `json:"local_order"`
	Heads           map[string]int64 `json:"heads"`
	PlainSHA256     string           `json:"plain_sha256"`
	CipherSHA256    string           `json:"cipher_sha256"`
	CipherSize      int64            `json:"cipher_size"`
}

const (
	snapshotSuffix = ".db.age"
	manifestSuffix = ".json"
	stagingDir     = ".staging"
)

// ParseRecipients parses age recipients (age1...).
func ParseRecipients(keys []string) ([]age.Recipient, error) {
	if len(keys) == 0 {
		return nil, errors.New("backup: no age recipients configured; run pi-fleet backup-keygen and backup-config")
	}
	var out []age.Recipient
	for _, k := range keys {
		r, err := age.ParseX25519Recipient(strings.TrimSpace(k))
		if err != nil {
			return nil, fmt.Errorf("backup: recipient %q: %w", k, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// Snapshot writes a verified, encrypted snapshot of st into dir.
func Snapshot(ctx context.Context, st *store.Store, dir string, recipients []age.Recipient, now time.Time, version string) (Manifest, error) {
	if err := os.MkdirAll(filepath.Join(dir, stagingDir), 0o700); err != nil {
		return Manifest{}, err
	}
	name := "pifleet-" + now.UTC().Format("20060102T150405Z")
	plain := filepath.Join(dir, stagingDir, name+".db")
	os.Remove(plain)
	defer os.Remove(plain)
	if err := st.SnapshotTo(ctx, plain); err != nil {
		return Manifest{}, fmt.Errorf("backup: snapshot: %w", err)
	}
	m := Manifest{Format: 1, File: name + snapshotSuffix, Created: now.UTC(), SoftwareVersion: version}
	if err := verifyCopy(ctx, plain, &m); err != nil {
		return Manifest{}, err
	}
	sum, err := encryptFile(plain, filepath.Join(dir, m.File), recipients)
	if err != nil {
		return Manifest{}, err
	}
	if m.PlainSHA256, err = fileSHA256(plain); err != nil {
		return Manifest{}, err
	}
	m.CipherSHA256, m.CipherSize = sum.hash, sum.size
	return m, writeJSON(filepath.Join(dir, name+manifestSuffix), m)
}

// verifyCopy refuses a snapshot that fails integrity or chain checks,
// and records which events it holds.
func verifyCopy(ctx context.Context, path string, m *Manifest) error {
	cp, err := store.Open(path)
	if err != nil {
		return fmt.Errorf("backup: open copy: %w", err)
	}
	defer cp.Close()
	if err := cp.IntegrityCheck(ctx); err != nil {
		return fmt.Errorf("backup: copy failed: %w", err)
	}
	rep, err := cp.Verify(ctx)
	if err != nil {
		return err
	}
	if !rep.OK() {
		return fmt.Errorf("backup: copy failed chain verification: %v", rep.Problems[0])
	}
	m.Heads = map[string]int64{}
	rows, err := cp.DB().QueryContext(ctx, `SELECT chain_id, max(seq) FROM events GROUP BY chain_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var c string
		var s int64
		if err := rows.Scan(&c, &s); err != nil {
			return err
		}
		m.Heads[c] = s
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return cp.DB().QueryRowContext(ctx, `SELECT coalesce(max(local_order), 0) FROM events`).Scan(&m.LocalOrder)
}

type written struct {
	hash string
	size int64
}

// encryptFile encrypts src to dst atomically, then reads dst back to
// confirm what reached the disk is what was written.
func encryptFile(src, dst string, recipients []age.Recipient) (written, error) {
	in, err := os.Open(src)
	if err != nil {
		return written{}, err
	}
	defer in.Close()
	return writeEncrypted(in, dst, recipients)
}

func writeEncrypted(in io.Reader, dst string, recipients []age.Recipient) (written, error) {
	part := dst + ".part"
	out, err := os.OpenFile(part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return written{}, err
	}
	h := sha256.New()
	counter := &countWriter{w: io.MultiWriter(out, h)}
	enc, err := age.Encrypt(counter, recipients...)
	if err == nil {
		if _, err = io.Copy(enc, in); err == nil {
			err = enc.Close()
		}
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(part)
		return written{}, err
	}
	if err := os.Rename(part, dst); err != nil {
		return written{}, err
	}
	syncDir(filepath.Dir(dst))
	w := written{hash: hex.EncodeToString(h.Sum(nil)), size: counter.n}
	if got, err := fileSHA256(dst); err != nil || got != w.hash {
		return written{}, fmt.Errorf("backup: %s did not read back correctly (%v)", dst, err)
	}
	return w, nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	part := path + ".part"
	if err := os.WriteFile(part, b, 0o600); err != nil {
		return err
	}
	f, err := os.Open(part)
	if err == nil {
		f.Sync()
		f.Close()
	}
	if err := os.Rename(part, path); err != nil {
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}

// Snapshots lists the manifests in dir, newest first.
func Snapshots(dir string) ([]Manifest, error) {
	names, err := filepath.Glob(filepath.Join(dir, "pifleet-*"+manifestSuffix))
	if err != nil {
		return nil, err
	}
	var out []Manifest
	for _, n := range names {
		b, err := os.ReadFile(n)
		if err != nil {
			return nil, err
		}
		var m Manifest
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("backup: %s: %w", n, err)
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

// --- event segments ---

var segmentRE = regexp.MustCompile(`^events-(\d{12})-(\d{12})\.jsonl\.age$`)

func segmentName(from, to int64) string {
	return fmt.Sprintf("events-%012d-%012d.jsonl.age", from, to)
}

type segmentLine struct {
	LocalOrder int64               `json:"local_order"`
	Event      fleetsync.WireEvent `json:"event"`
}

// ExportEvents writes the events stored after local order `after` to an
// encrypted segment in dir, returning the new high-water mark (unchanged
// when there was nothing new).
func ExportEvents(ctx context.Context, st *store.Store, dir string, recipients []age.Recipient, after int64) (int64, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return after, err
	}
	evs, orders, err := st.EventsAfter(ctx, after, 50000)
	if err != nil || len(evs) == 0 {
		return after, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for i, e := range evs {
		if err := enc.Encode(segmentLine{LocalOrder: orders[i], Event: fleetsync.ToWire(e)}); err != nil {
			return after, err
		}
	}
	to := orders[len(orders)-1]
	if _, err := writeEncrypted(&buf, filepath.Join(dir, segmentName(after+1, to)), recipients); err != nil {
		return after, err
	}
	return to, nil
}

type segment struct {
	path     string
	from, to int64
}

func segments(dir string) ([]segment, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []segment
	for _, e := range entries {
		m := segmentRE.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		from, _ := strconv.ParseInt(m[1], 10, 64)
		to, _ := strconv.ParseInt(m[2], 10, 64)
		out = append(out, segment{filepath.Join(dir, e.Name()), from, to})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].from < out[j].from })
	return out, nil
}

// --- retention ---

// Prune applies the retention policy (DESIGN.md §8.2): the newest
// snapshot of each of the last 14 days, 8 weeks and 24 months, and of
// every year, forever. Event segments are kept while they follow the
// oldest kept snapshot. It returns the files removed.
func Prune(dir string, now time.Time) ([]string, error) {
	snaps, err := Snapshots(dir)
	if err != nil {
		return nil, err
	}
	keep := map[string]bool{}
	bucket := func(limit int, key func(time.Time) string) {
		seen := map[string]bool{}
		for _, m := range snaps { // newest first: the first in each bucket is its newest
			k := key(m.Created)
			if seen[k] {
				continue
			}
			seen[k] = true
			if limit > 0 && len(seen) > limit {
				return
			}
			keep[m.File] = true
		}
	}
	bucket(14, func(t time.Time) string { return t.Format("2006-01-02") })
	bucket(8, func(t time.Time) string { y, w := t.ISOWeek(); return fmt.Sprint(y, "-", w) })
	bucket(24, func(t time.Time) string { return t.Format("2006-01") })
	bucket(0, func(t time.Time) string { return t.Format("2006") })

	var removed []string
	oldestKept := int64(-1)
	for _, m := range snaps {
		if keep[m.File] {
			if oldestKept < 0 || m.LocalOrder < oldestKept {
				oldestKept = m.LocalOrder
			}
			continue
		}
		base := strings.TrimSuffix(m.File, snapshotSuffix)
		for _, f := range []string{m.File, base + manifestSuffix} {
			if err := os.Remove(filepath.Join(dir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return removed, err
			}
			removed = append(removed, f)
		}
	}
	if oldestKept >= 0 {
		segs, err := segments(filepath.Join(dir, "events"))
		if err != nil {
			return removed, err
		}
		for _, s := range segs {
			if s.to <= oldestKept {
				if err := os.Remove(s.path); err != nil {
					return removed, err
				}
				removed = append(removed, filepath.Base(s.path))
			}
		}
	}
	return removed, nil
}

// --- restore ---

// RestoreReport describes a completed restore.
type RestoreReport struct {
	Snapshot     Manifest
	EventsReplay int
	Verify       store.Report
}

// Restore decrypts a snapshot into dbPath (which must not exist), replays
// any later event segments from segmentsDir, rebuilds projections and
// verifies every chain (DESIGN.md §8.4 F). newApplier builds the
// projector for the restored node's identity.
func Restore(ctx context.Context, manifestPath string, identities []age.Identity, segmentsDir, dbPath string,
	newApplier func(localNodeID string) store.Applier) (RestoreReport, error) {
	var rep RestoreReport
	b, err := os.ReadFile(manifestPath)
	if err != nil {
		return rep, err
	}
	if err := json.Unmarshal(b, &rep.Snapshot); err != nil {
		return rep, err
	}
	if _, err := os.Stat(dbPath); err == nil {
		return rep, fmt.Errorf("backup: %s already exists; restore into an empty location", dbPath)
	}
	snapPath := filepath.Join(filepath.Dir(manifestPath), rep.Snapshot.File)
	if got, err := fileSHA256(snapPath); err != nil || got != rep.Snapshot.CipherSHA256 {
		return rep, fmt.Errorf("backup: %s does not match its manifest (%v)", snapPath, err)
	}
	if err := decryptTo(snapPath, dbPath, identities); err != nil {
		return rep, err
	}
	if got, err := fileSHA256(dbPath); err != nil || got != rep.Snapshot.PlainSHA256 {
		os.Remove(dbPath)
		return rep, fmt.Errorf("backup: decrypted snapshot does not match its manifest (%v)", err)
	}

	plain, err := store.Open(dbPath)
	if err != nil {
		return rep, err
	}
	ln, err := plain.LocalNode(ctx)
	plain.Close()
	if err != nil {
		return rep, err
	}
	st, err := store.Open(dbPath, store.WithApplier(newApplier(ln.NodeID)))
	if err != nil {
		return rep, err
	}
	defer st.Close()

	segs, err := segments(segmentsDir)
	if err != nil {
		return rep, err
	}
	next := rep.Snapshot.LocalOrder
	for _, s := range segs {
		if s.to <= next {
			continue
		}
		if s.from > next+1 {
			return rep, fmt.Errorf("backup: event segments are missing between local order %d and %d", next, s.from)
		}
		n, last, err := replaySegment(ctx, st, s.path, identities, next)
		if err != nil {
			return rep, err
		}
		rep.EventsReplay += n
		next = last
	}
	if err := st.Rebuild(ctx); err != nil {
		return rep, err
	}
	if rep.Verify, err = st.Verify(ctx); err != nil {
		return rep, err
	}
	if !rep.Verify.OK() {
		return rep, fmt.Errorf("backup: restored database failed verification: %v", rep.Verify.Problems[0])
	}
	return rep, st.IntegrityCheck(ctx)
}

func decryptTo(src, dst string, identities []age.Identity) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	r, err := age.Decrypt(in, identities...)
	if err != nil {
		return fmt.Errorf("backup: decrypt %s: %w", filepath.Base(src), err)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func replaySegment(ctx context.Context, st *store.Store, path string, identities []age.Identity, after int64) (int, int64, error) {
	in, err := os.Open(path)
	if err != nil {
		return 0, after, err
	}
	defer in.Close()
	r, err := age.Decrypt(in, identities...)
	if err != nil {
		return 0, after, fmt.Errorf("backup: decrypt %s: %w", filepath.Base(path), err)
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	n, last := 0, after
	for sc.Scan() {
		var line segmentLine
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			return n, last, err
		}
		if line.LocalOrder <= after {
			continue
		}
		e, err := fleetsync.FromWire(line.Event)
		if err != nil {
			return n, last, err
		}
		switch err := st.Ingest(ctx, e); {
		case err == nil:
			n++
		case errors.Is(err, store.ErrDuplicate):
		default:
			return n, last, fmt.Errorf("backup: replaying %s seq %d: %w", e.ChainID, e.Seq, err)
		}
		last = line.LocalOrder
	}
	return n, last, sc.Err()
}
