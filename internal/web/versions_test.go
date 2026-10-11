package web

import (
	"net/url"
	"strings"
	"testing"

	"pi-fleet/internal/fleetsync"
)

func TestVersionsOnThePisPageAndOnAnOldPi(t *testing.T) {
	e := newEnv(t)
	piSrv, piStore, _ := e.employeePi(e.srv.URL, "tess")
	var nodeID string
	e.app.Store.DB().QueryRow(`SELECT id FROM nodes`).Scan(&nodeID)
	e.app.Store.SetConfig(e.ctx, "node_version:"+nodeID, "v0.3.0") // as an old Pi reports at hello

	admin := e.browser()
	admin.login("admin", "tumbleweed-gasket-42")
	_, _, page := admin.get("/admin/nodes")
	for _, want := range []string{"v0.3.0", "needs updating", "This master Pi runs pi-fleet v0.7.2", "need " + fleetsync.MinNodeVersion + " or newer"} {
		if !strings.Contains(page, want) {
			t.Errorf("Pis page lacks %q", want)
		}
	}
	if _, page = admin.post("/admin/nodes/require", url.Values{"version": {"v0.9.0"}, "from": {"2026-10-14"}}); !strings.Contains(page, "Not saved: the master Pi runs v0.7.2") {
		t.Fatalf("required a version newer than the master:\n%s", page)
	}
	if _, page = admin.post("/admin/nodes/require", url.Values{"version": {"v0.7.2"}, "from": {"2026-10-14"}}); !strings.Contains(page, "Employee Pis need pi-fleet v0.7.2 from 14 October 2026") ||
		!strings.Contains(page, "and v0.7.2 or newer from 14 October 2026") {
		t.Fatalf("requirement:\n%s", page)
	}
	if _, page = admin.post("/admin/nodes/require", url.Values{"clear": {"yes"}}); !strings.Contains(page, "only the built-in minimum") {
		t.Fatalf("clear:\n%s", page)
	}

	// The employee Pi says what to do.
	pi := *e
	pi.srv = piSrv
	b := pi.browser()
	b.login("tess", "copper-ladder-sunrise")
	piStore.SetConfig(e.ctx, fleetsync.ConfigUpdateDue, "v0.7.2 2026-10-14T06:00:00Z")
	if _, _, page = b.get("/"); !strings.Contains(page, "Please update this Pi to pi-fleet v0.7.2 before 14 October 2026") {
		t.Fatalf("no reminder:\n%s", page)
	}
	piStore.SetConfig(e.ctx, fleetsync.ConfigUpdateNeeded, "v0.7.2")
	if _, _, page = b.get("/"); !strings.Contains(page, "This Pi needs updating.") || !strings.Contains(page, "Update-Pi") {
		t.Fatalf("no banner:\n%s", page)
	}
}
