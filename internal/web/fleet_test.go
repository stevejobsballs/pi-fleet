package web

import (
	"crypto/ed25519"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// employeePi activates a Pi for a new user against central's sync API and
// serves its web interface.
func (e *env) employeePi(syncURL, username string) (*httptest.Server, *store.Store, string) {
	e.t.Helper()
	cheap := password.Params{Time: 1, MemoryKiB: 64, Threads: 1}
	userID, temp, err := e.app.CreateUser(e.ctx, e.super, app.NewUser{Username: username, LegalName: "Tess Tech", Email: username + "@example.org",
		Role: domain.RoleUser, HomeSites: []string{e.site}, IdentityVerification: "badge"})
	if err != nil {
		e.t.Fatal(err)
	}
	dir := e.t.TempDir()
	k, _, err := keys.LoadOrCreate(filepath.Join(dir, "keys"))
	if err != nil {
		e.t.Fatal(err)
	}
	nodeID, chainID := uuid.Must(uuid.NewV7()).String(), uuid.Must(uuid.NewV7()).String()
	proj := &domain.Projector{LocalNodeID: nodeID}
	st, err := store.Open(filepath.Join(dir, "pi.db"), store.WithApplier(proj))
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { st.Close() })
	st.TrustKey(e.ctx, nodeID, k.Event.Public().(ed25519.PublicKey))
	clock := func() time.Time { return e.now }
	author := &store.Author{NodeID: nodeID, ChainID: chainID, Signer: k.EventSigner(), Clock: hlc.New(clock, 0), Now: clock}
	st.Append(e.ctx, author, event.Draft{ActorUserID: "system:init", ActorSessionID: "s", Type: event.TypeChainStarted,
		EntityType: "chain", EntityID: chainID, SchemaVersion: 1, Payload: []byte(`{}`)})
	bl := &blobs.Store{Dir: filepath.Join(dir, "blobs")}
	cl := &fleetsync.Client{BaseURL: syncURL, Store: st, Keys: k, NodeID: nodeID, ChainID: chainID, Now: clock, Wipe: func() error { return nil }, Blobs: bl}
	if syncURL == e.srv.URL {
		cl.HTTP = e.srv.Client() // trusts the test master's certificate, like master.pem on a real Pi
	}
	act, err := cl.Activate(e.ctx, username, temp, "copper-ladder-sunrise", cheap)
	if err != nil {
		e.t.Fatal(err)
	}
	proj.CentralNodeID = act.CentralNodeID
	if err := e.app.ConfirmNode(e.ctx, e.super, nodeID, act.PairingWords); err != nil {
		e.t.Fatal(err)
	}
	if _, err := cl.Sync(e.ctx); err != nil {
		e.t.Fatal(err)
	}
	// As cmd/pi-fleet sets up a real Pi.
	ui := &Server{App: &app.App{Store: st, Author: author, Params: cheap, Now: clock, Blobs: bl, QueueUploads: true}, Role: "node", Fleet: cl, Now: clock}
	h, err := ui.Handler()
	if err != nil {
		e.t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	e.t.Cleanup(srv.Close)
	return srv, st, userID
}

func TestLiveFleetFromAnEmployeePi(t *testing.T) {
	e := newEnv(t)
	sync := httptest.NewServer((&fleetsync.Server{App: e.app, CentralKey: e.key, Now: func() time.Time { return e.now }, Logf: t.Logf,
		Fleet: (&FleetAPI{App: e.app}).Handler()}).Handler())
	defer sync.Close()

	// Equipment at another site, not due for anything: not in Tess's working set.
	bos, _ := e.app.CreateSite(e.ctx, e.super, "BOS", "Boston", "America/New_York")
	bosLoc, _ := e.app.CreateLocation(e.ctx, e.super, bos, "", "OR 3", "room")
	far, _ := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "BOS-0042", LocationID: bosLoc, Manufacturer: "Philips", Model: "IntelliVue", Serial: "PH-9"})
	// A shared stockroom with stock.
	part, _ := e.app.CreatePart(e.ctx, e.super, domain.PartCreated{PartNo: "FUSE-2A", Description: "Fuse", Unit: "each"})
	shop, _ := e.app.CreateStockLocation(e.ctx, e.super, domain.StockLocationCreated{SiteID: e.site, Name: "Biomed stockroom"})
	e.app.RecordStock(e.ctx, e.super, domain.StockTxnRecorded{Kind: domain.StockReceive, PartID: part, StockLocationID: shop, Quantity: 10})

	piSrv, piStore, tessID := e.employeePi(sync.URL, "tess")
	piEnv := *e
	piEnv.srv = piSrv
	b := piEnv.browser()
	b.login("tess", "copper-ladder-sunrise")

	if _, _, page := b.get("/assets?q=BOS-0042"); !strings.Contains(page, "No equipment matches") {
		t.Fatal("far equipment should not be in the working set")
	}
	_, _, page := b.get("/fleet/assets?q=BOS")
	if !strings.Contains(page, "BOS-0042") || !strings.Contains(page, "Philips IntelliVue") {
		t.Fatalf("fleet search:\n%s", page)
	}
	if _, _, page = b.get("/fleet/assets/" + far); !strings.Contains(page, "PH-9") || !strings.Contains(page, "OR 3") {
		t.Fatalf("fleet asset:\n%s", page)
	}
	var stored int
	piStore.DB().QueryRow(`SELECT count(*) FROM assets WHERE id = ?`, far).Scan(&stored)
	if stored != 0 {
		t.Fatal("live fleet data was stored on the Pi")
	}

	// A shared stockroom count goes straight to central.
	b.get("/inventory")
	_, page = b.post("/inventory/txn", url.Values{"kind": {"count"}, "part": {part}, "location": {shop}, "quantity": {"7"}})
	if !strings.Contains(page, "stock level is now 7") {
		t.Fatalf("shared count:\n%s", page)
	}
	if level, _ := e.app.StockLevel(e.ctx, part, shop); level != 7 {
		t.Fatalf("central level = %d", level)
	}
	var actor string
	e.app.Store.DB().QueryRow(`SELECT actor_user_id FROM events WHERE type = 'stock.txn_recorded' ORDER BY local_order DESC LIMIT 1`).Scan(&actor)
	if actor != tessID {
		t.Fatalf("count attributed to %s, want Tess", actor)
	}

	// Offline: a clear message, not an error page.
	sync.Close()
	if _, _, page = b.get("/fleet/assets?q=BOS"); !strings.Contains(page, "couldn&#39;t be reached") {
		t.Fatalf("offline fleet search:\n%s", page)
	}
	if _, page = b.post("/inventory/txn", url.Values{"kind": {"count"}, "part": {part}, "location": {shop}, "quantity": {"6"}}); !strings.Contains(page, "couldn&#39;t be reached") {
		t.Fatalf("offline shared count:\n%s", page)
	}
}
