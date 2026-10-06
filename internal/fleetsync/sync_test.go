package fleetsync

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"pi-fleet/internal/app"
	"pi-fleet/internal/domain"
	"pi-fleet/internal/event"
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

	srv := &Server{App: f.app, CentralKey: k.Event, Now: f.clock, Logf: t.Logf}
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
	p.app = &app.App{Store: st, Author: author, Params: cheap, Now: f.clock}
	_, err = st.Append(f.ctx, author, event.Draft{ActorUserID: "system:init", ActorSessionID: "system", Type: event.TypeChainStarted,
		EntityType: "chain", EntityID: chainID, SchemaVersion: 1, Payload: []byte(`{}`)})
	f.must(err)
	p.client = &Client{BaseURL: f.srv.URL, Store: st, Keys: k, NodeID: nodeID, ChainID: chainID, Version: "test",
		Now: f.clock, Wipe: func() error { p.wiped = true; return nil }}
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
	f.must(tess.app.ChangeWorkOrderStatus(f.ctx, tess.user, s.OpenWorkOrderID, domain.WOCompleted, ""))
	w, err := domain.GetWorkOrder(f.ctx, tess.app.Store.DB(), s.OpenWorkOrderID)
	f.must(err)
	if w.Status != domain.WOCompleted {
		t.Fatalf("on the Pi: %s", w.Status)
	}

	if r := tess.sync(); r.Pushed != 2 || r.Flagged != 0 {
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
	f.must(f.app.Store.SetConfig(f.ctx, "durable:"+tess.client.ChainID, "2"))
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
