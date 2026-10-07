package app

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"pi-fleet/internal/domain"
	"pi-fleet/internal/store"
)

func (e *env) assetWithMasterID(tag, masterID string) string {
	e.t.Helper()
	return e.must2(e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{
		Tag: tag, LocationID: e.loc, Manufacturer: "Fluke", Model: "ESA615", Serial: "SN-" + tag, MasterID: masterID,
	}))
}

func (e *env) getAsset(id string) domain.Asset {
	e.t.Helper()
	a, err := domain.GetAsset(e.ctx, e.st.DB(), id)
	e.must(err)
	return a
}

func TestMergeDuplicateEquipment(t *testing.T) {
	e := newEnv(t)
	mid, tech := e.activeUser("mona", domain.RoleMidTier), e.activeUser("tess", domain.RoleUser)
	keep := e.assetWithMasterID("NYC-0001", "M-100")
	dup := e.assetWithMasterID("PROV-7", "M-100") // the same pump, registered again
	other := e.assetWithMasterID("NYC-0002", "M-200")
	none := e.assetWithMasterID("NYC-0003", "")

	groups, err := domain.Duplicates(e.ctx, e.st.DB(), "")
	e.must(err)
	if len(groups) != 1 || groups[0].MasterID != "M-100" || len(groups[0].Assets) != 2 {
		t.Fatalf("duplicates = %+v", groups)
	}

	// Signed work on the duplicate, and it is out of service.
	wo := e.openHeld(mid, tech, "corrective", dup)
	e.must(e.sign(tech, wo, domain.MeaningPerformed))
	e.must(e.app.SetAssetStatus(e.ctx, tech, dup, e.getAsset(dup).Version, domain.AssetOutOfService, "cracked housing"))

	// Only mid-tier users merge, only records with the same MasterID, with a reason.
	wantRejection(t, e.app.MergeAssets(e.ctx, tech, keep, 1, dup, "same pump", true), domain.FlagNotAuthorized)
	wantRejection(t, e.app.MergeAssets(e.ctx, mid, keep, 1, other, "same pump", true), domain.FlagConflict)
	wantRejection(t, e.app.MergeAssets(e.ctx, mid, none, 1, keep, "same pump", true), domain.FlagConflict)
	wantRejection(t, e.app.MergeAssets(e.ctx, mid, keep, 1, dup, " ", true), domain.FlagInvalid)
	wantRejection(t, e.app.MergeAssets(e.ctx, mid, keep, 1, keep, "itself", true), domain.FlagInvalid)

	e.must(e.app.MergeAssets(e.ctx, mid, keep, 1, dup, "PROV-7 is NYC-0001 registered twice", true))
	k, d := e.getAsset(keep), e.getAsset(dup)
	if d.MergedInto != keep || k.MergedInto != "" {
		t.Fatalf("merged_into: kept %q, merged %q", k.MergedInto, d.MergedInto)
	}
	if k.Status != domain.AssetOutOfService {
		t.Fatalf("kept record status = %s, want out of service like the record merged into it", k.Status)
	}
	if g, _ := domain.AssetGroup(e.ctx, e.st.DB(), keep); !reflect.DeepEqual(g, []string{keep, dup}) {
		t.Fatalf("group = %v", g)
	}
	if groups, _ = domain.Duplicates(e.ctx, e.st.DB(), ""); len(groups) != 0 {
		t.Fatalf("duplicates after merge = %+v", groups)
	}
	// The signed work order is untouched and its signature still valid.
	w, err := domain.GetWorkOrder(e.ctx, e.st.DB(), wo)
	e.must(err)
	sigs, err := domain.WorkOrderSignatures(e.ctx, e.st.DB(), wo)
	e.must(err)
	if w.AssetID != dup || len(sigs) != 1 || sigs[0].Stale {
		t.Fatalf("work order asset %s, signatures %+v", w.AssetID, sigs)
	}

	// The merged record takes no more changes, and can't be merged again.
	serial := "X"
	wantRejection(t, e.app.UpdateAsset(e.ctx, tech, dup, d.Version, domain.AssetUpdated{Serial: &serial}), domain.FlagConflict)
	wantRejection(t, e.app.RelocateAsset(e.ctx, tech, dup, d.Version, e.loc), domain.FlagConflict)
	wantRejection(t, e.app.MergeAssets(e.ctx, mid, keep, k.Version, dup, "again", true), domain.FlagConflict)
	wantRejection(t, e.app.MergeAssets(e.ctx, mid, dup, d.Version, keep, "backwards", true), domain.FlagConflict)

	// A third record of the same equipment; merging the kept one into it
	// carries the earlier merge along.
	third := e.assetWithMasterID("BOS-0042", "M-100")
	e.must(e.app.MergeAssets(e.ctx, mid, third, 1, keep, "moved to Boston and re-registered", true))
	if d = e.getAsset(dup); d.MergedInto != third {
		t.Fatalf("earlier merged record points at %s, want %s", d.MergedInto, third)
	}
	if g, _ := domain.AssetGroup(e.ctx, e.st.DB(), third); len(g) != 3 {
		t.Fatalf("group = %v", g)
	}

	// Rebuilding from the events gives the same state.
	before := snapshot(t, e.st.DB())
	e.must(e.st.Rebuild(e.ctx))
	if after := snapshot(t, e.st.DB()); !reflect.DeepEqual(before["assets"], after["assets"]) {
		t.Fatalf("assets differ after rebuild:\n%v\n%v", before["assets"], after["assets"])
	}
}

// A Pi that hasn't synced since the merge changes the merged record.
func TestStatusChangeOnMergedRecordFromAnOfflinePi(t *testing.T) {
	e := newEnv(t)
	mid, tech := e.activeUser("mona", domain.RoleMidTier), e.activeUser("tess", domain.RoleUser)
	keep := e.assetWithMasterID("NYC-0001", "M-100")
	dup := e.assetWithMasterID("PROV-7", "M-100")
	pi := e.remote()
	oos := pi.write(e, tech, domain.TypeAssetStatusChanged, domain.EntityAsset, dup, 1, "",
		domain.AssetStatusChanged{Status: domain.AssetOutOfService, Reason: "fails leakage test"})
	edit := "SN-NEW"
	upd := pi.write(e, tech, domain.TypeAssetUpdated, domain.EntityAsset, dup, 1, "", domain.AssetUpdated{Serial: &edit})

	e.must(e.app.MergeAssets(e.ctx, mid, keep, 1, dup, "same pump", true))
	e.must(e.st.Ingest(e.ctx, oos))
	e.must(e.st.Ingest(e.ctx, upd))

	// Out of service reaches the kept record (safety) and is flagged; the
	// serial edit is kept as a flagged event for someone to redo.
	if got := e.flags(oos); !reflect.DeepEqual(got, []string{"conflict/true"}) {
		t.Fatalf("status flags = %v", got)
	}
	if got := e.flags(upd); !reflect.DeepEqual(got, []string{"conflict/false"}) {
		t.Fatalf("edit flags = %v", got)
	}
	if k := e.getAsset(keep); k.Status != domain.AssetOutOfService || k.Serial != "SN-NYC-0001" {
		t.Fatalf("kept record: status %s, serial %s", k.Status, k.Serial)
	}

	// Back in service on the merged record is refused: it must be done on
	// the kept record, by someone looking at it.
	back := pi.write(e, tech, domain.TypeAssetStatusChanged, domain.EntityAsset, dup, 2, "", domain.AssetStatusChanged{Status: domain.AssetInService})
	e.must(e.st.Ingest(e.ctx, back))
	if got := e.flags(back); !reflect.DeepEqual(got, []string{"conflict/false"}) {
		t.Fatalf("back-in-service flags = %v", got)
	}
}

func TestMasterIDRules(t *testing.T) {
	e := newEnv(t)
	for _, bad := range []string{" M-1", "M-1 ", "M\x001", string(make([]byte, domain.MaxMasterIDLength+1))} {
		_, err := e.app.RegisterAsset(e.ctx, e.super, domain.AssetRegistered{Tag: "T" + fmt.Sprint(len(bad)), LocationID: e.loc, Manufacturer: "F", Model: "M", MasterID: bad})
		var r *store.Rejection
		if !errors.As(err, &r) || r.Flag != domain.FlagInvalid {
			t.Errorf("MasterID %q: err = %v", bad, err)
		}
	}
	// Set and changed by an edit, like any other field.
	id := e.assetWithMasterID("A1", "")
	m := "M-300"
	e.must(e.app.UpdateAsset(e.ctx, e.super, id, 1, domain.AssetUpdated{MasterID: &m}))
	if a := e.getAsset(id); a.MasterID != "M-300" {
		t.Fatalf("MasterID = %q", a.MasterID)
	}
}

func TestMergeKeepingHistorySeparate(t *testing.T) {
	e := newEnv(t)
	mid, tech := e.activeUser("mona", domain.RoleMidTier), e.activeUser("tess", domain.RoleUser)
	a := e.assetWithMasterID("NYC-0001", "M-100")
	b := e.assetWithMasterID("OLD-7", "M-100")
	e.openHeld(mid, tech, "corrective", b)
	e.must(e.app.SetAssetStatus(e.ctx, tech, b, e.getAsset(b).Version, domain.AssetMissing, "not in room"))

	// No: B's history stays with B, but safety still applies.
	e.must(e.app.MergeAssets(e.ctx, mid, a, 1, b, "old import", false))
	if g, _ := domain.AssetGroup(e.ctx, e.st.DB(), a); !reflect.DeepEqual(g, []string{a}) {
		t.Fatalf("group = %v, want only the kept record", g)
	}
	if got := e.getAsset(b); got.MergedInto != a || got.HistoryIntegrated {
		t.Fatalf("merged record: into %q, integrated %v", got.MergedInto, got.HistoryIntegrated)
	}
	if got := e.getAsset(a); got.Status != domain.AssetMissing {
		t.Fatalf("kept record status = %s", got.Status)
	}

	// A is then merged, with its history, into C: B stays separate, under C.
	c := e.assetWithMasterID("BOS-0042", "M-100")
	e.must(e.app.MergeAssets(e.ctx, mid, c, 1, a, "moved to Boston", true))
	if g, _ := domain.AssetGroup(e.ctx, e.st.DB(), c); !reflect.DeepEqual(g, []string{c, a}) {
		t.Fatalf("group = %v", g)
	}
	if got := e.getAsset(b); got.MergedInto != c || got.HistoryIntegrated {
		t.Fatalf("separate record after second merge: into %q, integrated %v", got.MergedInto, got.HistoryIntegrated)
	}

	// D is merged into C keeping its history, after records were merged
	// into D with theirs: those stay with D, and changes to them reach C.
	d := e.assetWithMasterID("D-1", "M-100")
	dd := e.assetWithMasterID("D-2", "M-100")
	e.must(e.app.MergeAssets(e.ctx, mid, d, 1, dd, "same", true))
	e.must(e.app.MergeAssets(e.ctx, mid, c, e.getAsset(c).Version, d, "same", false))
	if got := e.getAsset(dd); got.MergedInto != d {
		t.Fatalf("D-2 moved to %s; it should stay with D-1, kept separate", got.MergedInto)
	}
	e.must(e.app.SetAssetStatus(e.ctx, mid, c, e.getAsset(c).Version, domain.AssetInService, ""))
	pi := e.remote()
	oos := pi.write(e, tech, domain.TypeAssetStatusChanged, domain.EntityAsset, dd, 1, "", domain.AssetStatusChanged{Status: domain.AssetOutOfService, Reason: "cracked"})
	e.must(e.st.Ingest(e.ctx, oos))
	if got := e.getAsset(c); got.Status != domain.AssetOutOfService {
		t.Fatalf("out of service on D-2 should reach BOS-0042 through D-1; status %s", got.Status)
	}

	before := snapshot(t, e.st.DB())
	e.must(e.st.Rebuild(e.ctx))
	if after := snapshot(t, e.st.DB()); !reflect.DeepEqual(before["assets"], after["assets"]) {
		t.Fatalf("assets differ after rebuild:\n%v\n%v", before["assets"], after["assets"])
	}
}

// Merges made before the question existed carry no answer, and showed
// both histories together; they still do.
func TestMergeWithoutAnAnswerIntegrates(t *testing.T) {
	e := newEnv(t)
	mid := e.activeUser("mona", domain.RoleMidTier)
	a, b := e.assetWithMasterID("A", "M-1"), e.assetWithMasterID("B", "M-1")
	old := e.remote().write(e, mid, domain.TypeAssetMerged, domain.EntityAsset, a, 1, "", domain.AssetMerged{MergedAssetID: b, Reason: "v0.3.0"})
	if strings.Contains(string(old.Payload), "integrate") {
		t.Fatalf("payload %s", old.Payload)
	}
	e.must(e.st.Ingest(e.ctx, old))
	if g, _ := domain.AssetGroup(e.ctx, e.st.DB(), a); len(g) != 2 {
		t.Fatalf("group = %v", g)
	}
}

// Work orders record when they were opened; the migration fills it in
// for older ones from the same event, the same way.
func TestWorkOrderOpenedAt(t *testing.T) {
	e := newEnv(t)
	mid, tech := e.activeUser("mona", domain.RoleMidTier), e.activeUser("tess", domain.RoleUser)
	wo := e.openHeld(mid, tech, "corrective", e.asset("A1"))
	var live, backfill string
	e.must(e.st.DB().QueryRow(`SELECT opened_at FROM work_orders WHERE id = ?`, wo).Scan(&live))
	e.must(e.st.DB().QueryRow(`SELECT substr(wall_time, 1, 19) || 'Z' FROM events WHERE entity_id = ? AND type = 'workorder.opened'`, wo).Scan(&backfill))
	if live == "" || live != backfill {
		t.Fatalf("opened_at %q, migration would give %q", live, backfill)
	}
}
