package standby

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"pi-fleet/internal/app"
	"pi-fleet/internal/blobs"
	"pi-fleet/internal/domain"
	"pi-fleet/internal/event"
	"pi-fleet/internal/hlc"
	"pi-fleet/internal/password"
	"pi-fleet/internal/store"
)

type master struct {
	t      *testing.T
	ctx    context.Context
	dir    string
	app    *app.App
	super  app.Actor
	loc    string
	nodeID string
	srv    *Server
	http   *httptest.Server
}

func newMaster(t *testing.T) *master {
	t.Helper()
	m := &master{t: t, ctx: context.Background(), dir: t.TempDir()}
	now := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	pub, priv, _ := ed25519.GenerateKey(nil)
	m.nodeID = uuid.Must(uuid.NewV7()).String()
	author := &store.Author{NodeID: m.nodeID, ChainID: uuid.Must(uuid.NewV7()).String(),
		Signer: event.Signer{KeyID: event.KeyID(pub), Key: priv}, Clock: hlc.New(clock, 0), Now: clock}
	st, err := store.Open(filepath.Join(m.dir, "pi-fleet.db"), store.WithApplier(&domain.Projector{LocalNodeID: m.nodeID}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	st.TrustKey(m.ctx, m.nodeID, pub)
	m.app = &app.App{Store: st, Author: author, Params: password.Params{Time: 1, MemoryKiB: 64, Threads: 1}, Now: clock,
		Blobs: &blobs.Store{Dir: filepath.Join(m.dir, "blobs")}}
	id, err := m.app.BootstrapSuperUser(m.ctx, app.NewUser{Username: "admin", LegalName: "Ada Admin", Email: "ada@example.org", IdentityVerification: "console"}, "tumbleweed-gasket-42")
	if err != nil {
		t.Fatal(err)
	}
	m.super = app.Actor{UserID: id, SessionID: "t"}
	site, _ := m.app.CreateSite(m.ctx, m.super, "NYC", "New York", "America/New_York")
	m.loc, _ = m.app.CreateLocation(m.ctx, m.super, site, "", "Biomed shop", "room")
	for d, files := range map[string][]string{"keys": {"event.key", "transport.key"}, "tls": {"cert.pem", "key.pem"}} {
		os.MkdirAll(filepath.Join(m.dir, d), 0o700)
		for _, f := range files {
			os.WriteFile(filepath.Join(m.dir, d, f), []byte("secret "+f), 0o600)
		}
	}
	m.srv = &Server{Store: st, DataDir: m.dir, Blobs: m.app.Blobs, Now: clock}
	m.http = httptest.NewServer(m.srv.Handler())
	t.Cleanup(m.http.Close)
	return m
}

func (m *master) must(err error) {
	m.t.Helper()
	if err != nil {
		m.t.Fatal(err)
	}
}

// standbyOf pairs a standby with the master.
func (m *master) standbyOf() *Follower {
	m.t.Helper()
	code, _, err := m.srv.NewCode(m.ctx)
	m.must(err)
	token, err := Pair(m.ctx, http.DefaultClient, m.http.URL, strings.ToLower(code))
	m.must(err)
	dir := m.t.TempDir()
	f := &Follower{DataDir: dir, Link: Link{Master: m.http.URL, Token: token}, HTTP: http.DefaultClient,
		Blobs: &blobs.Store{Dir: filepath.Join(dir, "blobs")},
		Open: func(ctx context.Context) (*store.Store, error) {
			return store.Open(filepath.Join(dir, "pi-fleet.db"), store.WithApplier(&domain.Projector{LocalNodeID: m.nodeID}))
		}}
	m.t.Cleanup(func() { f.Close() })
	return f
}

// tables dumps every projection table and the event log, to compare.
func tables(t *testing.T, db *sql.DB) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, table := range []string{"events", "users", "sites", "locations", "assets", "work_orders", "wo_leases", "pm_schedules",
		"attachments", "event_flags", "node_keys", "procedures", "checklist_results", "models"} {
		rows, err := db.Query(`SELECT * FROM ` + table + ` ORDER BY 1, 2`)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			rows.Scan(ptrs...)
			out[table] = append(out[table], fmt.Sprint(vals...))
		}
		rows.Close()
	}
	return out
}

func same(t *testing.T, what string, m *master, f *Follower) {
	t.Helper()
	want, got := tables(t, m.app.Store.DB()), tables(t, f.st.DB())
	for k := range want {
		if !reflect.DeepEqual(want[k], got[k]) {
			t.Fatalf("%s: %s differs:\nmaster:  %v\nstandby: %v", what, k, want[k], got[k])
		}
	}
}

func TestStandbyKeepsAnExactCopy(t *testing.T) {
	m := newMaster(t)
	pump, err := m.app.RegisterAsset(m.ctx, m.super, domain.AssetRegistered{Tag: "PUMP-1", LocationID: m.loc, Manufacturer: "Baxter", Model: "Sigma"})
	m.must(err)
	wo, number, err := m.app.OpenWorkOrder(m.ctx, m.super, app.NewWorkOrder{Type: "corrective", AssetID: pump, Priority: "high", Title: "Alarm",
		Problem: "Alarm on patient John Smith"})
	m.must(err)
	_, err = m.app.AddAttachment(m.ctx, m.super, domain.EntityAsset, pump, "manual.pdf", "service manual", []byte("%PDF-1.4 manual"))
	m.must(err)
	m.must(m.app.Store.SetConfig(m.ctx, "report_url", "https://pifleet.hospital.org"))

	f := m.standbyOf()
	m.must(f.Step(m.ctx)) // the snapshot, then nothing new
	same(t, "after the snapshot", m, f)
	for _, p := range []string{"keys/event.key", "tls/key.pem"} {
		if b, err := os.ReadFile(filepath.Join(f.DataDir, p)); err != nil || string(b) != "secret "+filepath.Base(p) {
			t.Fatalf("%s not copied: %v", p, err)
		}
	}
	if v, _ := f.st.Config(m.ctx, "report_url"); v != "https://pifleet.hospital.org" {
		t.Fatalf("setting not copied: %q", v)
	}
	// The master's pairing isn't copied: on taking over, none is paired.
	var n int
	f.st.DB().QueryRow(`SELECT count(*) FROM node_config WHERE key LIKE 'standby_%' AND value != ''`).Scan(&n)
	if n != 0 || !m.srv.Paired(m.ctx) {
		t.Fatalf("pairing copied (%d) or lost on the master", n)
	}

	// More work on the master, including a redaction: copied as events.
	_, err = m.app.RegisterAsset(m.ctx, m.super, domain.AssetRegistered{Tag: "VENT-1", LocationID: m.loc, Manufacturer: "Hamilton", Model: "C6"})
	m.must(err)
	_, err = m.app.AddAttachment(m.ctx, m.super, domain.EntityWorkOrder, wo, "photo.png", "", pngBytes())
	m.must(err)
	var opened string
	m.must(m.app.Store.DB().QueryRow(`SELECT event_id FROM events WHERE type = ? AND entity_id = ?`, domain.TypeWorkOrderOpened, wo).Scan(&opened))
	m.must(m.app.Redact(m.ctx, m.super, opened, "contained a patient's name", domain.WorkOrderOpened{Number: number, Type: "corrective",
		AssetID: pump, Priority: "high", Title: "Alarm", Problem: "Alarm during use [redacted]"}))
	m.must(m.app.Store.SetConfig(m.ctx, "require_node_version", "v0.16.0"))
	f.rounds = 0 // settings and attachments due this round
	m.must(f.Step(m.ctx))
	same(t, "after more work", m, f)
	if v, _ := f.st.Config(m.ctx, "require_node_version"); v != "v0.16.0" {
		t.Fatalf("changed setting not copied: %q", v)
	}
	var shas []string
	rows, _ := m.app.Store.DB().Query(`SELECT sha256 FROM attachments`)
	for rows.Next() {
		var s string
		rows.Scan(&s)
		shas = append(shas, s)
	}
	rows.Close()
	for _, s := range shas {
		if !f.Blobs.Has(s) {
			t.Fatalf("attachment %s not copied", s)
		}
	}
	// The master sees the standby, up to date.
	if st := m.srv.Status(m.ctx); !st.Paired || st.Seen.IsZero() || st.Behind != 0 {
		t.Fatalf("status: %+v", st)
	}
	if r := ReadReport(f.DataDir); r.LastOK.IsZero() || r.Error != "" || r.At != r.Head || r.Head == 0 {
		t.Fatalf("report: %+v", r)
	}
	// A copy that has drifted (here: a later snapshot taken from a
	// different master) is refused rather than mixed.
	other := newMaster(t)
	f.Link.Master = other.http.URL
	other.srv.Store.SetConfig(other.ctx, configToken, hashOf(f.Link.Token))
	if err := f.Step(m.ctx); err == nil {
		t.Fatal("followed a different master")
	}
	if r := ReadReport(f.DataDir); r.Error == "" {
		t.Fatalf("failure not reported: %+v", r)
	}
}

func TestPairingAndAccess(t *testing.T) {
	m := newMaster(t)
	if _, err := Pair(m.ctx, http.DefaultClient, m.http.URL, "AAAA-BBBB-CCCC"); err == nil || !strings.Contains(err.Error(), "no pairing code") {
		t.Fatalf("paired without a code: %v", err)
	}
	code, until, err := m.srv.NewCode(m.ctx)
	m.must(err)
	if until.Sub(time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)) != CodeLifetime || len(code) != 14 {
		t.Fatalf("code %q until %v", code, until)
	}
	if _, err := Pair(m.ctx, http.DefaultClient, m.http.URL, "AAAA-BBBB-CCCC"); err == nil || !strings.Contains(err.Error(), "isn't right") {
		t.Fatalf("wrong code: %v", err)
	}
	token, err := Pair(m.ctx, http.DefaultClient, m.http.URL, code)
	m.must(err)
	if _, err := Pair(m.ctx, http.DefaultClient, m.http.URL, code); err == nil {
		t.Fatal("a code worked twice")
	}
	get := func(tok string) int {
		req, _ := http.NewRequest("GET", m.http.URL+PathEvents+"?after=0", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		m.must(err)
		resp.Body.Close()
		return resp.StatusCode
	}
	if get(token) != 200 || get("wrong") != 401 || get("") != 401 {
		t.Fatal("access not limited to the paired standby")
	}
	m.must(m.srv.Unpair(m.ctx))
	if get(token) != 401 || m.srv.Paired(m.ctx) {
		t.Fatal("unpaired standby still has access")
	}
	// Codes can't be guessed by trying many.
	m.srv.NewCode(m.ctx)
	for range 10 {
		Pair(m.ctx, http.DefaultClient, m.http.URL, "AAAA-BBBB-CCCC")
	}
	if _, err := Pair(m.ctx, http.DefaultClient, m.http.URL, "AAAA-BBBB-CCCC"); err == nil || !strings.Contains(err.Error(), "too many") {
		t.Fatalf("no limit on tries: %v", err)
	}
}

// pngBytes is a tiny valid PNG.
func pngBytes() []byte {
	var b bytes.Buffer
	png.Encode(&b, image.NewGray(image.Rect(0, 0, 2, 2)))
	return b.Bytes()
}

func TestPlainErrors(t *testing.T) {
	_, err := http.Get("http://127.0.0.1:1/")
	if got := plain(err); got != "can't reach the master Pi" {
		t.Fatalf("%q", got)
	}
	if got := plain(fmt.Errorf("the copy here doesn't match the master")); got != "the copy here doesn't match the master" {
		t.Fatalf("%q", got)
	}
}
