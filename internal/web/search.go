package web

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"pi-fleet/internal/domain"
)

// One search box, on every page, for everything the person searching may
// open: each kind of record is searched only for roles that can open its
// page, and higher roles see everything lower ones do (super user ⊇
// mid-tier ⊇ user). A search that names exactly one thing (a scanned or
// typed asset tag, MasterID, serial number, work order or reported
// problem number) goes straight to it.

// hit is one search result: a link, what it is, and a line about it.
type hit struct{ Href, Name, Detail, Note string }

type hitGroup struct {
	Title string
	Hits  []hit
	More  bool // cut short at searchLimit
}

type searchData struct {
	Q        string
	Groups   []hitGroup
	Searched []string // the kinds of record searched, for this role
	Fleet    bool     // an employee Pi that can search the whole fleet
}

const searchLimit = 50

// searcher finds one kind of record. like is the text wrapped in % for
// LIKE; find returns at most searchLimit+1 hits.
type searcher struct {
	title   string
	minRole string
	central bool // only the master Pi has these
	find    func(ctx context.Context, q domain.Querier, text, like string, sess *session) ([]hit, error)
}

// searchers, in the order results are shown.
var searchers = []searcher{
	{title: "Equipment", minRole: domain.RoleUser, find: findEquipment},
	{title: "Work orders", minRole: domain.RoleUser, find: findWorkOrders},
	{title: "Reported problems", minRole: domain.RoleUser, central: true, find: findRequests},
	{title: "Models", minRole: domain.RoleUser, central: true, find: findModelHits},
	{title: "Checklists", minRole: domain.RoleUser, find: findChecklists},
	{title: "Checklists waiting for approval", minRole: domain.RoleMidTier, find: findProposals},
	{title: "Schedules", minRole: domain.RoleUser, find: findSchedules},
	{title: "Parts", minRole: domain.RoleUser, find: findParts},
	{title: "Locations", minRole: domain.RoleUser, find: findLocations},
	{title: "Sites", minRole: domain.RoleSuperUser, find: findSites},
	{title: "People", minRole: domain.RoleUser, find: findPeople},
	{title: "Pis", minRole: domain.RoleSuperUser, central: true, find: findPis},
	{title: "Kiosks", minRole: domain.RoleSuperUser, central: true, find: findKiosks},
}

// mySearchers are the searchers for this person on this Pi.
func (s *Server) mySearchers(sess *session) []searcher {
	var out []searcher
	for _, f := range searchers {
		if roleRank[sess.User.Role] >= roleRank[f.minRole] && (!f.central || s.Role == "central") {
			out = append(out, f)
		}
	}
	return out
}

func (s *Server) search(w http.ResponseWriter, r *http.Request, sess *session) error {
	ctx, q := r.Context(), s.App.Store.DB()
	d := searchData{Q: strings.TrimSpace(r.URL.Query().Get("q")), Fleet: s.Fleet != nil}
	mine := s.mySearchers(sess)
	for _, f := range mine {
		d.Searched = append(d.Searched, f.title)
	}
	if d.Q == "" {
		return s.render(w, r, sess, "search", "Search", d)
	}
	if to := exactMatch(ctx, q, d.Q, s.Role == "central"); to != "" {
		http.Redirect(w, r, to, http.StatusSeeOther)
		return nil
	}
	like := "%" + d.Q + "%"
	for _, f := range mine {
		hits, err := f.find(ctx, q, d.Q, like, sess)
		if err != nil {
			return fmt.Errorf("search %s: %w", f.title, err)
		}
		if len(hits) == 0 {
			continue
		}
		g := hitGroup{Title: f.title, Hits: hits}
		if len(g.Hits) > searchLimit {
			g.Hits, g.More = g.Hits[:searchLimit], true
		}
		d.Groups = append(d.Groups, g)
	}
	return s.render(w, r, sess, "search", "Search", d)
}

// scanHits runs a query whose rows are href, name, detail, note.
func scanHits(ctx context.Context, q domain.Querier, query string, args ...any) ([]hit, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	return scanAll(rows, err, func(r *sql.Rows) (hit, error) {
		var h hit
		return h, r.Scan(&h.Href, &h.Name, &h.Detail, &h.Note)
	})
}

func findEquipment(ctx context.Context, q domain.Querier, text, _ string, _ *session) ([]hit, error) {
	assets, err := listAssetsAt(ctx, q, text, "")
	var out []hit
	for _, a := range assets {
		h := hit{Href: "/assets/" + a.ID, Name: a.Tag, Detail: strings.TrimSpace(a.Manufacturer + " " + a.Model), Note: a.Location + ", " + a.Site}
		if a.MasterID != "" {
			h.Name += " · " + a.MasterID
		}
		if a.Serial != "" {
			h.Detail += " · serial " + a.Serial
		}
		if a.MergedIntoTag != "" {
			h.Note += " · merged into " + a.MergedIntoTag
		} else {
			h.Note += " · " + strings.ReplaceAll(a.Status, "_", " ")
		}
		out = append(out, h)
	}
	return out, err
}

func findWorkOrders(ctx context.Context, q domain.Querier, _, like string, _ *session) ([]hit, error) {
	return scanHits(ctx, q, `SELECT '/work-orders/' || w.id, w.number || ' · ' || w.title,
			a.tag || ' · ' || replace(w.type, '_', ' ') || ' · ' || replace(w.status, '_', ' '),
			coalesce('assigned to ' || (SELECT legal_name FROM users u WHERE u.id = w.assigned_to), 'not assigned')
				|| CASE WHEN w.due_at != '' THEN ' · due ' || substr(w.due_at, 1, 10) ELSE '' END
		FROM work_orders w JOIN assets a ON a.id = w.asset_id
		WHERE w.number LIKE ?1 OR w.title LIKE ?1 OR w.problem LIKE ?1 OR a.tag LIKE ?1 OR a.master_id LIKE ?1
		ORDER BY w.status IN ('closed', 'cancelled'), w.opened_at DESC LIMIT ?2`, like, searchLimit+1)
}

func findRequests(ctx context.Context, q domain.Querier, _, like string, _ *session) ([]hit, error) {
	return scanHits(ctx, q, `SELECT CASE WHEN r.status = 'converted' AND r.work_order_id != '' THEN '/work-orders/' || r.work_order_id
				ELSE '/requests#req-' || r.id END,
			r.number || ' · ' || r.category, a.tag || ' · ' || r.description,
			r.requester_name || ', ' || r.requester_department || ' · ' ||
				CASE r.status WHEN 'new' THEN 'waiting' WHEN 'converted' THEN 'work order opened' ELSE 'closed' END
		FROM service_requests r JOIN assets a ON a.id = r.asset_id
		WHERE r.number LIKE ?1 OR r.description LIKE ?1 OR r.category LIKE ?1 OR r.requester_name LIKE ?1
			OR r.requester_department LIKE ?1 OR a.tag LIKE ?1 OR a.master_id LIKE ?1
		ORDER BY r.status != 'new', r.submitted_at DESC LIMIT ?2`, like, searchLimit+1)
}

func findModelHits(ctx context.Context, q domain.Querier, text, _ string, _ *session) ([]hit, error) {
	models, err := findModels(ctx, q, text)
	var out []hit
	for _, m := range models {
		out = append(out, hit{Href: "/models/" + m.ID, Name: m.Manufacturer + " " + m.Model, Detail: m.Kind,
			Note: fmt.Sprintf("%d schedule%s", len(m.Schedules), plural(len(m.Schedules)))})
	}
	return out, err
}

func findChecklists(ctx context.Context, q domain.Querier, text, _ string, _ *session) ([]hit, error) {
	all, err := searchChecklists(ctx, q, text)
	var out []hit
	for _, c := range all {
		var where []string
		for _, m := range c.AppliesTo {
			where = append(where, strings.TrimSpace(m.Manufacturer+" "+m.Model))
		}
		h := hit{Href: "/procedures/" + c.ID, Name: c.Name, Detail: fmt.Sprintf("version %d", c.Version), Note: "for any equipment"}
		if c.Category != "" {
			h.Detail += " · " + c.Category
		}
		if len(where) > 0 {
			h.Note = "for " + strings.Join(where, "; ")
		}
		out = append(out, h)
	}
	return out, err
}

func findProposals(ctx context.Context, q domain.Querier, _, like string, _ *session) ([]hit, error) {
	return scanHits(ctx, q, `SELECT '/checklist-proposals/' || p.id, p.name, 'proposed by ' || coalesce(u.legal_name, p.proposed_by),
			'waiting since ' || substr(p.proposed_at, 1, 10)
		FROM procedure_proposals p LEFT JOIN users u ON u.id = p.proposed_by
		WHERE p.status = 'pending' AND (p.name LIKE ?1 OR p.category LIKE ?1 OR p.applies_to LIKE ?1 OR p.description LIKE ?1)
		ORDER BY p.proposed_at LIMIT ?2`, like, searchLimit+1)
}

func findSchedules(ctx context.Context, q domain.Querier, _, like string, _ *session) ([]hit, error) {
	return scanHits(ctx, q, `SELECT '/assets/' || a.id || '#schedules', p.title,
			a.tag || ' · ' || p.wo_type || ' every ' || p.interval_days || ' days',
			CASE WHEN p.status = 'active' THEN 'next due ' || p.next_due ELSE 'ended' END
		FROM pm_schedules p JOIN assets a ON a.id = p.asset_id
		WHERE p.title LIKE ?1 OR p.procedure LIKE ?1 OR a.tag LIKE ?1 OR a.master_id LIKE ?1
		ORDER BY p.status != 'active', p.next_due LIMIT ?2`, like, searchLimit+1)
}

func findParts(ctx context.Context, q domain.Querier, _, like string, _ *session) ([]hit, error) {
	return scanHits(ctx, q, `SELECT '/inventory', p.part_no || ' · ' || p.description, 'unit: ' || p.unit,
			coalesce((SELECT sum(qty) FROM stock_levels l WHERE l.part_id = p.id), 0) || ' in stock'
		FROM parts p WHERE p.part_no LIKE ?1 OR p.description LIKE ?1 ORDER BY p.part_no LIMIT ?2`, like, searchLimit+1)
}

// findLocations links a super user to the location on Sites and
// locations, and anyone else to the equipment there.
func findLocations(ctx context.Context, q domain.Querier, _, like string, sess *session) ([]hit, error) {
	hits, err := scanHits(ctx, q, `SELECT '/admin/sites#loc-' || l.id, l.name, s.path || ' · ' || s.name || ' · ' || l.kind,
			(SELECT count(*) FROM assets a WHERE a.location_id = l.id AND a.merged_into = '') || ' pieces of equipment'
				|| CASE WHEN l.status = 'pending' THEN ' · awaiting approval' ELSE '' END
		FROM locations l JOIN sites s ON s.id = l.site_id
		WHERE l.status != 'rejected' AND l.id != ?3 AND (l.name LIKE ?1 OR l.details LIKE ?1)
		ORDER BY s.path, l.name LIMIT ?2`, like, searchLimit+1, domain.UnallocatedLocationID)
	if sess.User.Role != domain.RoleSuperUser {
		for i := range hits {
			hits[i].Href = "/assets?q=" + url.QueryEscape(hits[i].Name)
		}
	}
	return hits, err
}

func findSites(ctx context.Context, q domain.Querier, _, like string, _ *session) ([]hit, error) {
	return scanHits(ctx, q, `SELECT '/admin/sites#site-' || s.id, s.path || ' · ' || s.name, s.timezone,
			(SELECT count(*) FROM locations l WHERE l.site_id = s.id AND l.status != 'rejected') || ' locations'
		FROM sites s WHERE s.id != ?3 AND (s.code LIKE ?1 OR s.name LIKE ?1) ORDER BY s.path LIMIT ?2`,
		like, searchLimit+1, domain.UnallocatedSiteID)
}

// findPeople finds active people by name for everyone, linking to their
// People page; super users also find them by username or email, and
// find disabled accounts, on User accounts.
func findPeople(ctx context.Context, q domain.Querier, _, like string, sess *session) ([]hit, error) {
	if sess.User.Role == domain.RoleSuperUser {
		return scanHits(ctx, q, `SELECT '/admin/users#user-' || id, legal_name, username || ' · ' || email,
				replace(role, '_', ' ') || ' · ' || replace(status, '_', ' ')
			FROM users WHERE username LIKE ?1 OR legal_name LIKE ?1 OR email LIKE ?1 ORDER BY legal_name LIMIT ?2`, like, searchLimit+1)
	}
	return scanHits(ctx, q, `SELECT '/people/' || id, legal_name, replace(role, '_', ' '),
			(SELECT count(*) FROM work_orders w WHERE w.assigned_to = u.id AND w.status NOT IN ('closed', 'cancelled')) || ' open work orders'
		FROM users u WHERE status = 'active' AND legal_name LIKE ?1 ORDER BY legal_name LIMIT ?2`, like, searchLimit+1)
}

func findPis(ctx context.Context, q domain.Querier, _, like string, _ *session) ([]hit, error) {
	return scanHits(ctx, q, `SELECT '/admin/nodes#pi-' || n.id, 'Pi ' || substr(n.id, 1, 8),
			CASE WHEN n.mode = 'kiosk' THEN 'kiosk ' || coalesce(k.name, '') ELSE coalesce(u.legal_name || ' (' || u.username || ')', '') END,
			replace(n.status, '_', ' ')
		FROM nodes n LEFT JOIN users u ON u.id = n.bound_user_id LEFT JOIN kiosks k ON k.id = n.kiosk_id
		WHERE n.id LIKE ?1 OR u.username LIKE ?1 OR u.legal_name LIKE ?1 OR k.name LIKE ?1
		ORDER BY n.status != 'active', n.activated_at DESC LIMIT ?2`, like, searchLimit+1)
}

func findKiosks(ctx context.Context, q domain.Querier, _, like string, _ *session) ([]hit, error) {
	return scanHits(ctx, q, `SELECT '/admin/kiosks#kiosk-' || k.id, k.name, s.path || ' · ' || s.name,
			(SELECT count(*) FROM kiosk_members m WHERE m.kiosk_id = k.id AND m.removed_hlc IS NULL) || ' members'
		FROM kiosks k JOIN sites s ON s.id = k.site_id WHERE k.name LIKE ?1 OR s.name LIKE ?1 ORDER BY k.name LIMIT ?2`, like, searchLimit+1)
}

// exactMatch is the page of the one record whose tag, MasterID, serial
// number or work order number is exactly text, or "".
func exactMatch(ctx context.Context, q domain.Querier, text string, central bool) string {
	var ids []string
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT coalesce(nullif(merged_into, ''), id) FROM assets
		WHERE tag = ?1 COLLATE NOCASE OR master_id = ?1 COLLATE NOCASE OR (serial != '' AND serial = ?1 COLLATE NOCASE) LIMIT 2`, text)
	if ids, err = scanAll(rows, err, func(r *sql.Rows) (string, error) {
		var id string
		return id, r.Scan(&id)
	}); err == nil && len(ids) == 1 {
		return "/assets/" + ids[0]
	}
	var wo string
	if q.QueryRowContext(ctx, `SELECT id FROM work_orders WHERE number = ? COLLATE NOCASE`, text).Scan(&wo) == nil {
		return "/work-orders/" + wo
	}
	var req string
	if central && q.QueryRowContext(ctx, `SELECT CASE WHEN status = 'converted' AND work_order_id != '' THEN '/work-orders/' || work_order_id
			ELSE '/requests#req-' || id END FROM service_requests WHERE number = ? COLLATE NOCASE`, text).Scan(&req) == nil {
		return req
	}
	return ""
}

// searchChecklists finds the newest version of each checklist whose
// name, category or equipment matches text.
func searchChecklists(ctx context.Context, q domain.Querier, text string) ([]domain.ChecklistMatch, error) {
	all, err := domain.ChecklistsFor(ctx, q, "", "")
	if err != nil {
		return nil, err
	}
	t := strings.ToLower(text)
	var out []domain.ChecklistMatch
	for _, c := range all {
		hay := strings.ToLower(c.Name + " " + c.Category + " " + c.Reference)
		for _, m := range c.AppliesTo {
			hay += " " + strings.ToLower(m.Manufacturer+" "+m.Model)
		}
		if strings.Contains(hay, t) {
			out = append(out, c)
		}
	}
	return out, nil
}

// assetPick chooses the equipment a form is for, without a list of every
// piece of equipment: search, then choose one of the matches. The form
// itself is shown once one is chosen.
type assetPick struct {
	Base    string // the page, e.g. /work-orders/new
	Anchor  string // where on the page the picker is, e.g. #new
	Q       string
	Results []assetRow
	More    bool
	Chosen  assetRow
}

// ChooseHref is the address that picks the equipment with this id.
func (p assetPick) ChooseHref(id string) string {
	return p.Base + "?asset=" + url.QueryEscape(id) + p.Anchor
}

func pickAsset(r *http.Request, base, anchor string, q domain.Querier) (assetPick, error) {
	ctx := r.Context()
	p := assetPick{Base: base, Anchor: anchor, Q: strings.TrimSpace(r.URL.Query().Get("q"))}
	if id := r.URL.Query().Get("asset"); id != "" {
		var a assetRow
		err := q.QueryRowContext(ctx, `SELECT a.id, a.tag, a.manufacturer, a.model, a.serial, a.status, s.path, l.name
			FROM assets a JOIN sites s ON s.id = a.site_id JOIN locations l ON l.id = a.location_id
			WHERE a.id = ? AND a.merged_into = ''`, id).Scan(&a.ID, &a.Tag, &a.Manufacturer, &a.Model, &a.Serial, &a.Status, &a.Site, &a.Location)
		if err == nil {
			p.Chosen = a
			return p, nil
		}
		if err != sql.ErrNoRows {
			return p, err
		}
	}
	if p.Q == "" {
		return p, nil
	}
	all, err := listAssetsAt(ctx, q, p.Q, "")
	if err != nil {
		return p, err
	}
	for _, a := range all {
		if a.MergedIntoTag == "" && a.Status != domain.AssetRetired {
			p.Results = append(p.Results, a)
		}
	}
	if len(p.Results) > searchLimit {
		p.Results, p.More = p.Results[:searchLimit], true
	}
	return p, nil
}

// --- My work (the home page) ---

// jobCard is one of my work orders, as a card on the home page.
type jobCard struct {
	ID, Number, Type, Title, Status, Priority, DueAt string
	AssetID, Tag, Equipment, Place                   string
	Who                                              string // who it is assigned to (on an equipment page)
	// When says when it is due, in words; Late marks it overdue.
	When string
	Late bool
}

func myJobs(ctx context.Context, q domain.Querier, userID string, now time.Time) ([]jobCard, error) {
	rows, err := q.QueryContext(ctx, `SELECT w.id, w.number, w.type, w.title, w.status, w.priority, w.due_at,
			a.id, a.tag, trim(a.manufacturer || ' ' || a.model), l.name || ', ' || s.name
		FROM work_orders w JOIN assets a ON a.id = w.asset_id JOIN locations l ON l.id = a.location_id JOIN sites s ON s.id = a.site_id
		WHERE w.assigned_to = ? AND w.status NOT IN ('closed', 'cancelled')
		ORDER BY w.status IN ('completed', 'reviewed'), w.due_at = '', substr(w.due_at, 1, 10),
			CASE w.priority WHEN 'urgent' THEN 0 WHEN 'high' THEN 1 WHEN 'normal' THEN 2 ELSE 3 END, w.number LIMIT 200`, userID)
	day := today(now)
	return scanAll(rows, err, func(r *sql.Rows) (jobCard, error) {
		var j jobCard
		err := r.Scan(&j.ID, &j.Number, &j.Type, &j.Title, &j.Status, &j.Priority, &j.DueAt, &j.AssetID, &j.Tag, &j.Equipment, &j.Place)
		j.When, j.Late = dueWords(j.DueAt, day)
		return j, err
	})
}

// dueWords says when something is due, in words, and whether it is late.
func dueWords(due, day string) (string, bool) {
	if len(due) < 10 {
		return "No due date", false
	}
	d := due[:10]
	t, err := time.Parse("2006-01-02", d)
	if err != nil {
		return "Due " + d, false
	}
	switch {
	case d < day:
		return "Overdue since " + t.Format("Monday 2 January"), true
	case d == day:
		return "Due today", false
	}
	if td, err := time.Parse("2006-01-02", day); err == nil && t.Sub(td) == 24*time.Hour {
		return "Due tomorrow", false
	}
	return "Due " + t.Format("Monday 2 January"), false
}
