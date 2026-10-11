package domain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Locations proposed by employees: while registering equipment, anyone can
// propose a new location and carry on registering equipment there. It waits
// for a super user's review. Rejected, its equipment moves to the
// Unallocated site, where it stays findable until someone moves it.

const (
	TypeLocationProposed = "location.proposed"
	TypeLocationReviewed = "location.reviewed"

	LocationApproved = "approved"
	LocationPending  = "pending"
	LocationRejected = "rejected"

	// The Unallocated site and its one location hold the equipment of
	// rejected locations. They are made when first needed, with fixed ids,
	// so every Pi and every rebuild makes the same ones.
	UnallocatedSiteID     = "unallocated-site"
	UnallocatedLocationID = "unallocated-location"
	UnallocatedSiteCode   = "UNALLOC"
	UnallocatedName       = "Unallocated"
)

// LocationDetails identify a location for someone looking for equipment
// there. All are optional.
type LocationDetails struct {
	Building   string `json:"building,omitempty"`
	Floor      string `json:"floor,omitempty"`
	Room       string `json:"room,omitempty"`
	Department string `json:"department,omitempty"`
	Contact    string `json:"contact,omitempty"` // who to ask there
	Phone      string `json:"phone,omitempty"`
	Directions string `json:"directions,omitempty"`
}

// Empty reports whether no detail is filled in.
func (d LocationDetails) Empty() bool { return d == LocationDetails{} }

func (d LocationDetails) trimmed() LocationDetails {
	t := strings.TrimSpace
	return LocationDetails{t(d.Building), t(d.Floor), t(d.Room), t(d.Department), t(d.Contact), t(d.Phone), t(d.Directions)}
}

// LocationProposed is a location an employee proposes for review.
type LocationProposed struct {
	SiteID  string          `json:"site_id"`
	Name    string          `json:"name"`
	Kind    string          `json:"kind"`
	Details LocationDetails `json:"details"`
}

// LocationReviewed approves or rejects a proposed location. A rejection
// needs a reason, which the proposer sees.
type LocationReviewed struct {
	Approved bool   `json:"approved"`
	Note     string `json:"note,omitempty"`
}

func (ap *applier) locationProposed(p *LocationProposed) error {
	if err := ap.require(RoleUser); err != nil {
		return err
	}
	if err := ap.newEntity("locations"); err != nil {
		return err
	}
	if blank(p.Name) || blank(p.Kind) {
		return invalid("location name and kind are required")
	}
	if p.SiteID == UnallocatedSiteID {
		return invalid("locations can't be added to the Unallocated site")
	}
	if ok, err := ap.exists(`SELECT 1 FROM sites WHERE id = ?`, p.SiteID); err != nil || !ok {
		return orInvalid(err, "site %s not found", p.SiteID)
	}
	if ok, err := ap.exists(`SELECT 1 FROM locations WHERE site_id = ? AND lower(trim(name)) = lower(trim(?)) AND status != ?`,
		p.SiteID, p.Name, LocationRejected); err != nil || ok {
		return orConflict(err, "the site already has a location called %s: choose it from the list", strings.TrimSpace(p.Name))
	}
	details, _ := json.Marshal(p.Details.trimmed())
	return ap.exec(`INSERT INTO locations (id, site_id, parent_id, name, kind, status, details, proposed_by, proposed_at, version, last_event_id)
		VALUES (?, ?, NULL, ?, ?, ?, ?, ?, ?, 1, ?)`,
		ap.e.EntityID, p.SiteID, strings.TrimSpace(p.Name), strings.TrimSpace(p.Kind), LocationPending, string(details),
		ap.e.ActorUserID, ap.wall(), ap.e.EventID)
}

func (ap *applier) locationReviewed(p *LocationReviewed) error {
	if err := ap.require(RoleSuperUser); err != nil {
		return err
	}
	var status string
	err := ap.tx.QueryRowContext(ap.ctx, `SELECT status FROM locations WHERE id = ?`, ap.e.EntityID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return invalid("location %s not found", ap.e.EntityID)
	}
	if err != nil {
		return err
	}
	if status != LocationPending {
		return orConflict(nil, "this location was already %s", status)
	}
	next := LocationApproved
	if !p.Approved {
		if blank(p.Note) {
			return invalid("rejecting a location needs a reason, which the person who proposed it sees")
		}
		next = LocationRejected
		if err := ap.moveToUnallocated(ap.e.EntityID); err != nil {
			return err
		}
	}
	return ap.exec(`UPDATE locations SET status = ?, reviewed_by = ?, review_note = ?, version = version + 1, last_event_id = ? WHERE id = ?`,
		next, ap.e.ActorUserID, strings.TrimSpace(p.Note), ap.e.EventID, ap.e.EntityID)
}

// moveToUnallocated moves the equipment at a rejected location.
func (ap *applier) moveToUnallocated(locationID string) error {
	if err := ap.ensureUnallocated(); err != nil {
		return err
	}
	rows, err := ap.tx.QueryContext(ap.ctx, `SELECT id FROM assets WHERE location_id = ?`, locationID)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		a, err := GetAsset(ap.ctx, ap.tx, id)
		if err != nil {
			return err
		}
		if err := ap.bumpAsset(a, []string{"location"}, `location_id = ?, site_id = ?`, UnallocatedLocationID, UnallocatedSiteID); err != nil {
			return err
		}
	}
	return nil
}

// ensureUnallocated makes the Unallocated site and location if needed.
func (ap *applier) ensureUnallocated() error {
	if err := ap.exec(`INSERT OR IGNORE INTO sites (id, code, name, timezone, path, lineage, version, last_event_id) VALUES (?, ?, ?, 'UTC', ?, ?, 1, ?)`,
		UnallocatedSiteID, UnallocatedSiteCode, UnallocatedName, UnallocatedSiteCode, "/"+UnallocatedSiteID+"/", ap.e.EventID); err != nil {
		return err
	}
	return ap.exec(`INSERT OR IGNORE INTO locations (id, site_id, parent_id, name, kind, status, version, last_event_id)
		VALUES (?, ?, NULL, ?, 'holding', ?, 1, ?)`, UnallocatedLocationID, UnallocatedSiteID, UnallocatedName, LocationApproved, ap.e.EventID)
}

// placeAt returns where equipment registered or moved to a location goes:
// the location's site, or Unallocated if the location was rejected (by a
// super user while the person registering it was offline).
func (ap *applier) placeAt(locationID string) (locID, siteID string, err error) {
	var status string
	err = ap.tx.QueryRowContext(ap.ctx, `SELECT site_id, status FROM locations WHERE id = ?`, locationID).Scan(&siteID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", invalid("location %s not found", locationID)
	}
	if err != nil {
		return "", "", err
	}
	if status == LocationRejected {
		if err := ap.ensureUnallocated(); err != nil {
			return "", "", err
		}
		return UnallocatedLocationID, UnallocatedSiteID, nil
	}
	return locationID, siteID, nil
}

// ProposedLocation is a location waiting for review, with what was
// registered there meanwhile.
type ProposedLocation struct {
	ID, SiteID, Site, Name, Kind string
	Details                      LocationDetails
	ProposedBy                   string // "Legal Name (username)"
	ProposedAt                   time.Time
	Equipment                    []string
}

// PendingLocations lists the locations waiting for review, oldest first.
func PendingLocations(ctx context.Context, q Querier) ([]ProposedLocation, error) {
	rows, err := q.QueryContext(ctx, `SELECT l.id, l.site_id, s.path || ' · ' || s.name, l.name, l.kind, l.details,
			coalesce((SELECT u.legal_name || ' (' || u.username || ')' FROM users u WHERE u.id = l.proposed_by), l.proposed_by), l.proposed_at
		FROM locations l JOIN sites s ON s.id = l.site_id WHERE l.status = ? ORDER BY l.proposed_at, l.id`, LocationPending)
	if err != nil {
		return nil, err
	}
	var out []ProposedLocation
	for rows.Next() {
		var p ProposedLocation
		var details, at string
		if err := rows.Scan(&p.ID, &p.SiteID, &p.Site, &p.Name, &p.Kind, &details, &p.ProposedBy, &at); err != nil {
			rows.Close()
			return nil, err
		}
		json.Unmarshal([]byte(details), &p.Details)
		p.ProposedAt, _ = time.Parse(time.RFC3339, at)
		out = append(out, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		rows, err := q.QueryContext(ctx, `SELECT tag || ' · ' || manufacturer || ' ' || model FROM assets WHERE location_id = ? ORDER BY tag`, out[i].ID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				rows.Close()
				return nil, err
			}
			out[i].Equipment = append(out[i].Equipment, s)
		}
		rows.Close()
	}
	return out, nil
}
