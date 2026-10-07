package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"pi-fleet/internal/store"
)

// TypeAssetMerged merges a duplicate equipment record into the record
// that is kept (the event's entity). Records are duplicates only when
// they have the same MasterID (DESIGN.md §5.5).
const TypeAssetMerged = "asset.merged"

// AssetMerged names the record merged into the kept one. The merged
// record keeps everything recorded against it, untouched (signed work
// orders include the equipment id, so rewriting it would make their
// signatures stale); it points at the kept record from then on, and
// the kept record's pages show both histories.
type AssetMerged struct {
	MergedAssetID string `json:"merged_asset_id"`
	Reason        string `json:"reason"`
}

func init() {
	payloadTypes[TypeAssetMerged] = struct {
		entity string
		new    func() any
	}{EntityAsset, func() any { return &AssetMerged{} }}
}

// MaxMasterIDLength bounds a MasterID.
const MaxMasterIDLength = 64

// CheckMasterID reports whether id is an acceptable MasterID. Empty means
// none. MasterIDs are compared exactly, so leading or trailing spaces and
// control characters are refused rather than quietly making two IDs that
// look the same differ.
func CheckMasterID(id string) error {
	if id == "" {
		return nil
	}
	if strings.TrimSpace(id) != id {
		return invalid("MasterID %q has spaces at the start or end", id)
	}
	if len(id) > MaxMasterIDLength {
		return invalid("MasterID is longer than %d characters", MaxMasterIDLength)
	}
	for _, r := range id {
		if !unicode.IsPrint(r) {
			return invalid("MasterID %q contains a control character", id)
		}
	}
	return nil
}

// mergedRecord refuses changes to a record that was merged away.
func mergedRecord(a Asset) error {
	if a.MergedInto != "" {
		return store.Reject(FlagConflict, "equipment %s was merged into %s; make the change there", a.Tag, a.MergedInto)
	}
	return nil
}

func (ap *applier) assetMerged(p *AssetMerged) error {
	if err := ap.require(RoleMidTier); err != nil {
		return err
	}
	keep, err := ap.targetAsset()
	if err != nil {
		return err
	}
	if blank(p.Reason) {
		return invalid("merging equipment records requires a reason")
	}
	if p.MergedAssetID == keep.ID {
		return invalid("cannot merge a record into itself")
	}
	gone, err := GetAsset(ap.ctx, ap.tx, p.MergedAssetID)
	if errors.Is(err, ErrNotFound) {
		return invalid("asset %s not found", p.MergedAssetID)
	}
	if err != nil {
		return err
	}
	if keep.MergedInto != "" {
		return store.Reject(FlagConflict, "%s was itself merged into %s", keep.Tag, keep.MergedInto)
	}
	if gone.MergedInto != "" {
		return store.Reject(FlagConflict, "%s was already merged into %s", gone.Tag, gone.MergedInto)
	}
	if keep.MasterID == "" || keep.MasterID != gone.MasterID {
		return store.Reject(FlagConflict, "%s and %s don't have the same MasterID (%q, %q); only records with the same MasterID are the same equipment",
			keep.Tag, gone.Tag, keep.MasterID, gone.MasterID)
	}
	// Records merged into the one going away now point at the kept one.
	if err := ap.exec(`UPDATE assets SET merged_into = ?, version = version + 1, last_event_id = ?
		WHERE id = ? OR merged_into = ?`, keep.ID, ap.e.EventID, gone.ID, gone.ID); err != nil {
		return err
	}
	// Safety: if the merged record was out of service or missing, the
	// equipment is, so the kept record takes the more restrictive status.
	if gone.Status != AssetRetired && keep.Status != AssetRetired && assetRestriction[gone.Status] > assetRestriction[keep.Status] {
		return ap.bumpAsset(keep, []string{"status"}, `status = ?`, gone.Status)
	}
	return ap.bumpAsset(keep, nil, "")
}

// AssetGroup returns the ids of an asset and of every record merged into
// it, the asset first.
func AssetGroup(ctx context.Context, q Querier, id string) ([]string, error) {
	rest, err := queryStrings(ctx, q, `SELECT id FROM assets WHERE merged_into = ? ORDER BY tag`, id)
	if err != nil {
		return nil, err
	}
	return append([]string{id}, rest...), nil
}

// AssetBrief is enough of an asset to tell duplicates apart.
type AssetBrief struct {
	ID, Tag, Site, Location, Manufacturer, Model, Serial, Status string
	WorkOrders, Calibrations, Schedules                          int
}

// DuplicateGroup is a MasterID held by more than one unmerged record.
type DuplicateGroup struct {
	MasterID string
	Assets   []AssetBrief
}

// Duplicates returns the MasterIDs held by more than one unmerged record,
// or only masterID's group when it is given.
func Duplicates(ctx context.Context, q Querier, masterID string) ([]DuplicateGroup, error) {
	rows, err := q.QueryContext(ctx, `SELECT a.master_id, a.id, a.tag, s.code, l.name, a.manufacturer, a.model, a.serial, a.status,
			(SELECT count(*) FROM work_orders w WHERE w.asset_id = a.id),
			(SELECT count(*) FROM calibration_records c WHERE c.asset_id = a.id AND c.status = 'valid'),
			(SELECT count(*) FROM pm_schedules p WHERE p.asset_id = a.id AND p.status = 'active')
		FROM assets a JOIN sites s ON s.id = a.site_id JOIN locations l ON l.id = a.location_id
		WHERE a.merged_into = '' AND a.master_id != '' AND (? = '' OR a.master_id = ?)
			AND a.master_id IN (SELECT master_id FROM assets WHERE master_id != '' AND merged_into = ''
				GROUP BY master_id HAVING count(*) > 1)
		ORDER BY a.master_id, a.tag`, masterID, masterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var groups []DuplicateGroup
	for rows.Next() {
		var mid string
		var b AssetBrief
		if err := rows.Scan(&mid, &b.ID, &b.Tag, &b.Site, &b.Location, &b.Manufacturer, &b.Model, &b.Serial, &b.Status,
			&b.WorkOrders, &b.Calibrations, &b.Schedules); err != nil {
			return nil, err
		}
		if len(groups) == 0 || groups[len(groups)-1].MasterID != mid {
			groups = append(groups, DuplicateGroup{MasterID: mid})
		}
		g := &groups[len(groups)-1]
		g.Assets = append(g.Assets, b)
	}
	return groups, rows.Err()
}

// mergedStatusChanged handles a status change made to a record that was
// merged away, typically on a Pi that hadn't synced since. The kept
// record is the same equipment, so a more restrictive status (out of
// service, missing) is applied to it; anything else is refused. Either
// way the event is flagged for review.
func (ap *applier) mergedStatusChanged(gone Asset, p *AssetStatusChanged) error {
	keep, err := GetAsset(ap.ctx, ap.tx, gone.MergedInto)
	if err != nil {
		return err
	}
	if p.Status == AssetRetired || keep.Status == AssetRetired || assetRestriction[p.Status] <= assetRestriction[keep.Status] {
		return store.Reject(FlagConflict, "equipment %s was merged into %s; make the change there", gone.Tag, keep.Tag)
	}
	if err := store.Flag(ap.ctx, ap.tx, ap.e.EventID, FlagConflict,
		fmt.Sprintf("%s was merged into %s; applied %s to %s for safety", gone.Tag, keep.Tag, p.Status, keep.Tag), true); err != nil {
		return err
	}
	return ap.bumpAsset(keep, []string{"status"}, `status = ?`, p.Status)
}
