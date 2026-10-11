package app

import (
	"errors"
	"strings"
	"testing"

	"pi-fleet/internal/domain"
	"pi-fleet/internal/store"
)

func (e *env) propose(actor Actor, name string) string {
	e.t.Helper()
	return e.must2(e.app.ProposeLocation(e.ctx, actor, domain.LocationProposed{SiteID: e.site, Name: name, Kind: "room",
		Details: domain.LocationDetails{Building: "East wing", Floor: "3", Room: " 312 ", Department: "Radiology"}}))
}

func (e *env) registerAt(actor Actor, tag, loc string) string {
	e.t.Helper()
	return e.must2(e.app.RegisterAsset(e.ctx, actor, domain.AssetRegistered{Tag: tag, LocationID: loc, Manufacturer: "GE", Model: "Logiq"}))
}

func (e *env) placeOf(assetID string) (site, loc string) {
	e.t.Helper()
	e.must(e.app.Store.DB().QueryRowContext(e.ctx, `SELECT site_id, location_id FROM assets WHERE id = ?`, assetID).Scan(&site, &loc))
	return site, loc
}

func TestProposedLocationApproved(t *testing.T) {
	e := newEnv(t)
	tess := e.activeUser("tess", domain.RoleUser)
	loc := e.propose(tess, "Ultrasound 3")
	a := e.registerAt(tess, "US-1", loc)

	pending, err := domain.PendingLocations(e.ctx, e.app.Store.DB())
	e.must(err)
	if len(pending) != 1 || pending[0].ID != loc || pending[0].Details.Room != "312" || !strings.Contains(pending[0].ProposedBy, "tess") ||
		len(pending[0].Equipment) != 1 || !strings.HasPrefix(pending[0].Equipment[0], "US-1") {
		t.Fatalf("pending = %+v", pending)
	}
	if err := e.app.ReviewLocation(e.ctx, tess, loc, true, ""); err == nil {
		t.Fatal("an ordinary user approved a location")
	}
	e.must(e.app.ReviewLocation(e.ctx, e.super, loc, true, ""))
	if site, l := e.placeOf(a); site != e.site || l != loc {
		t.Fatalf("approved location's equipment at %s/%s", site, l)
	}
	if err := e.app.ReviewLocation(e.ctx, e.super, loc, false, "changed my mind"); err == nil {
		t.Fatal("reviewed twice")
	}
	if pending, _ := domain.PendingLocations(e.ctx, e.app.Store.DB()); len(pending) != 0 {
		t.Fatalf("still pending: %+v", pending)
	}
}

func TestRejectedLocationsEquipmentGoesToUnallocated(t *testing.T) {
	e := newEnv(t)
	tess := e.activeUser("tess", domain.RoleUser)
	loc := e.propose(tess, "Closet B")
	a := e.registerAt(tess, "PUMP-9", loc)
	b := e.registerAt(tess, "PUMP-10", loc)

	if err := e.app.ReviewLocation(e.ctx, e.super, loc, false, " "); err == nil {
		t.Fatal("rejected without a reason")
	}
	e.must(e.app.ReviewLocation(e.ctx, e.super, loc, false, "not a real location; use Biomed shop"))
	for _, id := range []string{a, b} {
		if site, l := e.placeOf(id); site != domain.UnallocatedSiteID || l != domain.UnallocatedLocationID {
			t.Fatalf("rejected location's equipment at %s/%s", site, l)
		}
	}
	var name, code string
	e.must(e.app.Store.DB().QueryRowContext(e.ctx, `SELECT name, code FROM sites WHERE id = ?`, domain.UnallocatedSiteID).Scan(&name, &code))
	if name != "Unallocated" || code != "UNALLOC" {
		t.Fatalf("site %s %s", code, name)
	}
	// Someone offline still registering there lands in Unallocated too.
	c := e.registerAt(tess, "PUMP-11", loc)
	if site, _ := e.placeOf(c); site != domain.UnallocatedSiteID {
		t.Fatalf("registered at a rejected location: %s", site)
	}
	// It can be moved out again to a real location.
	ast, err := domain.GetAsset(e.ctx, e.app.Store.DB(), a)
	e.must(err)
	e.must(e.app.RelocateAsset(e.ctx, tess, a, ast.Version, e.loc))
	if site, l := e.placeOf(a); site != e.site || l != e.loc {
		t.Fatalf("moved to %s/%s", site, l)
	}
	// A rebuild arrives at the same place.
	e.must(e.st.Rebuild(e.ctx))
	if site, _ := e.placeOf(b); site != domain.UnallocatedSiteID {
		t.Fatalf("after rebuild: %s", site)
	}
	if site, _ := e.placeOf(a); site != e.site {
		t.Fatalf("after rebuild: %s", site)
	}
}

func TestProposedLocationRules(t *testing.T) {
	e := newEnv(t)
	tess := e.activeUser("tess", domain.RoleUser)
	var rej *store.Rejection
	if _, err := e.app.ProposeLocation(e.ctx, tess, domain.LocationProposed{SiteID: e.site, Name: "biomed SHOP ", Kind: "room"}); !errors.As(err, &rej) {
		t.Fatalf("duplicate of an existing location: %v", err)
	}
	if _, err := e.app.ProposeLocation(e.ctx, tess, domain.LocationProposed{SiteID: e.site, Name: "", Kind: "room"}); !errors.As(err, &rej) {
		t.Fatalf("no name: %v", err)
	}
	loc := e.propose(tess, "Closet C")
	e.must(e.app.ReviewLocation(e.ctx, e.super, loc, false, "no"))
	// Once rejected, the name can be proposed again (say, correctly this time).
	e.propose(tess, "Closet C")
	if _, err := e.app.ProposeLocation(e.ctx, tess, domain.LocationProposed{SiteID: domain.UnallocatedSiteID, Name: "X", Kind: "room"}); !errors.As(err, &rej) {
		t.Fatalf("location at Unallocated: %v", err)
	}
	if _, err := e.app.CreateSite(e.ctx, e.super, "UNALLOC", "Mine", "UTC"); !errors.As(err, &rej) {
		t.Fatalf("site code UNALLOC: %v", err)
	}
}
