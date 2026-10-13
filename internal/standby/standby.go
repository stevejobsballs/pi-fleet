// Package standby keeps a second Pi, the standby, as a live copy of the
// master Pi, ready to take its place if the master fails.
//
// A super user makes a one-time pairing code on the master; the standby's
// setup pairs with it and receives a token for this API. The standby then
// starts from a snapshot of the master's database and, every few seconds,
// copies the events the master has stored since, in the master's order,
// verifying each as the master did and applying it, so its records match.
// It also copies the attachments, the master's settings and its key and
// certificate files, so that taking over (in setup, on the standby, by
// hand) gives employee Pis, phones and people the same master.
package standby

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"pi-fleet/internal/blobs"
	"pi-fleet/internal/event"
	"pi-fleet/internal/fleetsync"
	"pi-fleet/internal/store"
)

// API paths, on the master Pi.
const (
	PathPair     = "/v1/standby/pair"
	PathSnapshot = "/v1/standby/snapshot"
	PathEvents   = "/v1/standby/events"
	PathState    = "/v1/standby/state"
	PathBlobs    = "/v1/standby/blobs/"
)

// Settings on the master (node_config), not copied to the standby.
const (
	configToken  = "standby_token" // sha256 of the standby's token
	configCode   = "standby_code"  // sha256 of the pairing code, space, expiry (RFC 3339)
	configPrefix = "standby_"
)

// CodeLifetime is how long a pairing code works.
const CodeLifetime = time.Hour

// Files the standby copies from the master's data folder: its keys and
// its certificates.
var copiedDirs = []string{"keys", "tls"}

// Events is one batch of the master's events, with their positions.
type Events struct {
	Events []fleetsync.WireEvent `json:"events"`
	Orders []int64               `json:"orders"`
	Head   int64                 `json:"head"` // the master's last position
	// AtHash is the hash of the master's event at the position asked
	// after, so the standby can check its copy matches the master's.
	AtHash []byte `json:"at_hash,omitempty"`
}

// State is the master's settings and files.
type State struct {
	Config map[string]string `json:"config"`
	Files  map[string][]byte `json:"files"` // by path in the data folder
}

// Server is the master Pi's side.
type Server struct {
	Store   *store.Store
	DataDir string
	Blobs   *blobs.Store
	Now     func() time.Time

	mu       sync.Mutex
	seen     time.Time // the standby's last request
	at, head int64     // its position then, and the master's
	tries    int       // failed pairings since the last code
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func hashOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// NewCode makes a one-time pairing code, replacing any earlier one.
func (s *Server) NewCode(ctx context.Context) (string, time.Time, error) {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	raw := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)[:12]
	code := raw[0:4] + "-" + raw[4:8] + "-" + raw[8:12]
	until := s.now().Add(CodeLifetime)
	s.mu.Lock()
	s.tries = 0
	s.mu.Unlock()
	return code, until, s.Store.SetConfig(ctx, configCode, hashOf(normCode(code))+" "+until.UTC().Format(time.RFC3339))
}

func normCode(c string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(c))
}

// Paired reports whether a standby is paired.
func (s *Server) Paired(ctx context.Context) bool {
	v, _ := s.Store.Config(ctx, configToken)
	return v != ""
}

// Unpair forgets the standby: its token stops working.
func (s *Server) Unpair(ctx context.Context) error {
	if err := s.Store.SetConfig(ctx, configToken, ""); err != nil {
		return err
	}
	s.mu.Lock()
	s.seen, s.at, s.head = time.Time{}, 0, 0
	s.mu.Unlock()
	return s.Store.SetConfig(ctx, configCode, "")
}

// Status is how the standby is doing, as the master sees it.
type Status struct {
	Paired bool
	Seen   time.Time // last contact; zero if none since the master started
	Behind int64     // events the standby didn't have at its last contact
}

func (s *Server) Status(ctx context.Context) Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{Paired: s.Paired(ctx), Seen: s.seen, Behind: max(s.head-s.at, 0)}
}

// Handler serves the standby API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+PathPair, s.pair)
	mux.HandleFunc("GET "+PathSnapshot, s.authed(s.snapshot))
	mux.HandleFunc("GET "+PathEvents, s.authed(s.events))
	mux.HandleFunc("GET "+PathState, s.authed(s.state))
	mux.HandleFunc("GET "+PathBlobs+"{sha}", s.authed(s.blob))
	return mux
}

// PairRequest and PairResponse pair a standby with a one-time code.
type PairRequest struct {
	Code string `json:"code"`
}

type PairResponse struct {
	Token string `json:"token"`
}

func (s *Server) pair(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req PairRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.tries++
	tries := s.tries
	s.mu.Unlock()
	stored, _ := s.Store.Config(ctx, configCode)
	hash, until, _ := strings.Cut(stored, " ")
	exp, err := time.Parse(time.RFC3339, until)
	switch {
	case tries > 10: // a code can't be guessed by trying many
		http.Error(w, "too many tries: make a new pairing code on the master Pi", http.StatusTooManyRequests)
		return
	case stored == "" || err != nil || s.now().After(exp):
		http.Error(w, "no pairing code is waiting: make one on the master Pi's Pis page", http.StatusForbidden)
		return
	case subtle.ConstantTimeCompare([]byte(hash), []byte(hashOf(normCode(req.Code)))) != 1:
		http.Error(w, "that pairing code isn't right", http.StatusForbidden)
		return
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	token := hex.EncodeToString(b)
	if err := s.Store.SetConfig(ctx, configToken, hashOf(token)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.Store.SetConfig(ctx, configCode, "") // used
	writeJSON(w, PairResponse{Token: token})
}

func (s *Server) authed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		want, _ := s.Store.Config(r.Context(), configToken)
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || want == "" || subtle.ConstantTimeCompare([]byte(hashOf(got)), []byte(want)) != 1 {
			http.Error(w, "not this master's standby", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	f, err := os.CreateTemp(s.DataDir, ".standby-snapshot-*")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	path := f.Name()
	f.Close()
	os.Remove(path) // VACUUM INTO makes it
	defer os.Remove(path)
	if err := s.Store.SnapshotTo(r.Context(), path); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// The pairing is this master's own: a standby that takes over starts
	// with none.
	if err := forgetPairing(r.Context(), path); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	snap, err := os.Open(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer snap.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	io.Copy(w, snap)
}

func forgetPairing(ctx context.Context, path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, `DELETE FROM node_config WHERE key LIKE ?`, configPrefix+"%")
	return err
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	evs, orders, err := s.Store.EventsAfter(ctx, after, 500)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	head, err := s.Store.LastLocalOrder(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.mu.Lock()
	s.seen, s.at, s.head = s.now(), after, head
	s.mu.Unlock()
	out := Events{Orders: orders, Head: head, Events: make([]fleetsync.WireEvent, len(evs))}
	if after > 0 {
		s.Store.DB().QueryRowContext(ctx, `SELECT hash FROM events WHERE local_order = ?`, after).Scan(&out.AtHash)
	}
	if out.Orders == nil {
		out.Orders = []int64{}
	}
	for i, e := range evs {
		out.Events[i] = fleetsync.ToWire(e)
	}
	writeJSON(w, out)
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st := State{Config: map[string]string{}, Files: map[string][]byte{}}
	rows, err := s.Store.DB().QueryContext(ctx, `SELECT key, value FROM node_config`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			rows.Close()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !strings.HasPrefix(k, configPrefix) {
			st.Config[k] = v
		}
	}
	rows.Close()
	for _, d := range copiedDirs {
		entries, _ := os.ReadDir(filepath.Join(s.DataDir, d))
		for _, e := range entries {
			if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(s.DataDir, d, e.Name()))
			if err == nil && len(b) < 1<<20 {
				st.Files[d+"/"+e.Name()] = b
			}
		}
	}
	writeJSON(w, st)
}

func (s *Server) blob(w http.ResponseWriter, r *http.Request) {
	b, err := s.Blobs.Get(r.PathValue("sha"))
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(b)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// --- the standby's side ---

// LinkFile, in the standby's data folder, says which master it follows.
const LinkFile = "standby.json"

// StatusFile, in the standby's data folder, says how it is doing.
const StatusFile = "standby-status.json"

// Link is how a standby reaches its master.
type Link struct {
	Master string `json:"master"` // https://name[:port]
	CA     string `json:"ca"`     // the master's own certificate, pinned (PEM)
	Token  string `json:"token"`
}

// Pair pairs with the master using a one-time code.
func Pair(ctx context.Context, c *http.Client, master, code string) (string, error) {
	body, _ := json.Marshal(PairRequest{Code: code})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, master+PathPair, strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return "", errors.New(strings.TrimSpace(string(msg)))
	}
	var pr PairResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil || pr.Token == "" {
		return "", errors.New("the master's answer couldn't be read")
	}
	return pr.Token, nil
}

// Report is how the standby is doing, as it records it.
type Report struct {
	LastOK time.Time `json:"last_ok"` // the last time the copy was brought up to date
	At     int64     `json:"at"`      // its position then
	Head   int64     `json:"head"`    // the master's position then
	Error  string    `json:"error,omitempty"`
	Since  time.Time `json:"since,omitempty"` // failing since
}

// ReadReport reads the standby's report ("" fields when there is none).
func ReadReport(dataDir string) Report {
	var r Report
	b, err := os.ReadFile(filepath.Join(dataDir, StatusFile))
	if err == nil {
		json.Unmarshal(b, &r)
	}
	return r
}

// Follower keeps a standby's copy of the master up to date.
type Follower struct {
	DataDir string
	Link    Link
	HTTP    *http.Client
	Blobs   *blobs.Store
	// Open opens the copy (pi-fleet.db in DataDir) with the master's
	// projections; Close closes it.
	Open func(ctx context.Context) (*store.Store, error)
	Now  func() time.Time
	Logf func(string, ...any)

	st     *store.Store
	rounds int
	report Report
}

func (f *Follower) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// Close closes the copy.
func (f *Follower) Close() error {
	if f.st != nil {
		err := f.st.Close()
		f.st = nil
		return err
	}
	return nil
}

func (f *Follower) get(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.Link.Master+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+f.Link.Token)
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		resp.Body.Close()
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return resp, nil
}

func (f *Follower) getJSON(ctx context.Context, path string, v any) error {
	resp, err := f.get(ctx, path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}

// Step brings the copy up to date once: the snapshot to start from if
// there is no copy yet, the events since, and now and then the settings,
// files and attachments.
func (f *Follower) Step(ctx context.Context) error {
	err := f.step(ctx)
	now := f.now()
	if err != nil {
		if f.report.Error == "" {
			f.report.Since = now
		}
		f.report.Error = plain(err)
	} else {
		f.report.Error, f.report.Since, f.report.LastOK = "", time.Time{}, now
	}
	if b, jerr := json.Marshal(f.report); jerr == nil {
		writeFile(filepath.Join(f.DataDir, StatusFile), b, 0o644)
	}
	return err
}

// plain says what went wrong in words for the standby's report (the
// log has the details).
func plain(err error) string {
	var op *net.OpError
	var nerr net.Error
	if errors.As(err, &op) || errors.As(err, &nerr) && nerr.Timeout() {
		return "can't reach the master Pi"
	}
	return err.Error()
}

func (f *Follower) step(ctx context.Context) error {
	db := filepath.Join(f.DataDir, "pi-fleet.db")
	if _, err := os.Stat(db); errors.Is(err, os.ErrNotExist) {
		if err := f.syncState(ctx); err != nil {
			return err
		}
		if err := f.fetchSnapshot(ctx, db); err != nil {
			return err
		}
	}
	if f.st == nil {
		st, err := f.Open(ctx)
		if err != nil {
			return err
		}
		f.st = st
	}
	for {
		at, err := f.st.LastLocalOrder(ctx)
		if err != nil {
			return err
		}
		var batch Events
		if err := f.getJSON(ctx, PathEvents+"?after="+strconv.FormatInt(at, 10), &batch); err != nil {
			return err
		}
		f.report.At, f.report.Head = at, batch.Head
		// The copy must be the master's: the same event at its position.
		if at > 0 {
			_, mine, err := f.st.ReplicaHead(ctx)
			if err != nil {
				return err
			}
			switch {
			case batch.Head < at:
				return fmt.Errorf("the master has fewer records (%d) than this copy (%d): it may have been restored from an older backup, or be a different master", batch.Head, at)
			case !bytes.Equal(batch.AtHash, mine):
				return errors.New("this copy doesn't match the master's records (a different master?): set the standby up again")
			}
		}
		if len(batch.Events) == 0 {
			break
		}
		evs := make([]event.Event, 0, len(batch.Events))
		for _, w := range batch.Events {
			e, err := fleetsync.FromWire(w)
			if err != nil {
				return err
			}
			evs = append(evs, e)
		}
		if err := f.st.Replicate(ctx, evs, batch.Orders); err != nil {
			return err
		}
		f.report.At = batch.Orders[len(batch.Orders)-1]
	}
	if f.rounds%12 == 0 { // about every minute
		if err := f.syncState(ctx); err != nil {
			return err
		}
		if err := f.syncBlobs(ctx); err != nil {
			return err
		}
	}
	f.rounds++
	return nil
}

func (f *Follower) fetchSnapshot(ctx context.Context, db string) error {
	resp, err := f.get(ctx, PathSnapshot)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	tmp := db + ".new"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if f.Logf != nil {
		f.Logf("standby: copied the master's records to start from")
	}
	return os.Rename(tmp, db)
}

// syncState copies the master's settings and its key and certificate
// files.
func (f *Follower) syncState(ctx context.Context) error {
	var st State
	if err := f.getJSON(ctx, PathState, &st); err != nil {
		return err
	}
	for path, b := range st.Files {
		dir, name := filepath.Split(filepath.Clean(path))
		dir = filepath.Clean(dir)
		if !validCopied(dir) || name == "" || strings.HasPrefix(name, ".") {
			continue
		}
		full := filepath.Join(f.DataDir, dir, name)
		if old, err := os.ReadFile(full); err == nil && string(old) == string(b) {
			continue
		}
		if err := os.MkdirAll(filepath.Join(f.DataDir, dir), 0o700); err != nil {
			return err
		}
		mode := os.FileMode(0o600)
		if strings.HasSuffix(name, "cert.pem") {
			mode = 0o644
		}
		if err := writeFile(full, b, mode); err != nil {
			return err
		}
	}
	if f.st != nil {
		for k, v := range st.Config {
			if err := f.st.SetConfig(ctx, k, v); err != nil {
				return err
			}
		}
	}
	return nil
}

func validCopied(dir string) bool {
	for _, d := range copiedDirs {
		if dir == d {
			return true
		}
	}
	return false
}

// syncBlobs fetches the attachments the copy lists but doesn't have.
func (f *Follower) syncBlobs(ctx context.Context) error {
	rows, err := f.st.DB().QueryContext(ctx, `SELECT DISTINCT sha256 FROM attachments WHERE status != 'purged'`)
	if err != nil {
		return err
	}
	var want []string
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			rows.Close()
			return err
		}
		if !f.Blobs.Has(sha) {
			want = append(want, sha)
		}
	}
	rows.Close()
	for _, sha := range want {
		resp, err := f.get(ctx, PathBlobs+sha)
		if err != nil {
			return fmt.Errorf("attachment %s: %w", sha, err)
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, blobs.MaxSize+1))
		resp.Body.Close()
		if err != nil {
			return err
		}
		if err := f.Blobs.PutExpected(sha, b); err != nil {
			return fmt.Errorf("attachment %s: %w", sha, err)
		}
	}
	return nil
}

// Run follows the master until ctx ends, every interval.
func (f *Follower) Run(ctx context.Context, interval time.Duration) error {
	defer f.Close()
	for {
		if err := f.Step(ctx); err != nil && f.Logf != nil && ctx.Err() == nil {
			f.Logf("standby: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}

func writeFile(path string, b []byte, mode os.FileMode) error {
	tmp := path + ".new"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
