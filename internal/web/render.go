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
)

// page is what every template receives.
type page struct {
	Title      string
	User       domain.User
	CSRF       string
	Role       string // central or node
	Flash      string
	Sync       *SyncInfo
	Clock      string // verified or unverified
	PHIWarning bool
	Notices    []string
	Data       any
}

func (p page) IsMid() bool   { return roleRank[p.User.Role] >= roleRank[domain.RoleMidTier] }
func (p page) IsSuper() bool { return p.User.Role == domain.RoleSuperUser }

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
	"label":       func(s string) string { return strings.ReplaceAll(s, "_", " ") },
	"urlquery":    urlQuery,
	"meaningText": func(m string) string { return domain.MeaningText[m] },
	"add":         func(a, b int) int { return a + b },
	"dict2":       func(a, b any) struct{ A, B any } { return struct{ A, B any }{a, b} },
	"dict3":       func(a, b, c, d any) struct{ A, B, C, D any } { return struct{ A, B, C, D any }{a, b, c, d} },
	"slice3":      func(a ...string) []string { return a },
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
	p := page{Title: title, Role: s.Role, Data: data, PHIWarning: true}
	if sess != nil {
		p.User, p.CSRF, p.Flash = sess.User, sess.CSRF, s.takeFlash(sess)
		p.Clock = string(s.App.ClockState())
		if s.Sync != nil {
			info := s.Sync(r.Context())
			p.Sync = &info
		}
		if s.Notices != nil && p.IsMid() {
			p.Notices = s.Notices(r.Context())
		}
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
func NodeSyncInfo(q domain.Querier, chainID string, lastSync, acked func(context.Context) string) func(context.Context) SyncInfo {
	return func(ctx context.Context) SyncInfo {
		var info SyncInfo
		info.LastSync, _ = time.Parse(time.RFC3339, lastSync(ctx))
		var head int64
		q.QueryRowContext(ctx, `SELECT coalesce(max(seq), 0) FROM events WHERE chain_id = ?`, chainID).Scan(&head)
		var a int64
		fmt.Sscan(acked(ctx), &a)
		info.Unsynced = head - a
		return info
	}
}
