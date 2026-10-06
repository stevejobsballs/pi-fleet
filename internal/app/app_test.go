package app

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"pi-fleet/internal/blobs"
	"pi-fleet/internal/domain"
	"pi-fleet/internal/event"
	"pi-fleet/internal/hlc"
	"pi-fleet/internal/password"
	"pi-fleet/internal/store"
)

var cheap = password.Params{Time: 1, MemoryKiB: 64, Threads: 1}

// env is a central (or node) with a projector, plus a clock tests control.
type env struct {
	t     *testing.T
	ctx   context.Context
	now   time.Time
	path  string
	st    *store.Store
	app   *App
	super Actor
	site  string
	loc   string
}

func newAuthor(t *testing.T, clock func() time.Time) (*store.Author, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &store.Author{
		NodeID:  newID(),
		ChainID: newID(),
		Signer:  event.Signer{KeyID: event.KeyID(pub), Key: priv},
		Clock:   hlc.New(clock, 0),
		Now:     clock,
	}, pub
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, ctx: context.Background(), now: time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)}
	clock := func() time.Time { return e.now }
	author, pub := newAuthor(t, clock)
	e.path = filepath.Join(t.TempDir(), "central.db")
	st, err := store.Open(e.path, store.WithApplier(&domain.Projector{LocalNodeID: author.NodeID}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.TrustKey(e.ctx, author.NodeID, pub); err != nil {
		t.Fatal(err)
	}
	e.st = st
	e.app = &App{Store: st, Author: author, Params: cheap, Now: clock, Blobs: &blobs.Store{Dir: filepath.Join(t.TempDir(), "blobs")}}

	superID, err := e.app.BootstrapSuperUser(e.ctx, NewUser{
		Username: "admin", LegalName: "Ada Admin", Email: "ada@example.org",
		IdentityVerification: "console bootstrap",
	}, "tumbleweed-gasket-42")
	if err != nil {
		t.Fatal(err)
	}
	e.super = Actor{UserID: superID, SessionID: newID()}
	e.site = e.must2(e.app.CreateSite(e.ctx, e.super, "NYC", "New York", "America/New_York"))
	e.loc = e.must2(e.app.CreateLocation(e.ctx, e.super, e.site, "", "Biomed shop", "room"))
	return e
}

// sign signs a work order as actor (acknowledging the clock warning:
// test authors have no verified clock).
func (e *env) sign(actor Actor, woID, meaning string) error {
	pw := "brass-kettle-orchard-7"
	if actor == e.super {
		pw = "tumbleweed-gasket-42"
	}
	return e.app.Sign(e.ctx, actor, woID, meaning, pw, true)
}

// reopen reopens the database with a different projector configuration.
func (e *env) reopen(p *domain.Projector) {
	e.t.Helper()
	st, err := store.Open(e.path, store.WithApplier(p))
	e.must(err)
	e.t.Cleanup(func() { st.Close() })
	e.st, e.app.Store = st, st
}

func (e *env) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) must2(s string, err error) string {
	e.t.Helper()
	e.must(err)
	return s
}

// activeUser creates and activates a user, returning their actor.
func (e *env) activeUser(username, role string) Actor {
	e.t.Helper()
	id, temp, err := e.app.CreateUser(e.ctx, e.super, NewUser{
		Username: username, LegalName: strings.ToUpper(username[:1]) + username[1:], Email: username + "@example.org",
		Role: role, HomeSites: []string{e.site}, IdentityVerification: "in person, badge checked",
	})
	e.must(err)
	a := Actor{UserID: id, SessionID: newID()}
	e.must(e.app.ChangePassword(e.ctx, a, temp, "brass-kettle-orchard-7"))
	return a
}

func (e *env) asset(tag string) string {
	e.t.Helper()
	return e.must2(e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{
		Tag: tag, LocationID: e.loc, Manufacturer: "Fluke", Model: "ESA615", Serial: "SN-" + tag, RiskClass: "high",
	}))
}

func wantRejection(t *testing.T, err error, flag string) {
	t.Helper()
	var r *store.Rejection
	if !errors.As(err, &r) || r.Flag != flag {
		t.Fatalf("err = %v, want rejection %s", err, flag)
	}
}

func TestBootstrapOnlyOnce(t *testing.T) {
	e := newEnv(t)
	_, err := e.app.BootstrapSuperUser(e.ctx, NewUser{Username: "second", LegalName: "S", Email: "s@example.org", IdentityVerification: "x"}, "another-good-password-9")
	if !errors.Is(err, ErrAlreadyBootstrap) {
		t.Fatalf("second bootstrap: %v", err)
	}
}

func TestUserLifecycle(t *testing.T) {
	e := newEnv(t)
	id, temp, err := e.app.CreateUser(e.ctx, e.super, NewUser{
		Username: "jdoe", LegalName: "Jane Doe", Email: "jane@example.org", Role: domain.RoleUser,
		HomeSites: []string{e.site}, IdentityVerification: "in person",
	})
	e.must(err)
	jane := Actor{UserID: id, SessionID: newID()}

	u, err := domain.GetUser(e.ctx, e.st.DB(), id)
	e.must(err)
	if u.Status != domain.UserStatusPending || !u.MustChangePassword {
		t.Fatalf("new user = %+v", u)
	}
	if _, err := e.app.Authenticate(e.ctx, "jdoe", temp); !errors.Is(err, ErrMustChangePassword) {
		t.Fatalf("login with one-time password: %v", err)
	}
	if _, err := e.app.Authenticate(e.ctx, "jdoe", "wrong"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, err := e.app.Authenticate(e.ctx, "nobody", "whatever"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("unknown user: %v", err)
	}
	// A pending user can do nothing but set their password.
	_, err = e.app.RegisterAsset(e.ctx, jane, domain.AssetRegistered{Tag: "X", LocationID: e.loc, Manufacturer: "M", Model: "M"})
	wantRejection(t, err, domain.FlagNotAuthorized)

	var pe *password.PolicyError
	if err := e.app.ChangePassword(e.ctx, jane, temp, "short"); !errors.As(err, &pe) {
		t.Fatalf("weak password: %v", err)
	}
	e.must(e.app.ChangePassword(e.ctx, jane, temp, "copper-ladder-sunrise"))
	u, err = e.app.Authenticate(e.ctx, "jdoe", "copper-ladder-sunrise")
	e.must(err)
	if u.Status != domain.UserStatusActive || !u.PasswordExpiresAt.Equal(e.now.Add(31*24*time.Hour)) {
		t.Fatalf("after activation = %+v", u)
	}

	// Reuse of a recent password is refused.
	e.must(e.app.ChangePassword(e.ctx, jane, "copper-ladder-sunrise", "granite-otter-meadow"))
	if err := e.app.ChangePassword(e.ctx, jane, "granite-otter-meadow", "copper-ladder-sunrise"); !errors.As(err, &pe) {
		t.Fatalf("reused password: %v", err)
	}

	// After 31 days the password must be changed before anything else.
	e.now = e.now.Add(32 * 24 * time.Hour)
	if _, err := e.app.Authenticate(e.ctx, "jdoe", "granite-otter-meadow"); !errors.Is(err, ErrMustChangePassword) {
		t.Fatalf("expired password: %v", err)
	}
	e.must(e.app.ChangePassword(e.ctx, jane, "granite-otter-meadow", "velvet-anchor-thistle"))
	_, err = e.app.Authenticate(e.ctx, "jdoe", "velvet-anchor-thistle")
	e.must(err)

	// Reset issues a one-time password that expires after 72 hours.
	temp2, err := e.app.ResetPassword(e.ctx, e.super, id, "phone call, callback to HR number")
	e.must(err)
	e.now = e.now.Add(73 * time.Hour)
	if _, err := e.app.Authenticate(e.ctx, "jdoe", temp2); !errors.Is(err, ErrTemporaryExpired) {
		t.Fatalf("expired one-time password: %v", err)
	}

	// Only super users manage accounts; usernames are never reused.
	wantRejection(t, e.app.DisableUser(e.ctx, jane, e.super.UserID, "coup"), domain.FlagNotAuthorized)
	e.must(e.app.DisableUser(e.ctx, e.super, id, "left the company"))
	if _, err := e.app.Authenticate(e.ctx, "jdoe", temp2); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("disabled user login: %v", err)
	}
	_, _, err = e.app.CreateUser(e.ctx, e.super, NewUser{Username: "jdoe", LegalName: "J", Email: "j@example.org", Role: domain.RoleUser, IdentityVerification: "x"})
	wantRejection(t, err, domain.FlagConflict)
}

func TestRoles(t *testing.T) {
	e := newEnv(t)
	mid := e.activeUser("mike", domain.RoleUser)
	_, err := e.app.CreateSite(e.ctx, mid, "BOS", "Boston", "America/New_York")
	wantRejection(t, err, domain.FlagNotAuthorized)
	e.must(e.app.ChangeRole(e.ctx, e.super, mid.UserID, domain.RoleMidTier))

	wantRejection(t, e.app.ChangeRole(e.ctx, e.super, e.super.UserID, domain.RoleUser), domain.FlagConflict)
	wantRejection(t, e.app.DisableUser(e.ctx, e.super, e.super.UserID, "oops"), domain.FlagConflict)
	_, err = e.app.CreateSite(e.ctx, e.super, "bad code", "X", "America/New_York")
	wantRejection(t, err, domain.FlagInvalid)
	_, err = e.app.CreateSite(e.ctx, e.super, "BOS", "Boston", "Mars/Olympus_Mons")
	wantRejection(t, err, domain.FlagInvalid)
}

func TestAssets(t *testing.T) {
	e := newEnv(t)
	tech := e.activeUser("tess", domain.RoleUser)
	id := e.asset("NYC-A-00001")
	_, err := e.app.RegisterAsset(e.ctx, tech, domain.AssetRegistered{Tag: "NYC-A-00001", LocationID: e.loc, Manufacturer: "M", Model: "M"})
	wantRejection(t, err, domain.FlagConflict)

	serial := "SN-NEW"
	e.must(e.app.UpdateAsset(e.ctx, tech, id, 1, domain.AssetUpdated{Serial: &serial}))
	// A second edit made from version 1 touching a different field merges.
	risk := "medium"
	e.must(e.app.UpdateAsset(e.ctx, tech, id, 1, domain.AssetUpdated{RiskClass: &risk}))
	// One touching the same field is a conflict.
	other := "SN-OTHER"
	wantRejection(t, e.app.UpdateAsset(e.ctx, tech, id, 1, domain.AssetUpdated{Serial: &other}), domain.FlagStaleBase)

	a, err := domain.GetAsset(e.ctx, e.st.DB(), id)
	e.must(err)
	if a.Serial != "SN-NEW" || a.RiskClass != "medium" || a.Version != 3 {
		t.Fatalf("asset = %+v", a)
	}

	wantRejection(t, e.app.SetAssetStatus(e.ctx, tech, id, a.Version, domain.AssetOutOfService, ""), domain.FlagInvalid)
	e.must(e.app.SetAssetStatus(e.ctx, tech, id, a.Version, domain.AssetOutOfService, "failed leakage test"))
	wantRejection(t, e.app.SetAssetStatus(e.ctx, tech, id, a.Version+1, domain.AssetRetired, "end of life"), domain.FlagNotAuthorized)
}

// remote is another node whose events reach this database through
// Ingest, as they would at central after a sync.
type remote struct {
	st     *store.Store
	author *store.Author
}

func (e *env) remote() *remote {
	e.t.Helper()
	author, pub := newAuthor(e.t, func() time.Time { return e.now })
	st, err := store.Open(filepath.Join(e.t.TempDir(), "remote.db")) // no projector: raw authoring
	e.must(err)
	e.t.Cleanup(func() { st.Close() })
	e.must(e.st.TrustKey(e.ctx, author.NodeID, pub))
	return &remote{st: st, author: author}
}

// write authors an event on the remote node now, without delivering it.
func (r *remote) write(e *env, actor Actor, typ, entityType, entityID string, base int64, lease string, payload any) event.Event {
	e.t.Helper()
	d, err := draft(actor, typ, entityType, entityID, base, lease, payload)
	e.must(err)
	ev, err := r.st.Append(e.ctx, r.author, d)
	e.must(err)
	return ev
}

func (e *env) flags(ev event.Event) []string {
	e.t.Helper()
	fs, err := e.st.Flags(e.ctx, ev.EventID)
	e.must(err)
	var out []string
	for _, f := range fs {
		out = append(out, fmt.Sprintf("%s/%v", f.Flag, f.Projected))
	}
	return out
}

func TestOfflineClaimRace(t *testing.T) {
	e := newEnv(t)
	alice, bob := e.activeUser("alice", domain.RoleUser), e.activeUser("bob", domain.RoleUser)
	woID, _, err := e.app.OpenWorkOrder(e.ctx, e.super, NewWorkOrder{Type: "pm", AssetID: e.asset("A1"), Priority: "normal", Title: "Annual PM"})
	e.must(err)

	// Both claim the same job on their own Pis while offline.
	ra, rb := e.remote(), e.remote()
	aliceClaim := ra.write(e, alice, domain.TypeWorkOrderClaimed, domain.EntityWorkOrder, woID, 1, "", domain.WorkOrderClaimed{LeaseID: newID()})
	bobClaim := rb.write(e, bob, domain.TypeWorkOrderClaimed, domain.EntityWorkOrder, woID, 1, "", domain.WorkOrderClaimed{LeaseID: newID()})

	// Bob syncs first and wins; Alice's claim is kept but flagged.
	e.must(e.st.Ingest(e.ctx, bobClaim))
	e.must(e.st.Ingest(e.ctx, aliceClaim))
	if got := e.flags(aliceClaim); !reflect.DeepEqual(got, []string{"duplicate_work/false"}) {
		t.Fatalf("alice's claim flags = %v", got)
	}
	w, err := domain.GetWorkOrder(e.ctx, e.st.DB(), woID)
	e.must(err)
	if w.AssignedTo != bob.UserID || w.Status != domain.WOAssigned {
		t.Fatalf("work order = %+v", w)
	}
	if _, err := e.st.Event(e.ctx, aliceClaim.EventID); err != nil {
		t.Fatalf("flagged event not kept: %v", err)
	}
}

func TestReassignmentOfflineGrace(t *testing.T) {
	e := newEnv(t)
	mid := e.activeUser("mona", domain.RoleMidTier)
	alice, bob := e.activeUser("alice", domain.RoleUser), e.activeUser("bob", domain.RoleUser)
	woID, number, err := e.app.OpenWorkOrder(e.ctx, alice, NewWorkOrder{Type: "corrective", AssetID: e.asset("A1"), Priority: "high", Title: "Alarm fault"})
	e.must(err)
	if !strings.HasPrefix(number, "NYC-WO-"+NodeShortCode(e.app.Author.NodeID)+"-00001") {
		t.Fatalf("number = %s", number)
	}
	aliceLease, err := e.app.AssignWorkOrder(e.ctx, mid, woID, alice.UserID)
	e.must(err)

	// Alice, offline, starts the work at 10:00.
	ra := e.remote()
	e.now = e.now.Add(time.Hour)
	started := ra.write(e, alice, domain.TypeWorkOrderStatusChanged, domain.EntityWorkOrder, woID, 2, aliceLease,
		domain.WorkOrderStatusChanged{From: domain.WOAssigned, To: domain.WOInProgress})

	// At 11:00 the supervisor, not knowing, reassigns to Bob.
	e.now = e.now.Add(time.Hour)
	_, err = e.app.AssignWorkOrder(e.ctx, mid, woID, bob.UserID)
	e.must(err)

	// At 12:00 Alice, still offline, puts it on hold.
	e.now = e.now.Add(time.Hour)
	held := ra.write(e, alice, domain.TypeWorkOrderStatusChanged, domain.EntityWorkOrder, woID, 3, aliceLease,
		domain.WorkOrderStatusChanged{From: domain.WOInProgress, To: domain.WOOnHold})

	// Work done before the reassignment is accepted under offline grace;
	// work done after it is flagged for a mid-tier user.
	e.must(e.st.Ingest(e.ctx, started))
	e.must(e.st.Ingest(e.ctx, held))
	if got := e.flags(started); len(got) != 0 {
		t.Fatalf("pre-reassignment work flagged: %v", got)
	}
	if got := e.flags(held); !reflect.DeepEqual(got, []string{"non_authorized/false"}) {
		t.Fatalf("post-reassignment work flags = %v", got)
	}
	w, err := domain.GetWorkOrder(e.ctx, e.st.DB(), woID)
	e.must(err)
	if w.Status != domain.WOInProgress || w.AssignedTo != bob.UserID {
		t.Fatalf("work order = %+v", w)
	}
}

func TestWorkOrderLifecycle(t *testing.T) {
	e := newEnv(t)
	mid := e.activeUser("mona", domain.RoleMidTier)
	tech := e.activeUser("tess", domain.RoleUser)
	other := e.activeUser("otto", domain.RoleUser)
	woID, _, err := e.app.OpenWorkOrder(e.ctx, mid, NewWorkOrder{Type: "pm", AssetID: e.asset("A1"), Priority: "normal", Title: "Annual PM", DueAt: "2026-11-01"})
	e.must(err)
	_, _, err = e.app.OpenWorkOrder(e.ctx, tech, NewWorkOrder{Type: "pm", AssetID: e.asset("A2"), Priority: "normal", Title: "PM"})
	wantRejection(t, err, domain.FlagNotAuthorized)

	step := func(a Actor, to, reason string) error { return e.app.ChangeWorkOrderStatus(e.ctx, a, woID, to, reason) }

	wantRejection(t, step(tech, domain.WOInProgress, ""), domain.FlagInvalid) // still open
	_, err = e.app.AssignWorkOrder(e.ctx, tech, woID, tech.UserID)
	wantRejection(t, err, domain.FlagNotAuthorized)
	_, err = e.app.AssignWorkOrder(e.ctx, mid, woID, tech.UserID)
	e.must(err)
	wantRejection(t, step(other, domain.WOInProgress, ""), domain.FlagNotAuthorized) // not the holder
	e.must(step(tech, domain.WOInProgress, ""))
	e.must(e.sign(tech, woID, domain.MeaningPerformed))
	wantRejection(t, e.sign(tech, woID, domain.MeaningReviewed), domain.FlagNotAuthorized) // tech can't review
	e.must(step(mid, domain.WOInProgress, "as-left reading missing"))                      // reopen
	e.must(e.sign(tech, woID, domain.MeaningPerformed))
	e.must(e.sign(mid, woID, domain.MeaningReviewed))
	e.must(e.sign(mid, woID, domain.MeaningApproved))
	wantRejection(t, step(mid, domain.WOCancelled, "too late"), domain.FlagInvalid)

	w, err := domain.GetWorkOrder(e.ctx, e.st.DB(), woID)
	e.must(err)
	if w.Status != domain.WOClosed || w.LeaseID != "" {
		t.Fatalf("closed work order = %+v", w)
	}

	// Two-person rule: a mid-tier user who performed the work can't review it.
	wo2, _, err := e.app.OpenWorkOrder(e.ctx, mid, NewWorkOrder{Type: "inspection", AssetID: e.asset("A3"), Priority: "low", Title: "Inspect"})
	e.must(err)
	_, err = e.app.AssignWorkOrder(e.ctx, mid, wo2, mid.UserID)
	e.must(err)
	e.must(e.app.ChangeWorkOrderStatus(e.ctx, mid, wo2, domain.WOInProgress, ""))
	e.must(e.sign(mid, wo2, domain.MeaningPerformed))
	wantRejection(t, e.sign(mid, wo2, domain.MeaningReviewed), domain.FlagNotAuthorized)
	e.must(e.sign(e.super, wo2, domain.MeaningReviewed))

	wantRejection(t, e.app.ChangeWorkOrderStatus(e.ctx, mid, wo2, domain.WOCancelled, ""), domain.FlagInvalid) // needs reason
}

func TestOutOfServiceWinsConcurrentEdit(t *testing.T) {
	e := newEnv(t)
	tech, tech2 := e.activeUser("tess", domain.RoleUser), e.activeUser("tom", domain.RoleUser)
	id := e.asset("A1")
	ra, rb := e.remote(), e.remote()

	// Both see version 1. One marks it missing, the other out of service.
	missing := ra.write(e, tech, domain.TypeAssetStatusChanged, domain.EntityAsset, id, 1, "", domain.AssetStatusChanged{Status: domain.AssetMissing, Reason: "not in room"})
	oos := rb.write(e, tech2, domain.TypeAssetStatusChanged, domain.EntityAsset, id, 1, "", domain.AssetStatusChanged{Status: domain.AssetOutOfService, Reason: "cracked housing"})
	back := rb.write(e, tech2, domain.TypeAssetStatusChanged, domain.EntityAsset, id, 1, "", domain.AssetStatusChanged{Status: domain.AssetInService})

	e.must(e.st.Ingest(e.ctx, missing))
	e.must(e.st.Ingest(e.ctx, oos))  // more restrictive: applied and flagged
	e.must(e.st.Ingest(e.ctx, back)) // less restrictive and stale: rejected

	if got := e.flags(oos); !reflect.DeepEqual(got, []string{"stale_base/true"}) {
		t.Fatalf("out-of-service flags = %v", got)
	}
	if got := e.flags(back); !reflect.DeepEqual(got, []string{"stale_base/false"}) {
		t.Fatalf("back-in-service flags = %v", got)
	}
	a, err := domain.GetAsset(e.ctx, e.st.DB(), id)
	e.must(err)
	if a.Status != domain.AssetOutOfService {
		t.Fatalf("status = %s", a.Status)
	}
}

func TestSystemActorOnlyFromLocalNode(t *testing.T) {
	e := newEnv(t)
	r := e.remote()
	ev := r.write(e, Console, domain.TypeSiteCreated, domain.EntitySite, newID(), 0, "", domain.SiteCreated{Code: "EVIL", Name: "x", Timezone: "UTC"})
	e.must(e.st.Ingest(e.ctx, ev))
	if got := e.flags(ev); !reflect.DeepEqual(got, []string{"non_authorized/false"}) {
		t.Fatalf("flags = %v", got)
	}
}

// snapshot dumps every projection table for comparison.
func snapshot(t *testing.T, db *sql.DB) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, table := range []string{"users", "user_password_history", "user_lockouts", "sites", "locations", "assets",
		"work_orders", "wo_leases", "pm_schedules", "calibration_records", "cal_points", "cal_standards",
		"parts", "stock_locations", "stock_txns", "stock_levels", "event_flags"} {
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
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			out[table] = append(out[table], fmt.Sprint(vals...))
		}
		rows.Close()
	}
	return out
}

func TestRebuildMatchesLiveProjections(t *testing.T) {
	e := newEnv(t)
	// Exercise a bit of everything, including flagged ingests.
	e.TestScenario()
	before := snapshot(t, e.st.DB())
	for _, table := range []string{"users", "assets", "work_orders", "wo_leases", "event_flags", "pm_schedules",
		"calibration_records", "cal_points", "cal_standards", "stock_txns", "stock_levels", "user_lockouts"} {
		if len(before[table]) == 0 {
			t.Fatalf("scenario left %s empty; the comparison would prove nothing", table)
		}
	}
	e.must(e.st.Rebuild(e.ctx))
	after := snapshot(t, e.st.DB())
	if !reflect.DeepEqual(before, after) {
		for k := range before {
			if !reflect.DeepEqual(before[k], after[k]) {
				t.Errorf("table %s differs after rebuild:\nbefore %v\nafter  %v", k, before[k], after[k])
			}
		}
	}
	rep, err := e.st.Verify(e.ctx)
	e.must(err)
	if !rep.OK() {
		t.Fatalf("verify: %v", rep.Problems)
	}
}

// TestScenario runs a mixed workload on e.
func (e *env) TestScenario() {
	mid := e.activeUser("mona", domain.RoleMidTier)
	tech, tech2 := e.activeUser("tess", domain.RoleUser), e.activeUser("tom", domain.RoleUser)
	id := e.asset("A1")
	serial := "SN-2"
	e.must(e.app.UpdateAsset(e.ctx, tech, id, 1, domain.AssetUpdated{Serial: &serial}))
	woID, _, err := e.app.OpenWorkOrder(e.ctx, tech, NewWorkOrder{Type: "corrective", AssetID: id, Priority: "urgent", Title: "No power"})
	e.must(err)
	r := e.remote()
	claim := r.write(e, tech2, domain.TypeWorkOrderClaimed, domain.EntityWorkOrder, woID, 1, "", domain.WorkOrderClaimed{LeaseID: newID()})
	_, err = e.app.AssignWorkOrder(e.ctx, mid, woID, tech.UserID)
	e.must(err)
	e.must(e.st.Ingest(e.ctx, claim)) // flagged duplicate_work
	e.must(e.app.ChangeWorkOrderStatus(e.ctx, tech, woID, domain.WOInProgress, ""))

	// Calibration against a scheduled standard, completed.
	std := e.must2(e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "STD", LocationID: e.loc, Manufacturer: "F", Model: "S", IsReferenceStandard: true}))
	e.must2(e.app.CreateSchedule(e.ctx, mid, domain.PMScheduleCreated{AssetID: std, WOType: "calibration", Title: "Cal", IntervalDays: 365, FirstDue: "2026-10-10"}))
	numbers, err := e.app.GenerateDueWorkOrders(e.ctx, WorkingSetWindow)
	e.must(err)
	if len(numbers) != 1 {
		e.t.Fatalf("generated %v", numbers)
	}
	calWO := e.openHeld(mid, tech, "calibration", id)
	_, err = e.app.RecordCalibration(e.ctx, tech, calibration(calWO, []string{std}, true, voltage("125", "120.2"), withAsLeft(leakage("10"), "8")))
	e.must(err)
	e.must(e.sign(tech, calWO, domain.MeaningPerformed))

	// Stock, including a count and a reversal.
	part := e.must2(e.app.CreatePart(e.ctx, mid, domain.PartCreated{PartNo: "P1", Description: "Part", Unit: "each"}))
	shop := e.must2(e.app.CreateStockLocation(e.ctx, mid, domain.StockLocationCreated{SiteID: e.site, Name: "Shop"}))
	e.must2(e.app.RecordStock(e.ctx, tech, domain.StockTxnRecorded{Kind: domain.StockReceive, PartID: part, StockLocationID: shop, Quantity: 5}))
	txn := e.must2(e.app.RecordStock(e.ctx, tech, domain.StockTxnRecorded{Kind: domain.StockIssue, PartID: part, StockLocationID: shop, Quantity: 2, WorkOrderID: woID}))
	e.must2(e.app.RecordStock(e.ctx, tech, domain.StockTxnRecorded{Kind: domain.StockCount, PartID: part, StockLocationID: shop, ObservedQty: ptr[int64](2)}))
	e.must(e.app.ReverseStock(e.ctx, mid, txn, "wrong part"))

	// A lockout.
	for i := 0; i < MaxFailedLogins; i++ {
		e.app.Authenticate(e.ctx, "tom", "wrong-password")
	}
}

func TestRedactionKeepsProjectionsRebuildable(t *testing.T) {
	e := newEnv(t)
	mid := e.activeUser("mona", domain.RoleMidTier)
	tech := e.activeUser("tess", domain.RoleUser)
	assetID := e.asset("A1")
	woID, number, err := e.app.OpenWorkOrder(e.ctx, tech, NewWorkOrder{
		Type: "corrective", AssetID: assetID, Priority: "high", Title: "Pump alarm", Problem: "Alarm while on patient John Smith MRN 1234567",
	})
	e.must(err)
	w, err := domain.GetWorkOrder(e.ctx, e.st.DB(), woID)
	e.must(err)
	var openedEvent string
	e.must(e.st.DB().QueryRowContext(e.ctx, `SELECT event_id FROM events WHERE type = ? AND entity_id = ?`, domain.TypeWorkOrderOpened, woID).Scan(&openedEvent))

	replacement := domain.WorkOrderOpened{Number: number, Type: w.Type, AssetID: assetID, Priority: w.Priority, Title: w.Title, Problem: "Alarm during use [redacted]"}
	err = e.app.Redact(e.ctx, tech, openedEvent, "contained PHI", replacement)
	wantRejection(t, err, domain.FlagNotAuthorized)
	e.must(e.app.Redact(e.ctx, mid, openedEvent, "contained PHI", replacement))

	w, err = domain.GetWorkOrder(e.ctx, e.st.DB(), woID)
	e.must(err)
	if w.Problem != "Alarm during use [redacted]" {
		t.Fatalf("problem after redaction = %q", w.Problem)
	}
	var stored []byte
	e.must(e.st.DB().QueryRowContext(e.ctx, `SELECT payload FROM events WHERE event_id = ?`, openedEvent).Scan(&stored))
	if stored != nil {
		t.Fatalf("original payload still stored: %s", stored)
	}
	rep, err := e.st.Verify(e.ctx)
	e.must(err)
	if !rep.OK() || rep.Redacted != 1 {
		t.Fatalf("verify after redaction = %+v", rep)
	}
}

func withAsLeft(p domain.CalPoint, asLeft string) domain.CalPoint {
	p.AsLeft = asLeft
	return p
}
