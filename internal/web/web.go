// Package web is pi-fleet's server-rendered user interface, served by
// every Pi: on an employee Pi for its owner, on the master Pi for super
// users, mid-tier users and anyone browsing the fleet.
//
// Pages are plain HTML forms with no JavaScript, so the Content Security
// Policy can forbid scripts entirely (DESIGN.md E1). Every write goes
// through internal/app, so the same validation applies as for synced
// events.
package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"pi-fleet/internal/app"
	"pi-fleet/internal/domain"
	"pi-fleet/internal/password"
	"pi-fleet/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

const (
	cookieName = "pf_session"
	// IdleTimeout ends a session after this long without a request
	// (DESIGN.md §6.5).
	IdleTimeout = 15 * time.Minute
	// MaxSessionAge ends a session regardless of activity.
	MaxSessionAge = 12 * time.Hour
)

// SyncInfo describes an employee Pi's sync state for the status bar.
type SyncInfo struct {
	LastSync time.Time // zero if never
	Unsynced int64
}

// Server serves the UI.
type Server struct {
	App *app.App
	// Role is "central" or "node".
	Role string
	// Secure marks cookies Secure; set when served over HTTPS.
	Secure bool
	// Sync reports sync state on an employee Pi; nil on central.
	Sync func(ctx context.Context) SyncInfo
	// PHIPatterns flag free text that may contain patient information
	// (DESIGN.md §3.6). BlockPHI refuses it instead of asking to confirm.
	PHIPatterns []*regexp.Regexp
	BlockPHI    bool
	// Fleet reaches central's live fleet API from an employee Pi; nil on
	// central.
	Fleet FleetClient
	// Notices returns warnings shown to mid-tier and super users, such
	// as overdue off-site backups.
	Notices func(ctx context.Context) []string
	// Now defaults to time.Now.
	Now func() time.Time

	pages  map[string]*template.Template
	flashM sync.Mutex
	flash  map[string]string
}

// DefaultPHIPatterns catch common ways patient details slip into notes.
var DefaultPHIPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(patient|pt name|mrn|medical record|date of birth|dob|room \d+ bed)\b`),
	regexp.MustCompile(`\b\d{7,10}\b`),                            // MRN-like numbers
	regexp.MustCompile(`\b\d{1,2}[/-]\d{1,2}[/-](19|20)?\d{2}\b`), // dates written as DOBs
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Handler builds the UI's routes.
func (s *Server) Handler() (http.Handler, error) {
	if err := s.parseTemplates(); err != nil {
		return nil, err
	}
	s.flash = map[string]string{}
	static, _ := fs.Sub(assets, "static")
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.logout)
	mux.Handle("GET /password", s.auth(domain.RoleUser, true, s.passwordPage))
	mux.Handle("POST /password", s.auth(domain.RoleUser, true, s.changePassword))

	user := func(h handler) http.Handler { return s.auth(domain.RoleUser, false, h) }
	mid := func(h handler) http.Handler { return s.auth(domain.RoleMidTier, false, h) }
	super := func(h handler) http.Handler { return s.auth(domain.RoleSuperUser, false, h) }

	mux.Handle("GET /{$}", user(s.dashboard))
	mux.Handle("GET /assets", user(s.assetList))
	mux.Handle("GET /assets/new", user(s.assetNew))
	mux.Handle("POST /assets", user(s.assetCreate))
	mux.Handle("GET /assets/{id}", user(s.assetView))
	mux.Handle("POST /assets/{id}/status", user(s.assetStatus))
	mux.Handle("POST /assets/{id}/relocate", user(s.assetRelocate))
	mux.Handle("POST /assets/{id}/edit", user(s.assetEdit))
	mux.Handle("GET /assets/{id}/audit", user(s.auditPage("asset")))
	mux.Handle("GET /assets/{id}/print", user(s.printPage("asset")))
	mux.Handle("GET /assets/{id}/export.json", user(s.exportJSON("asset")))
	mux.Handle("GET /work-orders/{id}/audit", user(s.auditPage("work_order")))
	mux.Handle("GET /work-orders/{id}/print", user(s.printPage("work_order")))
	mux.Handle("GET /work-orders/{id}/export.json", user(s.exportJSON("work_order")))
	if s.Fleet != nil {
		mux.Handle("GET /fleet/assets", user(s.fleetAssets))
		mux.Handle("GET /fleet/assets/{id}", user(s.fleetAsset))
	}
	mux.Handle("GET /work-orders", user(s.workOrderList))
	mux.Handle("GET /work-orders/new", user(s.workOrderNew))
	mux.Handle("POST /work-orders", user(s.workOrderCreate))
	mux.Handle("GET /work-orders/{id}", user(s.workOrderView))
	mux.Handle("POST /work-orders/{id}/claim", user(s.workOrderClaim))
	mux.Handle("POST /work-orders/{id}/assign", mid(s.workOrderAssign))
	mux.Handle("POST /work-orders/{id}/status", user(s.workOrderStatus))
	mux.Handle("POST /work-orders/{id}/sign", user(s.workOrderSign))
	mux.Handle("POST /work-orders/{id}/calibration", user(s.calibrationRecord))
	mux.Handle("POST /calibrations/{id}/void", user(s.calibrationVoid))
	mux.Handle("POST /signatures/{id}/withdraw", user(s.signatureWithdraw))
	mux.Handle("GET /inventory", user(s.inventory))
	mux.Handle("POST /inventory/txn", user(s.stockTxn))
	mux.Handle("POST /inventory/parts", mid(s.partCreate))
	mux.Handle("POST /inventory/locations", user(s.stockLocationCreate))
	mux.Handle("GET /schedules", user(s.scheduleList))
	mux.Handle("POST /schedules", mid(s.scheduleCreate))
	mux.Handle("POST /schedules/{id}/change", mid(s.scheduleChange))
	mux.Handle("POST /schedules/{id}/end", mid(s.scheduleEnd))
	mux.Handle("GET /review", mid(s.review))
	mux.Handle("POST /review/{id}/resolve", mid(s.flagResolve))
	mux.Handle("GET /admin/users", super(s.userList))
	mux.Handle("POST /admin/users", super(s.userCreate))
	mux.Handle("POST /admin/users/{id}/role", super(s.userRole))
	mux.Handle("POST /admin/users/{id}/disable", super(s.userDisable))
	mux.Handle("POST /admin/users/{id}/reset", super(s.userReset))
	mux.Handle("POST /admin/users/{id}/unlock", super(s.userUnlock))
	mux.Handle("GET /admin/sites", super(s.siteList))
	mux.Handle("POST /admin/sites", super(s.siteCreate))
	mux.Handle("POST /admin/locations", super(s.locationCreate))
	if s.Role == "central" {
		mux.Handle("GET /admin/nodes", super(s.nodeList))
		mux.Handle("POST /admin/nodes/{id}/confirm", super(s.nodeConfirm))
		mux.Handle("POST /admin/nodes/{id}/reject", super(s.nodeReject))
		mux.Handle("POST /admin/nodes/{id}/revoke", super(s.nodeRevoke))
		mux.Handle("POST /admin/nodes/{id}/unquarantine", super(s.nodeUnquarantine))
	}
	return securityHeaders(mux), nil
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("Cache-Control", "no-store")
		if r.Method == http.MethodPost && !sameOrigin(r) {
			http.Error(w, "cross-origin request refused", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// sameOrigin refuses POSTs whose Origin (or Referer) is another site.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" || origin == "null" {
		ref := r.Header.Get("Referer")
		if ref == "" {
			return origin == "" // no browser origin info at all: tools and tests
		}
		origin = ref
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

// --- sessions ---

type session struct {
	IDHash       string
	SessionID    string
	User         domain.User
	CSRF         string
	PasswordOnly bool
}

func (s *Server) actor(sess *session) app.Actor {
	return app.Actor{UserID: sess.User.ID, SessionID: sess.SessionID}
}

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Server) exec(ctx context.Context, query string, args ...any) error {
	return s.App.Store.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.ExecContext(ctx, query, args...)
		return err
	})
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u domain.User, passwordOnly bool) error {
	tok := randomToken()
	now := s.now().UTC().Format(time.RFC3339)
	if err := s.exec(r.Context(), `INSERT INTO sessions (id_hash, session_id, user_id, csrf_token, created_at, last_seen_at, password_only)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, hashToken(tok), uuid.NewString(), u.ID, randomToken(), now, now, passwordOnly); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: tok, Path: "/", HttpOnly: true, Secure: s.Secure, SameSite: http.SameSiteStrictMode})
	return nil
}

func (s *Server) loadSession(r *http.Request) (*session, error) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return nil, nil
	}
	ctx := r.Context()
	sess := &session{IDHash: hashToken(c.Value)}
	var userID, created, lastSeen string
	err = s.App.Store.DB().QueryRowContext(ctx, `SELECT session_id, user_id, csrf_token, created_at, last_seen_at, password_only
		FROM sessions WHERE id_hash = ?`, sess.IDHash).Scan(&sess.SessionID, &userID, &sess.CSRF, &created, &lastSeen, &sess.PasswordOnly)
	if err != nil {
		return nil, nil
	}
	now := s.now()
	c0, _ := time.Parse(time.RFC3339, created)
	l0, _ := time.Parse(time.RFC3339, lastSeen)
	if now.Sub(l0) > IdleTimeout || now.Sub(c0) > MaxSessionAge {
		s.exec(ctx, `DELETE FROM sessions WHERE id_hash = ?`, sess.IDHash)
		return nil, nil
	}
	if sess.User, err = domain.GetUser(ctx, s.App.Store.DB(), userID); err != nil {
		return nil, nil
	}
	if sess.User.Status != domain.UserStatusActive && !(sess.PasswordOnly && sess.User.Status == domain.UserStatusPending) || sess.User.Locked(now) {
		s.exec(ctx, `DELETE FROM sessions WHERE id_hash = ?`, sess.IDHash)
		return nil, nil
	}
	// A password that expired mid-session restricts the session.
	if sess.User.MustChangePassword || sess.User.PasswordExpired(now) {
		sess.PasswordOnly = true
	}
	return sess, s.exec(ctx, `UPDATE sessions SET last_seen_at = ? WHERE id_hash = ?`, now.UTC().Format(time.RFC3339), sess.IDHash)
}

type handler func(w http.ResponseWriter, r *http.Request, sess *session) error

var roleRank = map[string]int{domain.RoleUser: 1, domain.RoleMidTier: 2, domain.RoleSuperUser: 3}

// auth requires a signed-in user with at least minRole. Sessions limited
// to changing the password reach only handlers that allow it. Every POST
// must carry the session's CSRF token.
func (s *Server) auth(minRole string, allowPasswordOnly bool, h handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess, err := s.loadSession(r)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if sess == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if sess.PasswordOnly && !allowPasswordOnly {
			http.Redirect(w, r, "/password", http.StatusSeeOther)
			return
		}
		if roleRank[sess.User.Role] < roleRank[minRole] {
			http.Error(w, "you don't have access to this page", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil {
				http.Error(w, "bad form", http.StatusBadRequest)
				return
			}
			if subtle.ConstantTimeCompare([]byte(r.PostFormValue("csrf")), []byte(sess.CSRF)) != 1 {
				http.Error(w, "form expired; reload the page and try again", http.StatusForbidden)
				return
			}
		}
		if err := h(w, r, sess); err != nil {
			s.fail(w, r, err)
		}
	})
}

// userError is an error whose message is meant for the user.
type userError struct{ msg string }

func (e *userError) Error() string { return e.msg }

func userErr(format string, args ...any) error { return &userError{fmt.Sprintf(format, args...)} }

// message turns an error into something to show the user, or "" if it
// is an internal failure.
func message(err error) string {
	var ue *userError
	var rej *store.Rejection
	var pol *password.PolicyError
	switch {
	case errors.As(err, &ue):
		return ue.msg
	case errors.As(err, &rej):
		return rej.Detail
	case errors.Is(err, app.ErrBadCredentials), errors.Is(err, app.ErrLocked), errors.Is(err, app.ErrClockUnverified),
		errors.Is(err, app.ErrTemporaryExpired), errors.Is(err, app.ErrNotLeaseHolder), errors.Is(err, app.ErrMustChangePassword):
		return strings.TrimPrefix(err.Error(), "app: ")
	case errors.As(err, &pol):
		return "Password " + pol.Reason + "."
	}
	return ""
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, domain.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	log.Printf("web: %s %s: %v", r.Method, r.URL.Path, err)
	http.Error(w, "something went wrong; the error has been logged", http.StatusInternalServerError)
}

// done redirects after a successful POST, showing msg once.
func (s *Server) done(w http.ResponseWriter, r *http.Request, sess *session, to, msg string) error {
	s.flashM.Lock()
	s.flash[sess.SessionID] = msg
	s.flashM.Unlock()
	http.Redirect(w, r, to, http.StatusSeeOther)
	return nil
}

// failed redirects back after a POST the app refused, showing why. Other
// errors propagate as internal failures.
func (s *Server) failed(w http.ResponseWriter, r *http.Request, sess *session, to string, err error) error {
	msg := message(err)
	if msg == "" {
		return err
	}
	return s.done(w, r, sess, to, "Not saved: "+msg)
}

func (s *Server) takeFlash(sess *session) string {
	s.flashM.Lock()
	defer s.flashM.Unlock()
	m := s.flash[sess.SessionID]
	delete(s.flash, sess.SessionID)
	return m
}

// phiCheck reports whether text looks like it holds patient information
// and the user hasn't confirmed it doesn't.
func (s *Server) phiCheck(r *http.Request, texts ...string) error {
	for _, t := range texts {
		for _, re := range s.PHIPatterns {
			if !re.MatchString(t) {
				continue
			}
			if s.BlockPHI {
				return userErr("this text looks like it may contain patient information, which must never be entered here. Remove it and try again")
			}
			if r.PostFormValue("no_phi") != "yes" {
				return userErr("this text looks like it may contain patient information. Remove it, or tick “I confirm this contains no patient information” and save again")
			}
		}
	}
	return nil
}
