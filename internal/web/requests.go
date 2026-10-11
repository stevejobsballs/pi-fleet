package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"rsc.io/qr"

	"pi-fleet/internal/domain"
	"pi-fleet/internal/fleetsync"
)

// QR labels and problem reports. Every piece of equipment has a QR label
// naming it by its MasterID (or its asset tag, when it has none). Scanning
// it opens the master Pi's report page, where anyone on the hospital
// network can report a problem without signing in. Reports wait on the
// Requests page until someone turns each into a work order or closes it.

// reportCode is what a QR label names equipment by.
func reportCode(a domain.Asset) string {
	if a.MasterID != "" {
		return a.MasterID
	}
	return a.Tag
}

// reportBase is the master Pi's address as phones reach it: on the master,
// the address this page was opened with; on an employee Pi, the master's.
func (s *Server) reportBase(r *http.Request) string {
	if s.Role != "central" {
		if u, err := s.App.Store.Config(r.Context(), fleetsync.ConfigCentralURL); err == nil && u != "" {
			return strings.TrimSuffix(u, "/")
		}
	}
	scheme := "https"
	if r.TLS == nil {
		scheme = "http" // the preview, which runs without HTTPS
	}
	return scheme + "://" + r.Host
}

func reportURL(base, code string) string { return base + "/r/" + url.PathEscape(code) }

// qrPNG draws text as a QR code with the quiet margin scanners need.
func qrPNG(text string, scale int) ([]byte, error) {
	c, err := qr.Encode(text, qr.M)
	if err != nil {
		return nil, err
	}
	const margin = 4 // modules of white around the code
	n := (c.Size + 2*margin) * scale
	img := image.NewPaletted(image.Rect(0, 0, n, n), color.Palette{color.White, color.Black})
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; x++ {
			if !c.Black(x, y) {
				continue
			}
			for dy := 0; dy < scale; dy++ {
				for dx := 0; dx < scale; dx++ {
					img.SetColorIndex((x+margin)*scale+dx, (y+margin)*scale+dy, 1)
				}
			}
		}
	}
	var buf bytes.Buffer
	err = png.Encode(&buf, img)
	return buf.Bytes(), err
}

// assetQR serves an asset's QR code; ?download=1 saves it as a file.
func (s *Server) assetQR(w http.ResponseWriter, r *http.Request, sess *session) error {
	a, err := domain.GetAsset(r.Context(), s.App.Store.DB(), r.PathValue("id"))
	if err != nil {
		return err
	}
	img, err := qrPNG(reportURL(s.reportBase(r), reportCode(a)), 12)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "image/png")
	if r.URL.Query().Get("download") != "" {
		w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape("QR "+reportCode(a)+".png"))
	}
	_, err = w.Write(img)
	return err
}

type labelData struct {
	Asset     domain.Asset
	Code, URL string
}

// assetLabel is a page holding only the printable label. It is the one
// page allowed a script: the same-site label.js, which opens the print
// dialog when Print is pressed.
func (s *Server) assetLabel(w http.ResponseWriter, r *http.Request, sess *session) error {
	a, err := domain.GetAsset(r.Context(), s.App.Store.DB(), r.PathValue("id"))
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := s.pages["label"].ExecuteTemplate(&buf, "label", labelData{Asset: a, Code: reportCode(a), URL: reportURL(s.reportBase(r), reportCode(a))}); err != nil {
		return err
	}
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, err = buf.WriteTo(w)
	return err
}

// --- the report page (no sign-in) ---

// reportLimiter limits how often problems can be reported: per address,
// and from everyone together, so the form can't be used to flood the
// Requests page.
type reportLimiter struct {
	mu     sync.Mutex
	recent map[string][]time.Time
	all    []time.Time
}

const (
	reportsPerAddress = 5 // in reportWindow
	reportsInAll      = 120
	reportWindow      = 10 * time.Minute
)

func (l *reportLimiter) allow(addr string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.recent == nil {
		l.recent = map[string][]time.Time{}
	}
	keep := func(ts []time.Time) []time.Time {
		out := ts[:0]
		for _, t := range ts {
			if now.Sub(t) < reportWindow {
				out = append(out, t)
			}
		}
		return out
	}
	for k, ts := range l.recent {
		if l.recent[k] = keep(ts); len(l.recent[k]) == 0 {
			delete(l.recent, k)
		}
	}
	l.all = keep(l.all)
	if len(l.recent[addr]) >= reportsPerAddress || len(l.all) >= reportsInAll {
		return false
	}
	l.recent[addr] = append(l.recent[addr], now)
	l.all = append(l.all, now)
	return true
}

type reportData struct {
	Code       string
	Asset      domain.Asset
	Place      string
	Categories []string
	F          url.Values
	Error      string
	Request    *domain.ServiceRequest // the status page
}

func (s *Server) reportAsset(ctx context.Context, code string) (domain.Asset, string, error) {
	a, err := domain.ReportTarget(ctx, s.App.Store.DB(), code)
	if err != nil {
		return a, "", err
	}
	var place string
	s.App.Store.DB().QueryRowContext(ctx, `SELECT s.path || ' › ' || l.name FROM locations l JOIN sites s ON s.id = l.site_id WHERE l.id = ?`, a.LocationID).Scan(&place)
	return a, place, nil
}

func (s *Server) reportPage(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	a, place, err := s.reportAsset(r.Context(), code)
	if errors.Is(err, domain.ErrNotFound) {
		w.WriteHeader(http.StatusNotFound)
		err = s.render(w, r, nil, "report", "Report a problem", reportData{Code: code})
	} else if err == nil {
		err = s.render(w, r, nil, "report", "Report a problem", reportData{Code: code, Asset: a, Place: place, Categories: domain.RequestCategories, F: url.Values{}})
	}
	if err != nil {
		s.fail(w, r, err)
	}
}

func (s *Server) reportSubmit(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	a, place, err := s.reportAsset(r.Context(), code)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d := reportData{Code: code, Asset: a, Place: place, Categories: domain.RequestCategories, F: r.PostForm}
	f := func(k string) string { return strings.TrimSpace(r.PostFormValue(k)) }
	if err = s.phiCheck(r, f("description"), f("name"), f("department")); err == nil {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if !s.reports.allow(host, s.now()) {
			err = userErr("too many problems have been reported from here in the last few minutes. Please wait a little, or phone the biomedical engineering team")
		}
	}
	var number string
	if err == nil {
		_, number, err = s.App.SubmitRequest(r.Context(), domain.RequestSubmitted{AssetID: a.ID, Category: r.PostFormValue("category"),
			Description: f("description"), Name: f("name"), Department: f("department"), Phone: f("phone")})
	}
	if err != nil {
		msg := message(err)
		if msg == "" {
			s.fail(w, r, err)
			return
		}
		d.Error = "Not sent: " + msg + "."
		w.WriteHeader(http.StatusUnprocessableEntity)
		if err := s.render(w, r, nil, "report", "Report a problem", d); err != nil {
			s.fail(w, r, err)
		}
		return
	}
	http.Redirect(w, r, "/r/status/"+number, http.StatusSeeOther)
}

// reportStatus tells the person who reported a problem what became of it.
// It shows nothing about who reported it.
func (s *Server) reportStatus(w http.ResponseWriter, r *http.Request) {
	q, err := domain.GetRequest(r.Context(), s.App.Store.DB(), r.PathValue("number"))
	if err == nil && !strings.HasPrefix(r.PathValue("number"), "R-") {
		err = domain.ErrNotFound // only by its number, never its internal id
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.render(w, r, nil, "report", "Problem "+q.Number, reportData{Request: &q}); err != nil {
		s.fail(w, r, err)
	}
}

// --- the Requests page ---

type requestsData struct {
	New, Handled []domain.ServiceRequest
	Priorities   []string
	Types        []string
}

func (s *Server) requestList(w http.ResponseWriter, r *http.Request, sess *session) error {
	ctx, q := r.Context(), s.App.Store.DB()
	d := requestsData{Priorities: []string{"normal", "high", "urgent", "low"}, Types: []string{"corrective", "inspection"}}
	var err error
	if d.New, err = domain.ListRequests(ctx, q, domain.RequestStatusNew, 200); err != nil {
		return err
	}
	for _, st := range []string{domain.RequestStatusConverted, domain.RequestStatusClosed} {
		h, err := domain.ListRequests(ctx, q, st, 20)
		if err != nil {
			return err
		}
		d.Handled = append(d.Handled, h...)
	}
	return s.render(w, r, sess, "requests", "Reported problems", d)
}

func (s *Server) requestConvert(w http.ResponseWriter, r *http.Request, sess *session) error {
	id, number, err := s.App.ConvertRequest(r.Context(), s.actor(sess), r.PathValue("id"), r.PostFormValue("type"), r.PostFormValue("priority"))
	if err != nil {
		return s.failed(w, r, sess, "/requests", err)
	}
	return s.done(w, r, sess, "/work-orders/"+id, fmt.Sprintf("Work order %s opened from the reported problem.", number))
}

func (s *Server) requestClose(w http.ResponseWriter, r *http.Request, sess *session) error {
	if err := s.App.CloseRequest(r.Context(), s.actor(sess), r.PathValue("id"), strings.TrimSpace(r.PostFormValue("reason"))); err != nil {
		return s.failed(w, r, sess, "/requests", err)
	}
	return s.done(w, r, sess, "/requests", "Closed. The person who reported it sees the reason.")
}
