package web

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"pi-fleet/internal/domain"
	"pi-fleet/internal/fleetsync"
)

// page is what every template receives.
type page struct {
	Title      string
	User       domain.User
	CSRF       string
	Role       string // central or node
	Flash      string
	Sync       *SyncInfo
	CanSync    bool   // the Sync now button
	Clock      string // verified or unverified
	PHIWarning bool
	Notices    []string
	Insecure   bool
	Trial      bool
	Large      bool   // larger text and buttons (the default; compact is chosen per browser)
	Here       string // this page's path and query, to come back to
	Path       string // the page's URL path, to mark the current menu item
	FindQ      string // what was searched for, kept in the search box
	Data       any
	// BackHref and BackLabel: a Back link at the top left, to the screen
	// this one was opened from (pages whose data has a BackLink).
	BackHref, BackLabel string
}

// backLinker is page data that has a Back link.
type backLinker interface{ BackLink() (href, label string) }

func (p page) IsMid() bool   { return roleRank[p.User.Role] >= roleRank[domain.RoleMidTier] }
func (p page) IsSuper() bool { return p.User.Role == domain.RoleSuperUser }

// navItem is one page in the menu; navGroup is a top-level tab and the
// pages under it, shown on a second row when it is the open group.
type navItem struct {
	Href, Label string
	Current     bool
}

type navGroup struct {
	Label, Href string
	Current     bool
	Items       []navItem
}

// Nav is the menu for this person and this Pi: four groups (Today, Work,
// Equipment, Admin), the open one marked, with its pages and the current
// one (the longest matching address, so Register equipment beats the
// equipment list).
func (p page) Nav() []navGroup {
	central := p.Role == "central"
	work := []navItem{{Href: "/work-orders", Label: "Work orders"}}
	if central {
		work = append(work, navItem{Href: "/requests", Label: "Reported problems"})
	}
	work = append(work, navItem{Href: "/schedules", Label: "Schedules"}, navItem{Href: "/people", Label: "People"})
	if p.IsMid() {
		work = append(work, navItem{Href: "/review", Label: "Review"})
	}
	equipment := []navItem{{Href: "/assets", Label: "All equipment"}, {Href: "/assets/new", Label: "Register equipment"}}
	if central {
		equipment = append(equipment, navItem{Href: "/models", Label: "Models"})
	}
	equipment = append(equipment, navItem{Href: "/procedures", Label: "Checklists"}, navItem{Href: "/inventory", Label: "Parts and stock"})
	groups := []navGroup{
		{Label: "Today", Href: "/", Items: []navItem{{Href: "/", Label: "My work"}, {Href: "/today/open", Label: "Open work orders"}}},
		{Label: "Work", Items: work},
		{Label: "Equipment", Items: equipment},
	}
	if p.IsSuper() {
		admin := []navItem{{Href: "/admin/users", Label: "User accounts"}, {Href: "/admin/sites", Label: "Sites and locations"}}
		if central {
			admin = append(admin, navItem{Href: "/admin/nodes", Label: "Pis"}, navItem{Href: "/admin/kiosks", Label: "Kiosks"})
		}
		groups = append(groups, navGroup{Label: "Admin", Items: admin})
	}
	for gi := range groups {
		g := &groups[gi]
		if g.Href == "" {
			g.Href = g.Items[0].Href
		}
		best := -1
		for i, it := range g.Items {
			if p.In(it.Href) && (best < 0 || len(it.Href) > len(g.Items[best].Href)) {
				best = i
			}
		}
		if best >= 0 {
			g.Items[best].Current, g.Current = true, true
		}
		if g.Href == "/" && p.Path == "/" {
			g.Current = true
		}
	}
	return groups
}

// SubNav is the open group's pages, or nil.
func (p page) SubNav() *navGroup {
	for _, g := range p.Nav() {
		if g.Current && len(g.Items) > 0 {
			return &g
		}
	}
	return nil
}

// Here reports whether the page is in the menu section at prefix.
func (p page) In(prefix string) bool {
	if prefix == "/" {
		return p.Path == "/"
	}
	return p.Path == prefix || strings.HasPrefix(p.Path, prefix+"/")
}

var funcs = template.FuncMap{
	"when": func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		return t.UTC().Format("2006-01-02 15:04 UTC")
	},
	"whenLocal": func(t time.Time, tz string) string {
		if t.IsZero() {
			return ""
		}
		loc, err := time.LoadLocation(tz)
		if err != nil {
			return ""
		}
		return t.In(loc).Format("2006-01-02 15:04 MST")
	},
	"label": func(s string) string { return strings.ReplaceAll(s, "_", " ") },
	// priorityLabel is what a priority is called: urgent work is "stat".
	"priorityLabel": func(p string) string {
		return map[string]string{"urgent": "stat", "high": "high", "normal": "routine", "low": "low"}[p]
	},
	"kindLabel": func(k string) string { return kindLabels[k] },
	"inputless": domain.Inputless,
	"lines":     func(s string) []string { return strings.Split(s, "\n") },
	// qrpop is what the QR label pop-up (layout.html) needs.
	"qrpop": func(id, code, text, base string) map[string]string {
		m := map[string]string{"ID": id, "Code": code, "Text": text, "URL": reportURL(base, code)}
		if phonesMayFail(base) {
			m["Warn"] = "yes"
		}
		return m
	},
	"urlquery":    urlQuery,
	"meaningText": func(m string) string { return domain.MeaningText[m] },
	"add":         func(a, b int) int { return a + b },
	"sub":         func(a, b int) int { return a - b },
	// stepName is the text of the step with this id.
	"stepName": func(steps []domain.Step, id string) string {
		for _, s := range steps {
			if s.ID == id {
				return s.Text
			}
		}
		return id
	},
	"dict2":  func(a, b any) struct{ A, B any } { return struct{ A, B any }{a, b} },
	"dict3":  func(a, b, c, d any) struct{ A, B, C, D any } { return struct{ A, B, C, D any }{a, b, c, d} },
	"slice3": func(a ...string) []string { return a },
	"hhmm":   hhmm,
	"kib": func(n int64) string {
		if n < 1<<20 {
			return fmt.Sprintf("%.0f KiB", float64(n)/1024)
		}
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	},
	"files": func(list []domain.Attachment, csrf string, can, mid bool, action string) map[string]any {
		return map[string]any{"List": list, "CSRF": csrf, "Can": can, "Mid": mid, "Action": action}
	},
	"seq": func(n int) []int {
		out := make([]int, n)
		for i := range out {
			out[i] = i
		}
		return out
	},
}

func (s *Server) parseTemplates() error {
	s.pages = map[string]*template.Template{}
	names, err := templateNames()
	if err != nil {
		return err
	}
	for _, name := range names {
		t, err := template.New("").Funcs(funcs).ParseFS(assets, "templates/layout.html", "templates/"+name)
		if err != nil {
			return fmt.Errorf("web: template %s: %w", name, err)
		}
		s.pages[strings.TrimSuffix(name, ".html")] = t
	}
	return nil
}

func templateNames() ([]string, error) {
	entries, err := assets.ReadDir("templates")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.Name() != "layout.html" {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// render writes a page. sess may be nil (login page).
func (s *Server) render(w http.ResponseWriter, r *http.Request, sess *session, name, title string, data any) error {
	p := page{Title: title, Role: s.Role, Data: data, PHIWarning: true, Insecure: s.InsecureLAN, Trial: s.Trial, Path: r.URL.Path,
		Large: largeDisplay(r), Here: r.URL.RequestURI()}
	if p.Path == "/search" {
		p.FindQ = r.URL.Query().Get("q")
	}
	if strings.HasPrefix(p.Path, "/fleet/assets") {
		p.Path = "/assets" // the whole-fleet search is part of Equipment
	}
	if sess != nil {
		p.User, p.CSRF, p.Flash = sess.User, sess.CSRF, s.takeFlash(sess)
		p.Clock = string(s.App.ClockState())
		if s.Sync != nil {
			info := s.Sync(r.Context())
			p.Sync = &info
		}
		p.CanSync = s.SyncNow != nil
		if s.Notices != nil && p.IsMid() {
			p.Notices = s.Notices(r.Context())
		}
		if s.SiteCert != nil && p.IsSuper() {
			if info, ok := s.SiteCert.Site(); ok {
				switch days := int(info.NotAfter.Sub(s.now()).Hours() / 24); {
				case days < 0:
					p.Notices = append(p.Notices, "The hospital's certificate expired on "+info.NotAfter.Format("2 January 2006")+
						": browsers and phones warn. Install a renewed one on Admin › Pis.")
				case days < 30:
					p.Notices = append(p.Notices, fmt.Sprintf("The hospital's certificate expires in %d days, on %s. Ask IT for a renewed one and install it on Admin › Pis.",
						days, info.NotAfter.Format("2 January 2006")))
				}
			}
		}
	}
	if b, ok := data.(backLinker); ok {
		p.BackHref, p.BackLabel = b.BackLink()
	}
	t, ok := s.pages[name]
	if !ok {
		return fmt.Errorf("web: no template %q", name)
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", p); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, err := buf.WriteTo(w)
	return err
}

func urlQuery(s string) string { return url.QueryEscape(s) }

// NodeSyncInfo builds SyncInfo for an employee Pi from its store.
func NodeSyncInfo(q domain.Querier, chainID string, cfg func(context.Context, string) string) func(context.Context) SyncInfo {
	return func(ctx context.Context) SyncInfo {
		var info SyncInfo
		info.LastSync, _ = time.Parse(time.RFC3339, cfg(ctx, fleetsync.ConfigLastSync))
		var head int64
		q.QueryRowContext(ctx, `SELECT coalesce(max(seq), 0) FROM events WHERE chain_id = ?`, chainID).Scan(&head)
		var a int64
		fmt.Sscan(cfg(ctx, fleetsync.ConfigAckedSeq), &a)
		info.Unsynced = head - a
		info.UpdateNeeded = cfg(ctx, fleetsync.ConfigUpdateNeeded)
		if due := strings.Fields(cfg(ctx, fleetsync.ConfigUpdateDue)); len(due) == 2 {
			info.UpdateDue = due[0]
			info.UpdateBy, _ = time.Parse(time.RFC3339, due[1])
		}
		return info
	}
}
