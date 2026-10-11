package app

import (
	"errors"
	"testing"

	"pi-fleet/internal/domain"
	"pi-fleet/internal/store"
)

func (e *env) sitePath(id string) (path, lineage string) {
	e.t.Helper()
	e.must(e.app.Store.DB().QueryRowContext(e.ctx, `SELECT path, lineage FROM sites WHERE id = ?`, id).Scan(&path, &lineage))
	return path, lineage
}

func TestSitesInsideSites(t *testing.T) {
	e := newEnv(t)
	main := e.must2(e.app.CreateSite(e.ctx, e.super, "MAIN", "Main Hospital", "America/Denver"))
	north := e.must2(e.app.CreateSiteIn(e.ctx, e.super, main, "NORTH", "North Satellite Clinic", "America/Denver"))
	east := e.must2(e.app.CreateSiteIn(e.ctx, e.super, north, "EAST", "East Mobile Unit", "America/Denver"))
	if p, l := e.sitePath(east); p != "MAIN › NORTH › EAST" || l != "/"+main+"/"+north+"/"+east+"/" {
		t.Fatalf("east: %q %q", p, l)
	}

	var rej *store.Rejection
	if err := e.app.MoveSite(e.ctx, e.super, main, east); !errors.As(err, &rej) {
		t.Fatalf("site moved inside its own satellite: %v", err)
	}
	if err := e.app.MoveSite(e.ctx, e.super, main, main); !errors.As(err, &rej) {
		t.Fatalf("site moved inside itself: %v", err)
	}
	tess := e.activeUser("tess", domain.RoleUser)
	if err := e.app.MoveSite(e.ctx, tess, north, ""); !errors.As(err, &rej) {
		t.Fatalf("an ordinary user moved a site: %v", err)
	}

	// Moving NORTH to the top takes EAST with it.
	e.must(e.app.MoveSite(e.ctx, e.super, north, ""))
	if p, l := e.sitePath(east); p != "NORTH › EAST" || l != "/"+north+"/"+east+"/" {
		t.Fatalf("after moving north: %q %q", p, l)
	}
	// ...and under the original NYC site, everything follows again.
	e.must(e.app.MoveSite(e.ctx, e.super, north, e.site))
	if p, _ := e.sitePath(east); p != "NYC › NORTH › EAST" {
		t.Fatalf("after moving under NYC: %q", p)
	}
	e.must(e.st.Rebuild(e.ctx))
	if p, l := e.sitePath(east); p != "NYC › NORTH › EAST" || l != "/"+e.site+"/"+north+"/"+east+"/" {
		t.Fatalf("after rebuild: %q %q", p, l)
	}
	if _, err := e.app.CreateSiteIn(e.ctx, e.super, domain.UnallocatedSiteID, "X1", "X", "UTC"); !errors.As(err, &rej) {
		t.Fatalf("site inside Unallocated: %v", err)
	}
}
