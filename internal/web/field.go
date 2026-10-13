package web

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/url"

	"pi-fleet/internal/domain"
)

// Field mode (static/sw.js and static/field.js): phones and tablets keep
// the pages of someone's work for use without a network, and send what
// was saved there once the master Pi answers again. Only the master Pi
// offers it, since that is what phones and tablets reach.

// fieldCSP is the policy of signed-in pages on the master Pi: as every
// page's, plus pi-fleet's own script files (never inline ones), its
// service worker, and requests back to this site.
const fieldCSP = "default-src 'none'; script-src 'self'; worker-src 'self'; connect-src 'self'; style-src 'self'; img-src 'self'; font-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

// serviceWorker serves sw.js from the top of the site, so it can look
// after every page.
func (s *Server) serviceWorker(w http.ResponseWriter, r *http.Request) {
	b, err := assets.ReadFile("static/sw.js")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/javascript; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Content-Security-Policy", "default-src 'none'; connect-src 'self'")
	w.Write(b)
}

// fieldPing answers, so a page can tell the master Pi is reachable.
func (s *Server) fieldPing(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

type fieldManifest struct {
	Pages  []string `json:"pages"`
	Static []string `json:"static"`
}

// fieldManifest lists the pages to keep for someone's work: My work, and
// for each of their open work orders, its page, its checklists and its
// equipment; and the files every page needs.
func (s *Server) fieldManifest(w http.ResponseWriter, r *http.Request, sess *session) error {
	ctx, q := r.Context(), s.App.Store.DB()
	m := fieldManifest{Pages: []string{"/"}}
	rows, err := listWorkOrders(ctx, q, "mine", sess.User.ID)
	if err != nil {
		return err
	}
	assetSeen := map[string]bool{}
	for _, row := range rows {
		wo, err := domain.GetWorkOrder(ctx, q, row.ID)
		if err != nil {
			return err
		}
		if wo.Status == domain.WOCompleted || wo.Status == domain.WOReviewed {
			continue // waiting for review: nothing left to do on it
		}
		m.Pages = append(m.Pages, "/work-orders/"+wo.ID)
		cs, err := domain.Checklists(ctx, q, wo)
		if err != nil {
			return err
		}
		for _, c := range cs {
			m.Pages = append(m.Pages, "/work-orders/"+wo.ID+"/checklists/"+c.ID)
		}
		if !assetSeen[wo.AssetID] {
			assetSeen[wo.AssetID] = true
			m.Pages = append(m.Pages, "/assets/"+wo.AssetID)
		}
	}
	m.Static = []string{"/static/style.css", "/static/large.css", "/static/field.js", "/static/session.js", "/static/clock.js"}
	fonts, _ := fs.Glob(assets, "static/fonts/*")
	for _, f := range fonts {
		m.Static = append(m.Static, "/"+f)
	}
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(m)
}

// phoneURL opens someone's work on their phone: the sign-in page with
// their username filled in, then My work, which saves the work on the
// phone. It carries no password or sign-in token: whoever scans it still
// signs in as themselves.
func phoneURL(base, username string) string {
	return base + "/login?user=" + url.QueryEscape(username) + "&next=" + url.QueryEscape("/?take=1")
}

// phoneQR is the QR code of phoneURL, for the person signed in.
func (s *Server) phoneQR(w http.ResponseWriter, r *http.Request, sess *session) error {
	img, err := qrPNG(phoneURL(s.reportBase(r), sess.User.Username), 10)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "image/png")
	_, err = w.Write(img)
	return err
}
