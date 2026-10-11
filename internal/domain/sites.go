package domain

import (
	"database/sql"
	"errors"
	"strings"
)

// Sites can sit inside other sites (a satellite clinic inside its
// hospital). Each keeps its path of codes from the top ("MAIN › NORTH")
// and its lineage of ids ("/main/north/"), so equipment at a satellite is
// found under every site above it.

const (
	TypeSiteMoved = "site.moved"

	// PathSep separates the levels of a site's path, and a location's
	// name from its site's path.
	PathSep = " › "
)

// SiteMoved puts a site inside another, or at the top with no parent.
// Everything inside it moves with it.
type SiteMoved struct {
	ParentID string `json:"parent_id,omitempty"`
}

// InSite is an SQL condition, with one argument (a site id), matching
// sites table rows aliased as alias that are that site or inside it.
func InSite(alias string) string {
	return alias + `.lineage LIKE '%/' || ? || '/%'`
}

// sitePlace returns the path and lineage of a site with this code and id
// placed inside parentID ("" for the top).
func (ap *applier) sitePlace(id, code, parentID string) (path, lineage string, err error) {
	if parentID == "" {
		return code, "/" + id + "/", nil
	}
	if parentID == UnallocatedSiteID {
		return "", "", invalid("sites can't be put inside the Unallocated site")
	}
	var pPath, pLineage string
	err = ap.tx.QueryRowContext(ap.ctx, `SELECT path, lineage FROM sites WHERE id = ?`, parentID).Scan(&pPath, &pLineage)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", invalid("site %s not found", parentID)
	}
	if err != nil {
		return "", "", err
	}
	if strings.Contains(pLineage, "/"+id+"/") {
		return "", "", invalid("a site can't be put inside itself or inside one of its own satellites")
	}
	return pPath + PathSep + code, pLineage + id + "/", nil
}

func (ap *applier) siteMoved(p *SiteMoved) error {
	if err := ap.require(RoleSuperUser); err != nil {
		return err
	}
	id := ap.e.EntityID
	if id == UnallocatedSiteID {
		return invalid("the Unallocated site can't be moved")
	}
	var code, oldPath, oldLineage string
	var oldParent sql.NullString
	err := ap.tx.QueryRowContext(ap.ctx, `SELECT code, path, lineage, parent_id FROM sites WHERE id = ?`, id).Scan(&code, &oldPath, &oldLineage, &oldParent)
	if errors.Is(err, sql.ErrNoRows) {
		return invalid("site %s not found", id)
	}
	if err != nil {
		return err
	}
	if oldParent.String == p.ParentID {
		return invalid("the site is already there")
	}
	path, lineage, err := ap.sitePlace(id, code, p.ParentID)
	if err != nil {
		return err
	}
	var parent any
	if p.ParentID != "" {
		parent = p.ParentID
	}
	if err := ap.exec(`UPDATE sites SET parent_id = ?, version = version + 1, last_event_id = ? WHERE id = ?`, parent, ap.e.EventID, id); err != nil {
		return err
	}
	// The site and everything inside it get the new beginning of their
	// path and lineage (substr counts characters, so › is one).
	return ap.exec(`UPDATE sites SET path = ? || substr(path, length(?) + 1), lineage = ? || substr(lineage, length(?) + 1)
		WHERE lineage LIKE ? || '%'`, path, oldPath, lineage, oldLineage, oldLineage)
}
