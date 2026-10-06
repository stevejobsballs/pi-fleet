package backup

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/google/uuid"

	"pi-fleet/internal/app"
	"pi-fleet/internal/blobs"
	"pi-fleet/internal/domain"
	"pi-fleet/internal/event"
	"pi-fleet/internal/fleetsync"
	"pi-fleet/internal/hlc"
	"pi-fleet/internal/keys"
	"pi-fleet/internal/password"
	"pi-fleet/internal/store"
)

var cheap = password.Params{Time: 1, MemoryKiB: 64, Threads: 1}

type env struct {
	t        *testing.T
	ctx      context.Context
	now      time.Time
	dir      string
	keys     keys.NodeKeys
	nodeID   string
	chainID  string
	app      *app.App
	super    app.Actor
	mid      app.Actor
	site     string
	loc      string
	identity *age.X25519Identity
	runner   *Runner
	// restoredPath is where the last restore wrote its database.
	restoredPath string
}

func (e *env) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) clock() time.Time { return e.now }

func (e *env) openCentral(dbPath string) {
	e.t.Helper()
	st, err := store.Open(dbPath, store.WithApplier(&domain.Projector{LocalNodeID: e.nodeID}))
	e.must(err)
	e.t.Cleanup(func() { st.Close() })
	last, err := st.MaxHLC(e.ctx)
	e.must(err)
	bs := &blobs.Store{Dir: filepath.Join(e.dir, "blobs")}
	e.app = &app.App{Store: st, Params: cheap, Now: e.clock, Blobs: bs, Author: &store.Author{NodeID: e.nodeID, ChainID: e.chainID,
		Signer: e.keys.EventSigner(), Clock: hlc.New(e.clock, last), Now: e.clock}}
	e.runner = &Runner{App: e.app, Dir: filepath.Join(e.dir, "backup-disk"), Recipients: []age.Recipient{e.identity.Recipient()},
		Version: "test", Now: e.clock, Blobs: bs}
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, ctx: context.Background(), now: time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC), dir: t.TempDir()}
	var err error
	e.keys, _, err = keys.LoadOrCreate(filepath.Join(e.dir, "keys"))
	e.must(err)
	e.nodeID, e.chainID = uuid.Must(uuid.NewV7()).String(), uuid.Must(uuid.NewV7()).String()
	e.identity, err = age.GenerateX25519Identity()
	e.must(err)
	db := filepath.Join(e.dir, "central.db")
	st, err := store.Open(db)
	e.must(err)
	e.must(st.TrustKey(e.ctx, e.nodeID, e.keys.Event.Public().(ed25519.PublicKey)))
	e.must(st.SetLocalNode(e.ctx, store.LocalNode{NodeID: e.nodeID, ChainID: e.chainID}))
	st.Close()
	e.openCentral(db)

	superID, err := e.app.BootstrapSuperUser(e.ctx, app.NewUser{Username: "admin", LegalName: "Ada", Email: "ada@example.org", IdentityVerification: "console"}, "tumbleweed-gasket-42")
	e.must(err)
	e.super = app.Actor{UserID: superID, SessionID: "s"}
	e.site, err = e.app.CreateSite(e.ctx, e.super, "NYC", "New York", "America/New_York")
	e.must(err)
	e.loc, err = e.app.CreateLocation(e.ctx, e.super, e.site, "", "Shop", "room")
	e.must(err)
	id, temp, err := e.app.CreateUser(e.ctx, e.super, app.NewUser{Username: "mona", LegalName: "Mona", Email: "mona@example.org", Role: domain.RoleMidTier, IdentityVerification: "badge"})
	e.must(err)
	e.mid = app.Actor{UserID: id, SessionID: "s"}
	e.must(e.app.ChangePassword(e.ctx, e.mid, temp, "brass-kettle-orchard-7"))
	return e
}

func (e *env) asset(tag string) string {
	e.t.Helper()
	id, err := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: tag, LocationID: e.loc, Manufacturer: "Fluke", Model: "M"})
	e.must(err)
	return id
}

func (e *env) restore(manifest, segments string) (*store.Store, RestoreReport) {
	e.t.Helper()
	db := filepath.Join(e.t.TempDir(), "restored.db")
	e.restoredPath = db
	rep, err := Restore(e.ctx, manifest, []age.Identity{e.identity}, segments, db,
		func(id string) store.Applier { return &domain.Projector{LocalNodeID: id} })
	e.must(err)
	st, err := store.Open(db, store.WithApplier(&domain.Projector{LocalNodeID: e.nodeID}))
	e.must(err)
	e.t.Cleanup(func() { st.Close() })
	return st, rep
}

func TestSnapshotEncryptedAndRestorable(t *testing.T) {
	e := newEnv(t)
	e.asset("A1")
	m, _, err := e.runner.Nightly(e.ctx)
	e.must(err)

	cipher, err := os.ReadFile(filepath.Join(e.runner.Dir, m.File))
	e.must(err)
	if bytes.Contains(cipher, []byte("SQLite format 3")) || bytes.Contains(cipher, []byte("Fluke")) {
		t.Fatal("snapshot is not encrypted")
	}
	if len(m.Heads) != 1 || m.Heads[e.chainID] == 0 {
		t.Fatalf("heads = %v", m.Heads)
	}

	// Work after the snapshot reaches the backup disk as an event segment.
	e.now = e.now.Add(time.Hour)
	e.asset("A2")
	n, err := e.runner.Export(e.ctx)
	e.must(err)
	if n == 0 {
		t.Fatal("nothing exported")
	}
	if again, err := e.runner.Export(e.ctx); err != nil || again != 0 {
		t.Fatalf("second export: %d %v", again, err)
	}

	manifest := filepath.Join(e.runner.Dir, strings.TrimSuffix(m.File, ".db.age")+".json")
	st, rep := e.restore(manifest, filepath.Join(e.runner.Dir, "events"))
	if rep.EventsReplay == 0 || !rep.Verify.OK() {
		t.Fatalf("restore report = %+v", rep)
	}
	var tags int
	e.must(st.DB().QueryRow(`SELECT count(*) FROM assets WHERE tag IN ('A1', 'A2')`).Scan(&tags))
	if tags != 2 {
		t.Fatalf("restored %d of 2 assets", tags)
	}

	// The wrong identity can't decrypt.
	other, _ := age.GenerateX25519Identity()
	_, err = Restore(e.ctx, manifest, []age.Identity{other}, "", filepath.Join(t.TempDir(), "x.db"),
		func(id string) store.Applier { return &domain.Projector{LocalNodeID: id} })
	if err == nil {
		t.Fatal("restore with the wrong identity succeeded")
	}
	// A damaged snapshot is caught before decryption.
	cipher[len(cipher)/2] ^= 1
	e.must(os.WriteFile(filepath.Join(e.runner.Dir, m.File), cipher, 0o600))
	_, err = Restore(e.ctx, manifest, []age.Identity{e.identity}, "", filepath.Join(t.TempDir(), "y.db"),
		func(id string) store.Applier { return &domain.Projector{LocalNodeID: id} })
	if err == nil || !strings.Contains(err.Error(), "does not match its manifest") {
		t.Fatalf("damaged snapshot: %v", err)
	}
}

func TestSnapshotRefusesTamperedDatabase(t *testing.T) {
	e := newEnv(t)
	e.asset("A1")
	// Someone with root edits a record behind the triggers.
	e.must(e.app.Store.Update(e.ctx, func(tx *store.Tx) error {
		if _, err := tx.ExecContext(e.ctx, `DROP TRIGGER events_no_update`); err != nil {
			return err
		}
		_, err := tx.ExecContext(e.ctx, `UPDATE events SET payload = replace(payload, 'Fluke', 'Acme') WHERE type = 'asset.registered'`)
		return err
	}))
	if _, _, err := e.runner.Nightly(e.ctx); err == nil || !strings.Contains(err.Error(), "chain verification") {
		t.Fatalf("snapshot of a tampered database: %v", err)
	}
	if snaps, _ := Snapshots(e.runner.Dir); len(snaps) != 0 {
		t.Fatal("a failed snapshot was kept")
	}
}

func TestPruneRetention(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 6, 2, 0, 0, 0, time.UTC)
	// Two snapshots a day for 900 days.
	for d := 0; d < 900; d++ {
		for _, h := range []int{2, 14} {
			at := now.AddDate(0, 0, -d).Add(time.Duration(h-2) * time.Hour)
			name := "pifleet-" + at.Format("20060102T150405Z")
			e := writeJSON(filepath.Join(dir, name+".json"), Manifest{File: name + ".db.age", Created: at, LocalOrder: int64(1000000 - d*10 - h)})
			if e != nil {
				t.Fatal(e)
			}
			os.WriteFile(filepath.Join(dir, name+".db.age"), nil, 0o600)
		}
	}
	if _, err := Prune(dir, now); err != nil {
		t.Fatal(err)
	}
	snaps, err := Snapshots(dir)
	if err != nil {
		t.Fatal(err)
	}
	days := map[string]bool{}
	years := map[int]bool{}
	for _, m := range snaps {
		if m.Created.Hour() != 14 {
			t.Fatalf("kept %v, which isn't the newest snapshot of its day", m.Created)
		}
		days[m.Created.Format("2006-01-02")] = true
		years[m.Created.Year()] = true
	}
	if len(days) != len(snaps) {
		t.Fatal("more than one snapshot kept for some day")
	}
	// 14 daily + 8 weekly + 24 monthly + one per year, overlapping.
	if len(snaps) < 30 || len(snaps) > 14+8+24+3 {
		t.Fatalf("kept %d snapshots", len(snaps))
	}
	for d := 0; d < 14; d++ {
		if !days[now.AddDate(0, 0, -d).Format("2006-01-02")] {
			t.Fatalf("daily snapshot %d days ago was pruned", d)
		}
	}
	if !years[2024] {
		t.Fatal("no snapshot kept for 2024")
	}
}

func TestOffsiteRotationAdvancesWatermark(t *testing.T) {
	e := newEnv(t)
	e.asset("A1")
	diskA := t.TempDir()
	if _, _, err := e.runner.WriteOffsite(e.ctx, diskA); err == nil {
		t.Fatal("wrote to an unregistered disk")
	}
	_, err := e.runner.RegisterOffsite(e.ctx, diskA, "OFFSITE-A")
	e.must(err)
	// A disk registered with another master Pi is refused.
	stranger := t.TempDir()
	_, err = RegisterDisk(stranger, "OFFSITE-A")
	e.must(err)
	if _, _, err := e.runner.WriteOffsite(e.ctx, stranger); err == nil {
		t.Fatal("wrote to a disk registered elsewhere")
	}

	id, m, err := e.runner.WriteOffsite(e.ctx, diskA)
	e.must(err)
	if got, err := fileSHA256(filepath.Join(diskA, m.File)); err != nil || got != m.CipherSHA256 {
		t.Fatalf("copy on disk: %v", err)
	}
	// Written, but not yet confirmed off-site: no watermark.
	if seq, _ := domain.DurableSeq(e.ctx, e.app.Store.DB(), e.chainID); seq != 0 {
		t.Fatalf("watermark before confirmation = %d", seq)
	}
	var rej *store.Rejection
	if err := e.runner.ConfirmOffsite(e.ctx, e.super, "OFFSITE-B"); !errors.Is(err, ErrNothingToConfirm) {
		t.Fatalf("confirm unknown disk: %v", err)
	}
	if err := e.app.ConfirmOffsite(e.ctx, app.Actor{UserID: "nobody", SessionID: "s"}, id); !errors.As(err, &rej) {
		t.Fatalf("confirm by unknown user: %v", err)
	}
	e.must(e.runner.ConfirmOffsite(e.ctx, e.mid, "OFFSITE-A"))
	if seq, _ := domain.DurableSeq(e.ctx, e.app.Store.DB(), e.chainID); seq != m.Heads[e.chainID] {
		t.Fatalf("watermark = %d, want %d", seq, m.Heads[e.chainID])
	}
	if last, _ := domain.LastOffsiteConfirmed(e.ctx, e.app.Store.DB()); !last.Equal(e.now) {
		t.Fatalf("last confirmed = %v", last)
	}
	if err := e.runner.ConfirmOffsite(e.ctx, e.mid, "OFFSITE-A"); !errors.Is(err, ErrNothingToConfirm) {
		t.Fatalf("double confirmation: %v", err)
	}
	// Each disk keeps only its newest few snapshots.
	for i := 0; i < keepOnDisk+2; i++ {
		e.now = e.now.Add(24 * time.Hour)
		_, _, err := e.runner.WriteOffsite(e.ctx, diskA)
		e.must(err)
	}
	if snaps, _ := Snapshots(diskA); len(snaps) != keepOnDisk {
		t.Fatalf("disk holds %d snapshots", len(snaps))
	}
}

// TestCentralLostRestoreFromOffsite is the disaster drill of DESIGN.md
// §8.4 E: the master Pi's building is lost, it is restored from the last
// off-site disk alone, and the employee Pis re-send everything newer.
func TestCentralLostRestoreFromOffsite(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewServer((&fleetsync.Server{App: e.app, CentralKey: e.keys.Event, Now: e.clock, Logf: t.Logf}).Handler())
	defer srv.Close()
	pi := e.newPi(srv.URL)
	a := e.asset("A1")
	pi.sync()

	// Week one: offline work, synced, then an off-site copy confirmed.
	for i := 0; i < 3; i++ {
		_, _, err := pi.app.OpenWorkOrder(e.ctx, pi.user, app.NewWorkOrder{Type: "corrective", AssetID: a, Priority: "low", Title: "week one"})
		e.must(err)
	}
	pi.sync()
	disk := t.TempDir()
	_, err := e.runner.RegisterOffsite(e.ctx, disk, "OFFSITE-A")
	e.must(err)
	_, m, err := e.runner.WriteOffsite(e.ctx, disk)
	e.must(err)
	e.must(e.runner.ConfirmOffsite(e.ctx, e.mid, "OFFSITE-A"))

	// Week two: more work reaches central, but no new off-site copy.
	for i := 0; i < 2; i++ {
		_, _, err := pi.app.OpenWorkOrder(e.ctx, pi.user, app.NewWorkOrder{Type: "corrective", AssetID: a, Priority: "low", Title: "week two"})
		e.must(err)
	}
	e.now = e.now.Add(40 * 24 * time.Hour)
	r := pi.sync()
	// 40 days on, the Pi purges exactly what the off-site copy holds and
	// keeps week two, which only central has.
	durable := m.Heads[pi.client.ChainID]
	var oldest int64
	pi.app.Store.DB().QueryRow(`SELECT min(seq) FROM events WHERE chain_id = ?`, pi.client.ChainID).Scan(&oldest)
	if r.Purged != durable || oldest != durable+1 {
		t.Fatalf("purged %d, oldest kept seq %d; durable watermark %d", r.Purged, oldest, durable)
	}

	// Disaster: central and its backup disk are gone. Restore from the
	// off-site disk only (no event segments).
	srv.Close()
	manifest := filepath.Join(disk, strings.TrimSuffix(m.File, ".db.age")+".json")
	restored, _ := e.restore(manifest, "")
	var weekTwo int
	restored.DB().QueryRow(`SELECT count(*) FROM work_orders WHERE title = 'week two'`).Scan(&weekTwo)
	if weekTwo != 0 {
		t.Fatal("the off-site copy shouldn't hold week two")
	}
	// Bring central back on the restored database.
	restored.Close()
	e.openCentral(e.restoredPath)
	srv2 := httptest.NewServer((&fleetsync.Server{App: e.app, CentralKey: e.keys.Event, Now: e.clock, Logf: t.Logf}).Handler())
	defer srv2.Close()
	pi.client.BaseURL = srv2.URL

	// The Pi notices central is behind and re-sends week two.
	r = pi.sync()
	if r.Pushed != 2 {
		t.Fatalf("re-pushed %d events, want 2", r.Pushed)
	}
	e.app.Store.DB().QueryRow(`SELECT count(*) FROM work_orders WHERE title = 'week two'`).Scan(&weekTwo)
	if weekTwo != 2 {
		t.Fatalf("after re-sync central has %d week-two work orders", weekTwo)
	}
	rep, err := e.app.Store.Verify(e.ctx)
	e.must(err)
	if !rep.OK() {
		t.Fatalf("verify: %v", rep.Problems)
	}
}

// --- an employee Pi for the drill ---

type pi struct {
	e      *env
	app    *app.App
	client *fleetsync.Client
	user   app.Actor
}

func (e *env) newPi(url string) *pi {
	e.t.Helper()
	id, temp, err := e.app.CreateUser(e.ctx, e.super, app.NewUser{Username: "tess", LegalName: "Tess", Email: "tess@example.org", Role: domain.RoleUser, HomeSites: []string{e.site}, IdentityVerification: "badge"})
	e.must(err)
	dir := e.t.TempDir()
	k, _, err := keys.LoadOrCreate(filepath.Join(dir, "keys"))
	e.must(err)
	nodeID, chainID := uuid.Must(uuid.NewV7()).String(), uuid.Must(uuid.NewV7()).String()
	proj := &domain.Projector{LocalNodeID: nodeID}
	st, err := store.Open(filepath.Join(dir, "pi.db"), store.WithApplier(proj))
	e.must(err)
	e.t.Cleanup(func() { st.Close() })
	e.must(st.TrustKey(e.ctx, nodeID, k.Event.Public().(ed25519.PublicKey)))
	author := &store.Author{NodeID: nodeID, ChainID: chainID, Signer: k.EventSigner(), Clock: hlc.New(e.clock, 0), Now: e.clock}
	_, err = st.Append(e.ctx, author, event.Draft{ActorUserID: "system:init", ActorSessionID: "s", Type: event.TypeChainStarted,
		EntityType: "chain", EntityID: chainID, SchemaVersion: 1, Payload: []byte(`{}`)})
	e.must(err)
	p := &pi{e: e, app: &app.App{Store: st, Author: author, Params: cheap, Now: e.clock}, user: app.Actor{UserID: id, SessionID: "pi"}}
	p.client = &fleetsync.Client{BaseURL: url, Store: st, Keys: k, NodeID: nodeID, ChainID: chainID, Now: e.clock, Wipe: func() error { return nil }}
	act, err := p.client.Activate(e.ctx, "tess", temp, "copper-ladder-sunrise", cheap)
	e.must(err)
	proj.CentralNodeID = act.CentralNodeID
	e.must(e.app.ConfirmNode(e.ctx, e.super, nodeID, act.PairingWords))
	return p
}

func (p *pi) sync() fleetsync.Report {
	p.e.t.Helper()
	r, err := p.client.Sync(p.e.ctx)
	p.e.must(err)
	return r
}

func TestAttachmentFilesBackedUpAndRestored(t *testing.T) {
	e := newEnv(t)
	a := e.asset("A1")
	cert := []byte("%PDF-1.7 certificate 4411")
	id, err := e.app.AddAttachment(e.ctx, e.super, domain.EntityAsset, a, "cert.pdf", "", cert)
	e.must(err)
	att, _ := domain.GetAttachment(e.ctx, e.app.Store.DB(), id)

	_, _, err = e.runner.Nightly(e.ctx)
	e.must(err)
	backed := filepath.Join(e.runner.Dir, "blobs", att.SHA256+".age")
	enc, err := os.ReadFile(backed)
	if err != nil || bytes.Contains(enc, []byte("certificate")) {
		t.Fatalf("file not backed up encrypted: %v", err)
	}
	disk := t.TempDir()
	_, err = e.runner.RegisterOffsite(e.ctx, disk, "OFFSITE-A")
	e.must(err)
	_, _, err = e.runner.WriteOffsite(e.ctx, disk)
	e.must(err)

	restored := &blobs.Store{Dir: t.TempDir()}
	n, err := RestoreBlobs(disk, []age.Identity{e.identity}, restored)
	e.must(err)
	if got, _ := restored.Get(att.SHA256); n != 1 || string(got) != string(cert) {
		t.Fatalf("restored %d files, content %q", n, got)
	}

	// Purged for patient information: gone from the backup disk, and from
	// the off-site disk at its next rotation.
	e.must(e.app.DetachAttachment(e.ctx, e.super, id, "patient label visible", true))
	_, _, err = e.runner.Nightly(e.ctx)
	e.must(err)
	if _, err := os.Stat(backed); !os.IsNotExist(err) {
		t.Fatal("purged file still on the backup disk")
	}
	_, _, err = e.runner.WriteOffsite(e.ctx, disk)
	e.must(err)
	if _, err := os.Stat(filepath.Join(disk, "blobs", att.SHA256+".age")); !os.IsNotExist(err) {
		t.Fatal("purged file still on the off-site disk")
	}
}
