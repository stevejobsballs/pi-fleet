package fleetsync

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"pi-fleet/internal/app"
	"pi-fleet/internal/blobs"
	"pi-fleet/internal/domain"
	"pi-fleet/internal/event"
	"pi-fleet/internal/fleetca"
	"pi-fleet/internal/hlc"
	"pi-fleet/internal/httpsig"
	"pi-fleet/internal/keys"
	"pi-fleet/internal/password"
	"pi-fleet/internal/store"
)

var cheap = password.Params{Time: 1, MemoryKiB: 64, Threads: 1}

// fleet is a central with an HTTP server and a test-controlled clock.
type fleet struct {
	t     *testing.T
	ctx   context.Context
	now   time.Time
	app   *app.App
	srv   *httptest.Server
	super app.Actor
	mid   app.Actor
	site  string
	loc   string
}

func (f *fleet) clock() time.Time { return f.now }

func (f *fleet) must(err error) {
	f.t.Helper()
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *fleet) must2(s string, err error) string {
	f.t.Helper()
	f.must(err)
	return s
}

func newAuthor(t *testing.T, k keys.NodeKeys, nodeID, chainID string, clock func() time.Time) *store.Author {
	return &store.Author{NodeID: nodeID, ChainID: chainID, Signer: k.EventSigner(), Clock: hlc.New(clock, 0), Now: clock}
}

func newFleet(t *testing.T) *fleet {
	t.Helper()
	f := &fleet{t: t, ctx: context.Background(), now: time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	k, _, err := keys.LoadOrCreate(filepath.Join(dir, "keys"))
	f.must(err)
	centralID := uuid.Must(uuid.NewV7()).String()
	st, err := store.Open(filepath.Join(dir, "central.db"), store.WithApplier(&domain.Projector{LocalNodeID: centralID}))
	f.must(err)
	t.Cleanup(func() { st.Close() })
	f.must(st.TrustKey(f.ctx, centralID, k.Event.Public().(ed25519.PublicKey)))
	f.app = &app.App{Store: st, Author: newAuthor(t, k, centralID, uuid.Must(uuid.NewV7()).String(), f.clock), Params: cheap, Now: f.clock}
	superID, err := f.app.BootstrapSuperUser(f.ctx, app.NewUser{Username: "admin", LegalName: "Ada Admin", Email: "ada@example.org", IdentityVerification: "console"}, "tumbleweed-gasket-42")
	f.must(err)
	f.super = app.Actor{UserID: superID, SessionID: "s"}
	f.site = f.must2(f.app.CreateSite(f.ctx, f.super, "NYC", "New York", "America/New_York"))
	f.loc = f.must2(f.app.CreateLocation(f.ctx, f.super, f.site, "", "Shop", "room"))
	f.mid = f.activeCentralUser("mona", domain.RoleMidTier)

	f.app.Blobs = &blobs.Store{Dir: filepath.Join(dir, "blobs")}
	ca, err := fleetca.LoadOrCreate(filepath.Join(dir, "keys"), "test")
	f.must(err)
	srv := &Server{App: f.app, CentralKey: k.Event, Now: f.clock, Logf: t.Logf, Blobs: f.app.Blobs, CA: ca}
	f.srv = httptest.NewServer(srv.Handler())
	t.Cleanup(f.srv.Close)
	return f
}

// activeCentralUser makes a user who works on central's web UI only.
func (f *fleet) activeCentralUser(username, role string) app.Actor {
	f.t.Helper()
	id, temp, err := f.app.CreateUser(f.ctx, f.super, app.NewUser{Username: username, LegalName: username, Email: username + "@example.org", Role: role, IdentityVerification: "in person"})
	f.must(err)
	a := app.Actor{UserID: id, SessionID: "s"}
	f.must(f.app.ChangePassword(f.ctx, a, temp, "brass-kettle-orchard-7"))
	return a
}

// pi is an employee's Pi.
type pi struct {
	f      *fleet
	dir    string
	proj   *domain.Projector
	app    *app.App
	client *Client
	wiped  bool
	user   app.Actor
}

func (f *fleet) newPi() *pi {
	f.t.Helper()
	p := &pi{f: f, dir: f.t.TempDir()}
	k, _, err := keys.LoadOrCreate(filepath.Join(p.dir, "keys"))
	f.must(err)
	nodeID, chainID := uuid.Must(uuid.NewV7()).String(), uuid.Must(uuid.NewV7()).String()
	p.proj = &domain.Projector{LocalNodeID: nodeID}
	st, err := store.Open(filepath.Join(p.dir, "pi.db"), store.WithApplier(p.proj))
	f.must(err)
	f.t.Cleanup(func() { st.Close() })
	f.must(st.TrustKey(f.ctx, nodeID, k.Event.Public().(ed25519.PublicKey)))
	f.must(st.SetLocalNode(f.ctx, store.LocalNode{NodeID: nodeID, ChainID: chainID}))
	author := newAuthor(f.t, k, nodeID, chainID, f.clock)
	author.ClockState = func() event.ClockState { return ClockState(f.ctx, st, f.now) }
	p.app = &app.App{Store: st, Author: author, Params: cheap, Now: f.clock, Blobs: &blobs.Store{Dir: filepath.Join(p.dir, "blobs")}, QueueUploads: true}
	_, err = st.Append(f.ctx, author, event.Draft{ActorUserID: "system:init", ActorSessionID: "system", Type: event.TypeChainStarted,
		EntityType: "chain", EntityID: chainID, SchemaVersion: 1, Payload: []byte(`{}`)})
	f.must(err)
	p.client = &Client{BaseURL: f.srv.URL, Store: st, Keys: k, NodeID: nodeID, ChainID: chainID, Version: "test",
		Now: f.clock, Wipe: func() error { p.wiped = true; return nil }, Blobs: p.app.Blobs}
	return p
}

// enrol creates a user on central and activates and confirms a Pi for them.
func (f *fleet) enrol(username, role string) *pi {
	f.t.Helper()
	id, temp, err := f.app.CreateUser(f.ctx, f.super, app.NewUser{Username: username, LegalName: username, Email: username + "@example.org", Role: role, HomeSites: []string{f.site}, IdentityVerification: "in person"})
	f.must(err)
	p := f.newPi()
	act, err := p.client.Activate(f.ctx, username, temp, "copper-ladder-sunrise", cheap)
	f.must(err)
	p.proj.CentralNodeID = act.CentralNodeID
	f.must(f.app.ConfirmNode(f.ctx, f.super, p.client.NodeID, act.PairingWords))
	p.user = app.Actor{UserID: id, SessionID: "pi"}
	p.sync()
	return p
}

func (p *pi) sync() Report {
	p.f.t.Helper()
	r, err := p.client.Sync(p.f.ctx)
	p.f.must(err)
	return r
}

func (f *fleet) asset(tag string) string {
	return f.must2(f.app.RegisterAsset(f.ctx, f.super, domain.AssetRegistered{Tag: tag, LocationID: f.loc, Manufacturer: "Fluke", Model: "ESA615"}))
}

func TestActivationAndConfirmation(t *testing.T) {
	f := newFleet(t)
	id, temp, err := f.app.CreateUser(f.ctx, f.super, app.NewUser{Username: "tess", LegalName: "Tess Tech", Email: "tess@example.org", Role: domain.RoleUser, HomeSites: []string{f.site}, IdentityVerification: "badge"})
	f.must(err)
	p := f.newPi()

	if _, err := p.client.Activate(f.ctx, "tess", "WRONG-PASS-WORD-0000", "copper-ladder-sunrise", cheap); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("wrong one-time password: %v", err)
	}
	if _, err := p.client.Activate(f.ctx, "nobody", temp, "copper-ladder-sunrise", cheap); err == nil {
		t.Fatal("unknown user activated")
	}
	act, err := p.client.Activate(f.ctx, "tess", temp, "copper-ladder-sunrise", cheap)
	f.must(err)
	if act.PairingWords != p.client.PairingWords() || len(strings.Fields(act.PairingWords)) != 6 {
		t.Fatalf("pairing words %q", act.PairingWords)
	}
	p.proj.CentralNodeID = act.CentralNodeID

	// Pending: no sync, no snapshot, and the user can't log in anywhere.
	if s, err := p.client.ActivationStatus(f.ctx); err != nil || s != domain.NodeStatusPending {
		t.Fatalf("status = %q, %v", s, err)
	}
	if _, err := p.client.Sync(f.ctx); !errors.Is(err, ErrNotActivated) {
		t.Fatalf("sync while pending: %v", err)
	}
	if _, err := f.app.Authenticate(f.ctx, "tess", "copper-ladder-sunrise"); !errors.Is(err, app.ErrBadCredentials) {
		t.Fatalf("chosen password usable before confirmation: %v", err)
	}

	// A second Pi can't claim the same account (one Pi per user).
	p2 := f.newPi()
	if _, err := p2.client.Activate(f.ctx, "tess", temp, "copper-ladder-sunrise", cheap); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("second Pi for the same user: %v", err)
	}

	// The super user must read back the right words.
	var rej *store.Rejection
	if err := f.app.ConfirmNode(f.ctx, f.super, p.client.NodeID, "acorn acorn acorn acorn acorn acorn"); !errors.As(err, &rej) {
		t.Fatalf("confirm with wrong words: %v", err)
	}
	f.must(f.app.ConfirmNode(f.ctx, f.super, p.client.NodeID, act.PairingWords))
	if s, err := p.client.ActivationStatus(f.ctx); err != nil || s != domain.NodeStatusActive {
		t.Fatalf("status after confirm = %q, %v", s, err)
	}
	u, err := f.app.Authenticate(f.ctx, "tess", "copper-ladder-sunrise")
	f.must(err)
	if u.ID != id {
		t.Fatal("wrong user")
	}

	// First sync: the Pi's chain goes up, the working set comes down,
	// and the employee can log in on the Pi offline.
	r := p.sync()
	if r.Pushed != 1 || !r.SnapshotLoaded || !r.ClockVerified {
		t.Fatalf("first sync = %+v", r)
	}
	if _, err := p.app.Authenticate(f.ctx, "tess", "copper-ladder-sunrise"); err != nil {
		t.Fatalf("login on the Pi: %v", err)
	}
	other, err := domain.GetUserByUsername(f.ctx, p.app.Store.DB(), "admin")
	f.must(err)
	if other.Verifier != "" {
		t.Fatal("another user's password verifier reached the Pi")
	}
	if r := p.sync(); r.Pushed != 0 || r.SnapshotLoaded {
		t.Fatalf("idle sync did work: %+v", r)
	}
}

func TestOfflineWorkReachesCentral(t *testing.T) {
	f := newFleet(t)
	tess := f.enrol("tess", domain.RoleUser)

	a := f.asset("A1")
	sched := f.must2(f.app.CreateSchedule(f.ctx, f.mid, domain.PMScheduleCreated{AssetID: a, WOType: "pm", Title: "Semiannual PM", IntervalDays: 180, FirstDue: "2026-10-20"}))
	_, err := f.app.GenerateDueWorkOrders(f.ctx, WorkingSetHorizon)
	f.must(err)
	s, err := domain.GetSchedule(f.ctx, f.app.Store.DB(), sched)
	f.must(err)
	_, err = f.app.AssignWorkOrder(f.ctx, f.mid, s.OpenWorkOrderID, tess.user.UserID)
	f.must(err)

	if r := tess.sync(); !r.SnapshotLoaded {
		t.Fatal("assignment not pulled")
	}
	// Offline on the Pi: do the work.
	f.now = f.now.Add(2 * time.Hour)
	f.must(tess.app.ChangeWorkOrderStatus(f.ctx, tess.user, s.OpenWorkOrderID, domain.WOInProgress, ""))
	f.must(tess.app.Sign(f.ctx, tess.user, s.OpenWorkOrderID, domain.MeaningPerformed, "copper-ladder-sunrise", false))
	w, err := domain.GetWorkOrder(f.ctx, tess.app.Store.DB(), s.OpenWorkOrderID)
	f.must(err)
	if w.Status != domain.WOCompleted {
		t.Fatalf("on the Pi: %s", w.Status)
	}

	if r := tess.sync(); r.Pushed != 3 || r.Flagged != 0 {
		t.Fatalf("sync = %+v", r)
	}
	w, err = domain.GetWorkOrder(f.ctx, f.app.Store.DB(), s.OpenWorkOrderID)
	f.must(err)
	if w.Status != domain.WOCompleted {
		t.Fatalf("on central: %s", w.Status)
	}
	s, _ = domain.GetSchedule(f.ctx, f.app.Store.DB(), sched)
	if s.NextDue != "2027-04-04" {
		t.Fatalf("next due on central = %s", s.NextDue)
	}
	// The completed work order dropped out of the working set, but the
	// Pi's view matches central's after the post-push snapshot.
	if _, err := domain.GetWorkOrder(f.ctx, tess.app.Store.DB(), w.ID); err != nil {
		t.Fatalf("completed WO still open on central should remain in the working set: %v", err)
	}
	rep, err := f.app.Store.Verify(f.ctx)
	f.must(err)
	if !rep.OK() || rep.Chains != 2 {
		t.Fatalf("central verify = %+v", rep)
	}
}

func TestClaimRaceBetweenTwoPis(t *testing.T) {
	f := newFleet(t)
	alice, bob := f.enrol("alice", domain.RoleUser), f.enrol("bob", domain.RoleUser)
	wo, _, err := f.app.OpenWorkOrder(f.ctx, f.mid, app.NewWorkOrder{Type: "pm", AssetID: f.asset("A1"), Priority: "normal", Title: "Unassigned PM"})
	f.must(err)
	alice.sync()
	bob.sync()

	// Both claim while offline.
	_, err = alice.app.ClaimWorkOrder(f.ctx, alice.user, wo)
	f.must(err)
	_, err = bob.app.ClaimWorkOrder(f.ctx, bob.user, wo)
	f.must(err)

	if r := alice.sync(); r.Flagged != 0 {
		t.Fatalf("alice = %+v", r)
	}
	r := bob.sync()
	if r.Pushed != 1 || r.Flagged != 1 {
		t.Fatalf("bob = %+v", r)
	}
	// Bob's Pi now shows Alice holding it, and his claim flagged.
	w, err := domain.GetWorkOrder(f.ctx, bob.app.Store.DB(), wo)
	f.must(err)
	if w.AssignedTo != alice.user.UserID {
		t.Fatalf("bob's Pi shows holder %s", w.AssignedTo)
	}
	var flag string
	f.must(bob.app.Store.DB().QueryRowContext(f.ctx, `SELECT f.flag FROM event_flags f JOIN events e USING (event_id) WHERE e.type = ?`, domain.TypeWorkOrderClaimed).Scan(&flag))
	if flag != domain.FlagDuplicateWork {
		t.Fatalf("bob's flag = %s", flag)
	}
}

func TestRebaseKeepsUnsyncedWork(t *testing.T) {
	f := newFleet(t)
	tess := f.enrol("tess", domain.RoleUser)
	a := f.asset("A1")
	tess.sync()

	// Offline corrective work order, not yet pushed.
	wo, _, err := tess.app.OpenWorkOrder(f.ctx, tess.user, app.NewWorkOrder{Type: "corrective", AssetID: a, Priority: "high", Title: "Alarm fault"})
	f.must(err)
	// Meanwhile central changes something else.
	f.must2(f.app.CreateSite(f.ctx, f.super, "BOS", "Boston", "America/New_York"))

	// Pull a snapshot without pushing first: central lacks the new WO.
	centralPub, err := tess.client.centralPub(f.ctx)
	f.must(err)
	f.must(tess.client.pullSnapshot(f.ctx, centralPub))
	if _, err := domain.GetSiteByCode(f.ctx, tess.app.Store.DB(), "BOS"); err != nil {
		t.Fatalf("central's change missing: %v", err)
	}
	if _, err := domain.GetWorkOrder(f.ctx, tess.app.Store.DB(), wo); err != nil {
		t.Fatalf("unsynced work lost in rebase: %v", err)
	}
	// And the stored snapshot rebuilds to the same state.
	f.must(ReapplyStoredSnapshot(f.ctx, tess.app.Store, centralPub, tess.client.NodeID))
	if _, err := domain.GetWorkOrder(f.ctx, tess.app.Store.DB(), wo); err != nil {
		t.Fatalf("unsynced work lost in reapply: %v", err)
	}
}

func TestSnapshotTamperingRejected(t *testing.T) {
	f := newFleet(t)
	tess := f.enrol("tess", domain.RoleUser)
	n, err := domain.GetNode(f.ctx, f.app.Store.DB(), tess.client.NodeID)
	f.must(err)
	snap, err := BuildSnapshot(f.ctx, f.app.Store.DB(), f.app.Author.NodeID, n, tess.client.ChainID, f.now)
	f.must(err)
	_, otherKey, _ := ed25519.GenerateKey(nil)
	body, sig, err := SignSnapshot(snap, otherKey)
	f.must(err)
	centralPub, _ := tess.client.centralPub(f.ctx)
	if _, err := ApplySnapshot(f.ctx, tess.app.Store, body, sig, centralPub, tess.client.NodeID); !errors.Is(err, ErrBadSnapshot) {
		t.Fatalf("snapshot signed by another key: %v", err)
	}
	body, sig, err = SignSnapshot(snap, ed25519.NewKeyFromSeed(make([]byte, 32)))
	f.must(err)
	body = bytes.Replace(body, []byte(`"NYC"`), []byte(`"EVL"`), 1)
	if _, err := ApplySnapshot(f.ctx, tess.app.Store, body, sig, centralPub, tess.client.NodeID); !errors.Is(err, ErrBadSnapshot) {
		t.Fatalf("edited snapshot: %v", err)
	}
}

func TestRequestAuthentication(t *testing.T) {
	f := newFleet(t)
	tess := f.enrol("tess", domain.RoleUser)
	send := func(req *http.Request) int {
		resp, err := http.DefaultClient.Do(req)
		f.must(err)
		resp.Body.Close()
		return resp.StatusCode
	}
	signedHello := func(key ed25519.PrivateKey, keyID string) (*http.Request, []byte) {
		body := []byte(`{"chain_id":"` + tess.client.ChainID + `","head_seq":0}`)
		req, _ := http.NewRequest("POST", f.srv.URL+PathHello, bytes.NewReader(body))
		n := make([]byte, 8)
		rand.Read(n)
		f.must(httpsig.Sign(req, body, keyID, key, f.now, hex.EncodeToString(n)))
		return req, body
	}

	req, body := signedHello(tess.client.Keys.Transport, tess.client.transportKeyID())
	replay := req.Clone(f.ctx)
	replay.Body = http.NoBody
	if code := send(req); code != http.StatusOK {
		t.Fatalf("valid request: %d", code)
	}
	replay, _ = http.NewRequest("POST", f.srv.URL+PathHello, bytes.NewReader(body))
	replay.Header = req.Header.Clone()
	if code := send(replay); code != http.StatusUnauthorized {
		t.Fatalf("replayed request: %d", code)
	}

	// The event key can't stand in for the transport key.
	req, _ = signedHello(tess.client.Keys.Event, tess.client.transportKeyID())
	if code := send(req); code != http.StatusUnauthorized {
		t.Fatalf("signed with the wrong key: %d", code)
	}
	_, stranger, _ := ed25519.GenerateKey(nil)
	req, _ = signedHello(stranger, event.KeyID(stranger.Public().(ed25519.PublicKey)))
	if code := send(req); code != http.StatusUnauthorized {
		t.Fatalf("unknown key: %d", code)
	}
	// A captured request with an edited body fails the digest.
	req, _ = signedHello(tess.client.Keys.Transport, tess.client.transportKeyID())
	req.Body = http.NoBody
	req2, _ := http.NewRequest("POST", f.srv.URL+PathHello, strings.NewReader(`{"chain_id":"x"}`))
	req2.Header = req.Header.Clone()
	if code := send(req2); code != http.StatusUnauthorized {
		t.Fatalf("edited body: %d", code)
	}
}

func TestPiCannotActForAnotherUser(t *testing.T) {
	f := newFleet(t)
	tess := f.enrol("tess", domain.RoleUser)
	a := f.asset("A1")
	// A modified Pi signs an event claiming to be the super user.
	seq, prev, err := tess.app.Store.Head(f.ctx, tess.client.ChainID)
	f.must(err)
	e, err := event.Seal(event.Draft{ActorUserID: f.super.UserID, ActorSessionID: "x", Type: domain.TypeAssetStatusChanged,
		EntityType: domain.EntityAsset, EntityID: a, BaseVersion: 1, SchemaVersion: 1, Payload: []byte(`{"status":"retired","reason":"x"}`)},
		event.Position{NodeID: tess.client.NodeID, ChainID: tess.client.ChainID, Seq: seq + 1, PrevHash: prev,
			HLC: tess.app.Author.Clock.Now(), WallTime: f.now, ClockState: event.ClockVerified}, tess.client.Keys.EventSigner())
	f.must(err)
	var resp EventsResponse
	_, _, err = tess.client.do(f.ctx, http.MethodPost, PathEvents, EventsRequest{Events: []WireEvent{ToWire(e)}}, &resp, true)
	f.must(err)
	if len(resp.Results) != 1 || !slices.Contains(resp.Results[0].Flags, domain.FlagNotAuthorized) {
		t.Fatalf("impersonation result = %+v", resp)
	}
	asset, _ := domain.GetAsset(f.ctx, f.app.Store.DB(), a)
	if asset.Status != domain.AssetInService {
		t.Fatal("impersonated event took effect")
	}
}

func TestForkQuarantinesPi(t *testing.T) {
	f := newFleet(t)
	tess := f.enrol("tess", domain.RoleUser)
	// A different event at seq 1 of the same chain.
	e, err := event.Seal(event.Draft{ActorUserID: "system:init", ActorSessionID: "system", Type: event.TypeChainStarted,
		EntityType: "chain", EntityID: tess.client.ChainID, SchemaVersion: 1, Payload: []byte(`{"forged":true}`)},
		event.Position{NodeID: tess.client.NodeID, ChainID: tess.client.ChainID, Seq: 1, HLC: 1, WallTime: f.now, ClockState: event.ClockVerified},
		tess.client.Keys.EventSigner())
	f.must(err)
	var resp EventsResponse
	_, _, err = tess.client.do(f.ctx, http.MethodPost, PathEvents, EventsRequest{Events: []WireEvent{ToWire(e)}}, &resp, true)
	f.must(err)
	if !strings.HasPrefix(resp.Error, "fork") {
		t.Fatalf("fork response = %+v", resp)
	}
	if _, err := tess.client.Sync(f.ctx); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("sync after fork: %v", err)
	}
	// After investigating, a super user lifts the quarantine.
	var rej *store.Rejection
	if err := f.app.ClearQuarantine(f.ctx, f.super, tess.client.NodeID, ""); !errors.As(err, &rej) {
		t.Fatalf("clearing without a reason: %v", err)
	}
	f.must(f.app.ClearQuarantine(f.ctx, f.super, tess.client.NodeID, "forged event came from a test harness; chain intact"))
	tess.sync()
}

func TestRevocationWipesAfterDeliveringWork(t *testing.T) {
	f := newFleet(t)
	tess, otto := f.enrol("tess", domain.RoleUser), f.enrol("otto", domain.RoleUser)
	a := f.asset("A1")
	tess.sync()
	otto.sync()

	// Both do work offline, then their Pis are revoked: Tess's with
	// unsynced work kept (a Pi that turned up again), Otto's without.
	_, _, err := tess.app.OpenWorkOrder(f.ctx, tess.user, app.NewWorkOrder{Type: "corrective", AssetID: a, Priority: "high", Title: "Before revocation"})
	f.must(err)
	_, _, err = otto.app.OpenWorkOrder(f.ctx, otto.user, app.NewWorkOrder{Type: "corrective", AssetID: a, Priority: "high", Title: "Otto's"})
	f.must(err)
	f.now = f.now.Add(time.Minute)
	f.must(f.app.RevokeNode(f.ctx, f.super, tess.client.NodeID, "replaced", true))
	f.must(f.app.RevokeNode(f.ctx, f.super, otto.client.NodeID, "left the company", false))
	f.now = f.now.Add(time.Minute)
	_, _, err = tess.app.OpenWorkOrder(f.ctx, tess.user, app.NewWorkOrder{Type: "corrective", AssetID: a, Priority: "high", Title: "After revocation"})
	f.must(err)

	if _, err := tess.client.Sync(f.ctx); !errors.Is(err, ErrRevoked) || !tess.wiped {
		t.Fatalf("tess: %v wiped=%v", err, tess.wiped)
	}
	var before, after int
	f.must(f.app.Store.DB().QueryRowContext(f.ctx, `SELECT count(*) FROM work_orders WHERE title = 'Before revocation'`).Scan(&before))
	f.must(f.app.Store.DB().QueryRowContext(f.ctx, `SELECT count(*) FROM work_orders WHERE title = 'After revocation'`).Scan(&after))
	if before != 1 || after != 0 {
		t.Fatalf("work delivered: before=%d after=%d", before, after)
	}

	if _, err := otto.client.Sync(f.ctx); !errors.Is(err, ErrRevoked) || !otto.wiped {
		t.Fatalf("otto: %v wiped=%v", err, otto.wiped)
	}
	var ottos int
	f.must(f.app.Store.DB().QueryRowContext(f.ctx, `SELECT count(*) FROM work_orders WHERE title = 'Otto''s'`).Scan(&ottos))
	if ottos != 0 {
		t.Fatal("a revoked Pi without keep_unsynced delivered work")
	}
}

func TestPurgeOnlyBelowDurableWatermark(t *testing.T) {
	f := newFleet(t)
	tess := f.enrol("tess", domain.RoleUser)
	a := f.asset("A1")
	tess.sync()
	for i := 0; i < 3; i++ {
		_, _, err := tess.app.OpenWorkOrder(f.ctx, tess.user, app.NewWorkOrder{Type: "corrective", AssetID: a, Priority: "low", Title: "x"})
		f.must(err)
	}
	tess.sync() // seq 1..4 on central

	f.now = f.now.Add(40 * 24 * time.Hour)
	if r := tess.sync(); r.Purged != 0 {
		t.Fatalf("purged %d with no durable backup", r.Purged)
	}
	// Stand-in for a confirmed off-site backup holding seq 1-2.
	f.must(f.app.Store.Update(f.ctx, func(tx *store.Tx) error {
		_, err := tx.ExecContext(f.ctx, `INSERT INTO durable_heads (chain_id, seq) VALUES (?, 2)`, tess.client.ChainID)
		return err
	}))
	if r := tess.sync(); r.Purged != 2 {
		t.Fatalf("purged %d, want 2 (seq 1-2)", r.Purged)
	}
	rep, err := tess.app.Store.Verify(f.ctx)
	f.must(err)
	if !rep.OK() || rep.Events != 2 {
		t.Fatalf("Pi verify after purge = %+v", rep)
	}
	// The Pi keeps working and syncing.
	_, _, err = tess.app.OpenWorkOrder(f.ctx, tess.user, app.NewWorkOrder{Type: "corrective", AssetID: a, Priority: "low", Title: "y"})
	f.must(err)
	if r := tess.sync(); r.Pushed != 1 {
		t.Fatalf("sync after purge = %+v", r)
	}
}

func TestClockVerification(t *testing.T) {
	f := newFleet(t)
	tess := f.enrol("tess", domain.RoleUser)
	if ClockState(f.ctx, tess.app.Store, f.now) != event.ClockVerified {
		t.Fatal("clock not verified after sync")
	}
	// The Pi boots without an RTC and its clock is two days behind. It
	// still syncs (signing with central's time) but its clock stays
	// unverified, so its events are marked.
	skewed := func() time.Time { return f.now.Add(-48 * time.Hour) }
	tess.client.Now = skewed
	tess.app.Author.Now = skewed
	f.must(tess.app.Store.SetConfig(f.ctx, ConfigClockVerifiedAt, "2000-01-01T00:00:00Z"))
	_, _, err := tess.app.OpenWorkOrder(f.ctx, tess.user, app.NewWorkOrder{Type: "corrective", AssetID: f.asset("A1"), Priority: "low", Title: "x"})
	if err == nil {
		t.Fatal("asset created on central after the last sync should not be on the Pi yet")
	}
	r, err := tess.client.Sync(f.ctx)
	if err != nil || r.ClockVerified {
		t.Fatalf("sync with skewed clock: %+v %v", r, err)
	}
	var a string
	f.must(f.app.Store.DB().QueryRowContext(f.ctx, `SELECT id FROM assets WHERE tag = 'A1'`).Scan(&a))
	_, _, err = tess.app.OpenWorkOrder(f.ctx, tess.user, app.NewWorkOrder{Type: "corrective", AssetID: a, Priority: "low", Title: "x"})
	f.must(err)
	var state string
	f.must(tess.app.Store.DB().QueryRowContext(f.ctx, `SELECT clock_state FROM events ORDER BY local_order DESC LIMIT 1`).Scan(&state))
	if state != string(event.ClockUnverified) {
		t.Fatalf("event from a Pi with a wrong clock has clock_state %s", state)
	}
	if ClockState(f.ctx, tess.app.Store, f.now.Add(25*time.Hour)) != event.ClockUnverified {
		t.Fatal("verification should lapse after 24 hours")
	}
}

func TestAttachmentFilesTravel(t *testing.T) {
	f := newFleet(t)
	tess, bob := f.enrol("tess", domain.RoleUser), f.enrol("bob", domain.RoleUser)
	a := f.asset("A1")
	tess.sync()
	bob.sync()

	// Tess attaches a certificate offline; it uploads on her next sync.
	cert := []byte("%PDF-1.7 certificate for A1")
	id, err := tess.app.AddAttachment(f.ctx, tess.user, domain.EntityAsset, a, "cert.pdf", "certificate", cert)
	f.must(err)
	if r := tess.sync(); r.Uploaded != 1 {
		t.Fatalf("uploaded %d", r.Uploaded)
	}
	att, err := domain.GetAttachment(f.ctx, f.app.Store.DB(), id)
	f.must(err)
	if !f.app.Blobs.Has(att.SHA256) {
		t.Fatal("central doesn't have the file")
	}
	if r := tess.sync(); r.Uploaded != 0 {
		t.Fatal("uploaded twice")
	}

	// Bob's Pi lists it, fetches the file on demand, and caches it.
	bob.sync()
	if bob.app.Blobs.Has(att.SHA256) {
		t.Fatal("files should be fetched on demand, not pushed to every Pi")
	}
	got, err := bob.client.FetchBlob(f.ctx, att.SHA256)
	if err != nil || string(got) != string(cert) || !bob.app.Blobs.Has(att.SHA256) {
		t.Fatalf("fetch: %q %v", got, err)
	}

	// Central refuses files no attachment refers to.
	stray := []byte("%PDF-1.4 not attached anywhere")
	if _, _, err := tess.client.doRaw(f.ctx, http.MethodPut, PathBlobs+sha256hex(stray), stray, nil, true); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("stray upload: %v", err)
	}

	// A mid-tier user purges it on central: gone there, and from Bob's
	// cache at his next sync.
	f.must(f.app.DetachAttachment(f.ctx, f.mid, id, "shows a patient wristband", true))
	if f.app.Blobs.Has(att.SHA256) {
		t.Fatal("purged file still on central")
	}
	bob.sync()
	if bob.app.Blobs.Has(att.SHA256) {
		t.Fatal("purged file still cached on a Pi")
	}
	if _, err := bob.client.FetchBlob(f.ctx, att.SHA256); err == nil {
		t.Fatal("central served a purged file")
	}
}

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestKioskMode(t *testing.T) {
	f := newFleet(t)
	alice := f.activeCentralUser("alice", domain.RoleUser)
	bob := f.activeCentralUser("bob", domain.RoleUser)
	carol := f.activeCentralUser("carol", domain.RoleUser)
	a := f.asset("A1")
	kioskID, otp, err := f.app.CreateKiosk(f.ctx, f.super, f.site, "nyc-shop")
	f.must(err)
	f.must(f.app.AddKioskMember(f.ctx, f.super, kioskID, alice.UserID))
	f.must(f.app.AddKioskMember(f.ctx, f.super, kioskID, bob.UserID))

	k := f.newPi()
	if _, err := k.client.ActivateKiosk(f.ctx, "nyc-shop", "WRONG-WRONG-WRONG-0000"); err == nil {
		t.Fatal("kiosk activated with the wrong password")
	}
	act, err := k.client.ActivateKiosk(f.ctx, "nyc-shop", otp)
	f.must(err)
	k.proj.CentralNodeID = act.CentralNodeID
	if _, err := f.newPi().client.ActivateKiosk(f.ctx, "nyc-shop", otp); err == nil || !strings.Contains(err.Error(), "409") {
		t.Fatalf("second Pi for the same kiosk: %v", err)
	}
	f.must(f.app.ConfirmNode(f.ctx, f.super, k.client.NodeID, act.PairingWords))
	// The activation password was single use.
	if _, err := f.newPi().client.ActivateKiosk(f.ctx, "nyc-shop", otp); err == nil {
		t.Fatal("kiosk activation password reused")
	}
	k.sync()

	// Members sign in with their own passwords; others can't.
	for _, u := range []string{"alice", "bob"} {
		if _, err := k.app.Authenticate(f.ctx, u, "brass-kettle-orchard-7"); err != nil {
			t.Fatalf("%s on the kiosk: %v", u, err)
		}
	}
	if _, err := k.app.Authenticate(f.ctx, "carol", "brass-kettle-orchard-7"); !errors.Is(err, app.ErrBadCredentials) {
		t.Fatalf("non-member on the kiosk: %v", err)
	}

	// Each member's work is attributed to them.
	_, _, err = k.app.OpenWorkOrder(f.ctx, alice, app.NewWorkOrder{Type: "corrective", AssetID: a, Priority: "high", Title: "Alice's"})
	f.must(err)
	_, _, err = k.app.OpenWorkOrder(f.ctx, bob, app.NewWorkOrder{Type: "corrective", AssetID: a, Priority: "high", Title: "Bob's (before removal)"})
	f.must(err)
	if _, _, err := k.app.OpenWorkOrder(f.ctx, carol, app.NewWorkOrder{Type: "corrective", AssetID: a, Priority: "low", Title: "x"}); err == nil {
		t.Fatal("the kiosk accepted work from a non-member")
	}

	// Bob is removed on central while the kiosk is offline; the work he
	// did before still counts (offline grace).
	f.now = f.now.Add(time.Minute)
	f.must(f.app.RemoveKioskMember(f.ctx, f.super, kioskID, bob.UserID))
	if r := k.sync(); r.Pushed != 2 || r.Flagged != 0 {
		t.Fatalf("kiosk sync = %+v", r)
	}
	for title, who := range map[string]string{"Alice's": alice.UserID, "Bob's (before removal)": bob.UserID} {
		var opener string
		f.must(f.app.Store.DB().QueryRowContext(f.ctx, `SELECT opened_by FROM work_orders WHERE title = ?`, title).Scan(&opener))
		if opener != who {
			t.Fatalf("%q opened by %s", title, opener)
		}
	}
	if _, err := k.app.Authenticate(f.ctx, "bob", "brass-kettle-orchard-7"); !errors.Is(err, app.ErrBadCredentials) {
		t.Fatalf("removed member still signs in on the kiosk: %v", err)
	}

	// A brand-new user can start on the kiosk with their one-time
	// password, choosing their own there.
	daveID, temp, err := f.app.CreateUser(f.ctx, f.super, app.NewUser{Username: "dave", LegalName: "Dave", Email: "dave@example.org", Role: domain.RoleUser, IdentityVerification: "badge"})
	f.must(err)
	f.must(f.app.AddKioskMember(f.ctx, f.super, kioskID, daveID))
	k.sync()
	if _, err := k.app.Authenticate(f.ctx, "dave", temp); !errors.Is(err, app.ErrMustChangePassword) {
		t.Fatalf("new member with one-time password: %v", err)
	}
	dave := app.Actor{UserID: daveID, SessionID: "kiosk"}
	f.must(k.app.ChooseFirstPassword(f.ctx, dave, "granite-otter-meadow")) // as the web page does
	if r := k.sync(); r.Flagged != 0 {
		t.Fatalf("password change from the kiosk flagged: %+v", r)
	}
	if _, err := f.app.Authenticate(f.ctx, "dave", "granite-otter-meadow"); err != nil {
		t.Fatalf("password chosen on the kiosk not accepted centrally: %v", err)
	}

	// Kiosk membership doesn't use up a member's own Pi.
	f.must(f.app.AddKioskMember(f.ctx, f.super, kioskID, carol.UserID))
	resetCarol, err := f.app.ResetPassword(f.ctx, f.super, carol.UserID, "phone")
	f.must(err)
	_, err = f.newPi().client.Activate(f.ctx, "carol", resetCarol, "copper-ladder-sunrise", cheap)
	f.must(err)

	// Last, because it leaves the kiosk's chain behind central's: a
	// modified kiosk can't record work as a non-member.
	outsider := f.activeCentralUser("erin", domain.RoleUser)
	seq, prev, err := k.app.Store.Head(f.ctx, k.client.ChainID)
	f.must(err)
	e, err := event.Seal(event.Draft{ActorUserID: outsider.UserID, ActorSessionID: "x", Type: domain.TypeAssetStatusChanged,
		EntityType: domain.EntityAsset, EntityID: a, BaseVersion: 1, SchemaVersion: 1, Payload: []byte(`{"status":"missing","reason":"x"}`)},
		event.Position{NodeID: k.client.NodeID, ChainID: k.client.ChainID, Seq: seq + 1, PrevHash: prev,
			HLC: k.app.Author.Clock.Now(), WallTime: f.now, ClockState: event.ClockVerified}, k.client.Keys.EventSigner())
	f.must(err)
	var resp EventsResponse
	_, _, err = k.client.do(f.ctx, http.MethodPost, PathEvents, EventsRequest{Events: []WireEvent{ToWire(e)}}, &resp, true)
	f.must(err)
	if len(resp.Results) != 1 || !slices.Contains(resp.Results[0].Flags, domain.FlagNotAuthorized) {
		t.Fatalf("non-member event = %+v", resp)
	}

}

func TestPiWebCertificate(t *testing.T) {
	f := newFleet(t)
	kiosk := f.enrol("tess", domain.RoleUser)
	keys := filepath.Join(kiosk.dir, "keys")
	names := []string{"kiosk-nyc.local", "192.168.1.20"}

	changed, err := kiosk.client.EnsureTLSCert(f.ctx, keys, names)
	if err != nil || !changed {
		t.Fatalf("first certificate: %v %v", changed, err)
	}
	if changed, err = kiosk.client.EnsureTLSCert(f.ctx, keys, names); err != nil || changed {
		t.Fatalf("certificate re-issued needlessly: %v %v", changed, err)
	}

	// Serve HTTPS with it; a tablet trusting the fleet CA connects.
	pair, err := tls.LoadX509KeyPair(filepath.Join(keys, TLSCertFile), filepath.Join(keys, TLSKeyFile))
	f.must(err)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "kiosk") }))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	srv.StartTLS()
	defer srv.Close()
	caPEM, err := os.ReadFile(filepath.Join(keys, TLSCAFile))
	f.must(err)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	tablet := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "kiosk-nyc.local"}}}
	resp, err := tablet.Get(srv.URL)
	f.must(err)
	resp.Body.Close()
	stranger := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "bank.example.com"}}}
	if _, err := stranger.Get(srv.URL); err == nil {
		t.Fatal("certificate accepted for a name it wasn't issued for")
	}

	// The fleet CA is downloadable for installing on tablets.
	resp, err = http.Get(f.srv.URL + PathFleetCA)
	f.must(err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != string(caPEM) {
		t.Fatal("fleet CA download differs")
	}

	// Pis that aren't confirmed get no certificate.
	pending := f.newPi()
	_, temp, _ := f.app.CreateUser(f.ctx, f.super, app.NewUser{Username: "pat", LegalName: "Pat", Email: "pat@example.org", Role: domain.RoleUser, IdentityVerification: "badge"})
	_, err = pending.client.Activate(f.ctx, "pat", temp, "copper-ladder-sunrise", cheap)
	f.must(err)
	if _, err := pending.client.EnsureTLSCert(f.ctx, filepath.Join(pending.dir, "keys"), names); err == nil {
		t.Fatal("pending Pi got a certificate")
	}
}

func TestChecklistCompletedOffline(t *testing.T) {
	f := newFleet(t)
	tess := f.enrol("tess", domain.RoleUser)
	a := f.asset("A1")
	proc, _, err := f.app.PublishProcedure(f.ctx, f.mid, "PM", []domain.Step{
		{ID: "visual", Text: "Visual", Kind: domain.StepCheck, Required: true},
		{ID: "leak", Text: "Leakage", Kind: domain.StepNumber, Unit: "µA", Lower: "0", Upper: "300", Required: true},
	})
	f.must(err)
	wo, _, err := f.app.OpenWorkOrder(f.ctx, f.mid, app.NewWorkOrder{Type: "pm", AssetID: a, Priority: "normal", Title: "PM", ProcedureID: proc})
	f.must(err)
	_, err = f.app.AssignWorkOrder(f.ctx, f.mid, wo, tess.user.UserID)
	f.must(err)
	tess.sync()

	// All offline on the Pi: start, record, log time, sign.
	f.must(tess.app.ChangeWorkOrderStatus(f.ctx, tess.user, wo, domain.WOInProgress, ""))
	f.must(tess.app.RecordStep(f.ctx, tess.user, wo, "visual", "pass", ""))
	f.must(tess.app.RecordStep(f.ctx, tess.user, wo, "leak", "95.5", ""))
	_, err = tess.app.LogLabor(f.ctx, tess.user, wo, 50, "2026-10-06", "")
	f.must(err)
	f.must(tess.app.Sign(f.ctx, tess.user, wo, domain.MeaningPerformed, "copper-ladder-sunrise", false))

	// Central recomputes the same signed content and accepts everything.
	if r := tess.sync(); r.Pushed != 6 || r.Flagged != 0 {
		t.Fatalf("sync = %+v", r)
	}
	w, _ := domain.GetWorkOrder(f.ctx, f.app.Store.DB(), wo)
	sigs, _ := domain.WorkOrderSignatures(f.ctx, f.app.Store.DB(), wo)
	_, total, _ := domain.Labor(f.ctx, f.app.Store.DB(), wo)
	if w.Status != domain.WOCompleted || len(sigs) != 1 || sigs[0].Stale || total != 50 {
		t.Fatalf("central: status %s, signatures %+v, labour %d", w.Status, sigs, total)
	}
}

func TestMeterReadingsFromPis(t *testing.T) {
	f := newFleet(t)
	tess, bob := f.enrol("tess", domain.RoleUser), f.enrol("bob", domain.RoleUser)
	a := f.asset("VENT-1")
	at := func(h int) time.Time { return time.Date(2026, 10, 5, h, 0, 0, 0, time.UTC) }
	_, err := f.app.RecordMeter(f.ctx, f.mid, a, "hours", "1000", at(1), false)
	f.must(err)
	sched, err := f.app.CreateSchedule(f.ctx, f.mid, domain.PMScheduleCreated{AssetID: a, WOType: "pm", Title: "500 h PM",
		IntervalDays: 365, FirstDue: "2027-09-01", Meter: "hours", MeterInterval: "500"})
	f.must(err)
	tess.sync()
	bob.sync()

	// Both read the meter offline; Bob's earlier reading syncs last.
	_, err = tess.app.RecordMeter(f.ctx, tess.user, a, "hours", "1460", at(8), false)
	f.must(err)
	_, err = bob.app.RecordMeter(f.ctx, bob.user, a, "hours", "1300", at(5), false)
	f.must(err)
	tot, _, _ := domain.MeterTotal(f.ctx, tess.app.Store.DB(), a, "hours")
	if tot != "460" {
		t.Fatalf("total on Tess's Pi = %s", tot)
	}
	tess.sync()
	if r := bob.sync(); r.Flagged != 0 {
		t.Fatalf("bob = %+v", r)
	}
	tot, _, _ = domain.MeterTotal(f.ctx, f.app.Store.DB(), a, "hours")
	if tot != "460" {
		t.Fatalf("central total = %s", tot)
	}
	// Central generates the usage-triggered work, and Bob's Pi agrees on
	// the total after its next snapshot.
	got, err := f.app.GenerateDueWorkOrders(f.ctx, WorkingSetHorizon)
	f.must(err)
	if len(got) != 1 {
		t.Fatalf("generated %v", got)
	}
	bob.sync()
	tot, _, _ = domain.MeterTotal(f.ctx, bob.app.Store.DB(), a, "hours")
	s, _ := domain.GetSchedule(f.ctx, bob.app.Store.DB(), sched)
	if tot != "460" || s.OpenWorkOrderID == "" {
		t.Fatalf("Bob's Pi: total %s, open work %q", tot, s.OpenWorkOrderID)
	}
}

// A Pi works offline on a record that central merges meanwhile, and
// registers the same equipment again.
func TestMergeWhileAPiIsOffline(t *testing.T) {
	f := newFleet(t)
	tess := f.enrol("tess", domain.RoleUser)
	reg := func(a *app.App, actor app.Actor, tag string) string {
		return f.must2(a.RegisterAsset(f.ctx, actor, domain.AssetRegistered{Tag: tag, LocationID: f.loc, Manufacturer: "Baxter", Model: "Sigma", MasterID: "M-1"}))
	}
	keep, dup := reg(f.app, f.super, "NYC-1"), reg(f.app, f.super, "PROV-1")
	tess.sync()

	// Offline on the Pi: the duplicate fails a test, and the pump gets
	// registered a third time.
	f.must(tess.app.SetAssetStatus(f.ctx, tess.user, dup, 1, domain.AssetOutOfService, "fails leakage"))
	third := reg(tess.app, tess.user, "NYC-1B")
	// Meanwhile on the master.
	f.must(f.app.MergeAssets(f.ctx, f.mid, keep, 1, dup, "same pump", true))

	if r := tess.sync(); r.Pushed != 2 || r.Flagged != 1 {
		t.Fatalf("sync = %+v", r)
	}
	for name, st := range map[string]*store.Store{"central": f.app.Store, "pi": tess.app.Store} {
		k, _ := domain.GetAsset(f.ctx, st.DB(), keep)
		d, _ := domain.GetAsset(f.ctx, st.DB(), dup)
		if k.Status != domain.AssetOutOfService || d.MergedInto != keep {
			t.Errorf("%s: kept record %s, merged record points at %q", name, k.Status, d.MergedInto)
		}
	}
	groups, err := domain.Duplicates(f.ctx, f.app.Store.DB(), "M-1")
	f.must(err)
	if len(groups) != 1 || len(groups[0].Assets) != 2 || groups[0].Assets[1].ID != third {
		t.Fatalf("duplicates = %+v", groups)
	}
}

func TestTooOldPiCannotSync(t *testing.T) {
	f := newFleet(t)
	tess := f.enrol("tess", domain.RoleUser)
	tess.client.Version = "v0.3.0" // before the merge columns
	f.must2(tess.app.RegisterAsset(f.ctx, tess.user, domain.AssetRegistered{Tag: "OLD-1", LocationID: f.loc, Manufacturer: "F", Model: "M"}))
	_, err := tess.client.Sync(f.ctx)
	if !errors.Is(err, ErrTooOld) || !strings.Contains(err.Error(), "v0.3.0") || !strings.Contains(err.Error(), MinNodeVersion) {
		t.Fatalf("err = %v", err)
	}
	if v, _ := tess.app.Store.Config(f.ctx, ConfigUpdateNeeded); v != MinNodeVersion {
		t.Fatalf("update_needed = %q", v)
	}
	if got := NodeVersion(f.ctx, f.app.Store, tess.client.NodeID); got != "v0.3.0" {
		t.Fatalf("central recorded version %q", got)
	}
	// An old Pi that doesn't stop by itself is refused anyway.
	pub, _ := tess.client.centralPub(f.ctx)
	if err := tess.client.pullSnapshot(f.ctx, pub); err == nil || !strings.Contains(err.Error(), "needs "+MinNodeVersion) {
		t.Fatalf("snapshot for an old Pi: %v", err)
	}
	if _, _, err := tess.client.push(f.ctx, 0, 100); err == nil || !strings.Contains(err.Error(), "needs "+MinNodeVersion) {
		t.Fatalf("push from an old Pi: %v", err)
	}
	var n int
	f.app.Store.DB().QueryRow(`SELECT count(*) FROM assets WHERE tag = 'OLD-1'`).Scan(&n)
	if n != 0 {
		t.Fatal("central took records from a too-old Pi")
	}
	// Updated, it syncs and the warning goes.
	tess.client.Version = MinNodeVersion
	if r := tess.sync(); r.Pushed != 1 {
		t.Fatalf("sync after update = %+v", r)
	}
	if v, _ := tess.app.Store.Config(f.ctx, ConfigUpdateNeeded); v != "" {
		t.Fatalf("update_needed still %q", v)
	}
}

func TestRequiredVersionWithGracePeriod(t *testing.T) {
	f := newFleet(t)
	tess := f.enrol("tess", domain.RoleUser)
	tess.client.Version = "v0.7.0"
	if err := RequireVersion(f.ctx, f.app.Store, "v0.8.0", f.now, "v0.7.2"); err == nil {
		t.Fatal("required a version newer than the master")
	}
	if err := RequireVersion(f.ctx, f.app.Store, "latest", f.now, "v0.7.2"); err == nil {
		t.Fatal("accepted a bad version")
	}
	from := f.now.Add(7 * 24 * time.Hour)
	f.must(RequireVersion(f.ctx, f.app.Store, "v0.7.2", from, "v0.7.2"))

	// During the grace period: syncs, and is told the date.
	tess.sync()
	if v, _ := tess.app.Store.Config(f.ctx, ConfigUpdateDue); v != "v0.7.2 "+from.UTC().Format(time.RFC3339) {
		t.Fatalf("update_due = %q", v)
	}
	// After it: blocked until updated.
	f.now = from.Add(time.Hour)
	if _, err := tess.client.Sync(f.ctx); !errors.Is(err, ErrTooOld) {
		t.Fatalf("err = %v", err)
	}
	if !NodeRequirement(f.ctx, f.app.Store, f.now).Outdated("v0.7.0") {
		t.Fatal("not shown as outdated")
	}
	tess.client.Version = "v0.7.2"
	tess.sync()
	// Cleared: back to the built-in minimum.
	f.must(RequireVersion(f.ctx, f.app.Store, "", time.Time{}, "v0.7.2"))
	if r := NodeRequirement(f.ctx, f.app.Store, f.now); r.Min != MinNodeVersion || r.Next != "" {
		t.Fatalf("requirement = %+v", r)
	}
}

func TestDevelopmentBuildsAreNeverBlocked(t *testing.T) {
	if tooOld("dev", "v9.0.0") || tooOld("test", "v9.0.0") || tooOld("v1.0.0", "") || !tooOld("v0.3.9", "v0.4.0") || tooOld("v0.10.0", "v0.4.0") {
		t.Fatal("tooOld")
	}
}

// An employee proposes a location on their Pi and registers equipment
// there, offline; central gets both, and a rejection there moves the
// equipment to Unallocated.
func TestLocationProposedOnAPi(t *testing.T) {
	f := newFleet(t)
	tess := f.enrol("tess", domain.RoleUser)
	loc, err := tess.app.ProposeLocation(f.ctx, tess.user, domain.LocationProposed{SiteID: f.site, Name: "Cath lab 2", Kind: "room",
		Details: domain.LocationDetails{Building: "North", Floor: "2"}})
	f.must(err)
	asset, err := tess.app.RegisterAsset(f.ctx, tess.user, domain.AssetRegistered{Tag: "CATH-1", LocationID: loc, Manufacturer: "Philips", Model: "Azurion"})
	f.must(err)
	if r := tess.sync(); r.Pushed != 2 || r.Flagged != 0 {
		t.Fatalf("sync = %+v", r)
	}
	var status string
	f.must(tess.app.Store.DB().QueryRow(`SELECT status FROM locations WHERE id = ?`, loc).Scan(&status))
	if status != domain.LocationPending {
		t.Fatalf("on the Pi after sync: %s", status)
	}
	pending, err := domain.PendingLocations(f.ctx, f.app.Store.DB())
	f.must(err)
	if len(pending) != 1 || pending[0].Details.Building != "North" || len(pending[0].Equipment) != 1 {
		t.Fatalf("central's pending = %+v", pending)
	}
	f.must(f.app.ReviewLocation(f.ctx, f.super, loc, false, "duplicate of Cath lab B"))
	var site string
	f.must(f.app.Store.DB().QueryRow(`SELECT site_id FROM assets WHERE id = ?`, asset).Scan(&site))
	if site != domain.UnallocatedSiteID {
		t.Fatalf("central: %s", site)
	}
	tess.sync()
	f.must(tess.app.Store.DB().QueryRow(`SELECT status FROM locations WHERE id = ?`, loc).Scan(&status))
	if status != domain.LocationRejected {
		t.Fatalf("on the Pi after the rejection: %s", status)
	}
}
