package web

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"strings"
	"time"

	"pi-fleet/internal/domain"
)

// One search box for everything: equipment, work orders and checklists.
// A search that names exactly one thing (a scanned or typed asset tag,
// MasterID, serial number or work order number) goes straight to it.

type searchData struct {
	Q          string
	Assets     []assetRow
	WorkOrders []woRow
	Checklists []domain.ChecklistMatch
	More       bool // some list was cut short
	Fleet      bool // an employee Pi that can search the whole fleet
}

const searchLimit = 50

func (s *Server) search(w http.ResponseWriter, r *http.Request, sess *session) error {
	ctx, q := r.Context(), s.App.Store.DB()
	d := searchData{Q: strings.TrimSpace(r.URL.Query().Get("q")), Fleet: s.Fleet != nil}
	if d.Q == "" {
		return s.render(w, r, sess, "search", "Search", d)
	}
	if to := exactMatch(ctx, q, d.Q); to != "" {
		http.Redirect(w, r, to, http.StatusSeeOther)
		return nil
	}
	var err error
	if d.Assets, err = listAssetsAt(ctx, q, d.Q, ""); err != nil {
		return err
	}
	if d.WorkOrders, err = searchWorkOrders(ctx, q, d.Q); err != nil {
		return err
	}
	if d.Checklists, err = searchChecklists(ctx, q, d.Q); err != nil {
		return err
	}
	if len(d.Assets) > searchLimit {
		d.Assets, d.More = d.Assets[:searchLimit], true
	}
	if len(d.WorkOrders) > searchLimit {
		d.WorkOrders, d.More = d.WorkOrders[:searchLimit], true
	}
	return s.render(w, r, sess, "search", "Search", d)
}

// exactMatch is the page of the one record whose tag, MasterID, serial
// number or work order number is exactly text, or "".
func exactMatch(ctx context.Context, q domain.Querier, text string) string {
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
	return ""
}

func searchWorkOrders(ctx context.Context, q domain.Querier, text string) ([]woRow, error) {
	like := "%" + text + "%"
	rows, err := q.QueryContext(ctx, `SELECT w.id, w.number, w.type, w.title, w.status, w.priority, w.due_at, a.tag,
			coalesce((SELECT legal_name FROM users u WHERE u.id = w.assigned_to), ''), w.opened_at
		FROM work_orders w JOIN assets a ON a.id = w.asset_id
		WHERE w.number LIKE ?1 OR w.title LIKE ?1 OR w.problem LIKE ?1 OR a.tag LIKE ?1 OR a.master_id LIKE ?1
		ORDER BY w.status IN ('closed', 'cancelled'), w.opened_at DESC LIMIT ?2`, like, searchLimit+1)
	return scanAll(rows, err, func(r *sql.Rows) (woRow, error) {
		var w woRow
		return w, r.Scan(&w.ID, &w.Number, &w.Type, &w.Title, &w.Status, &w.Priority, &w.DueAt, &w.Asset, &w.AssignedTo, &w.OpenedAt)
	})
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
