package fleetsync

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"pi-fleet/internal/app"
	"pi-fleet/internal/domain"
	"pi-fleet/internal/event"
	"pi-fleet/internal/httpsig"
	"pi-fleet/internal/pairing"
	"pi-fleet/internal/password"
	"pi-fleet/internal/store"
)

const (
	maxBody         = 2 << 20 // per request
	maxSkew         = 5 * time.Minute
	nonceLifetime   = 10 * time.Minute
	activationLimit = 20 // activation requests per IP per minute
)

// Server is central's sync API.
type Server struct {
	App *app.App
	// CentralKey signs snapshots; it is central's event key.
	CentralKey ed25519.PrivateKey
	// Now defaults to time.Now.
	Now func() time.Time
	// Logf defaults to log.Printf.
	Logf func(format string, args ...any)
	// Fleet serves live fleet-wide reads (and shared stockroom counts)
	// under /v1/fleet/ for confirmed Pis (DESIGN.md §5.9). The requesting
	// node is in the request context (NodeFromContext).
	Fleet http.Handler

	mu      sync.Mutex
	secret  []byte
	buckets map[string]*bucket
}

type bucket struct {
	start time.Time
	n     int
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// Handler returns the HTTP handler for the sync API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+PathChallenge, s.limited(s.handleChallenge))
	mux.HandleFunc("POST "+PathActivate, s.limited(s.handleActivate))
	mux.HandleFunc("GET "+PathStatus, s.signed(s.handleStatus, anyStatus))
	mux.HandleFunc("POST "+PathHello, s.signed(s.handleHello, anyStatus))
	mux.HandleFunc("POST "+PathEvents, s.signed(s.handleEvents, canPush))
	mux.HandleFunc("GET "+PathSnapshot, s.signed(s.handleSnapshot, activeOnly))
	if s.Fleet != nil {
		mux.HandleFunc(PathFleet, s.signed(s.handleFleet, activeOnly))
	}
	return mux
}

// httpError is an error with a status code and a message safe to show.
type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return e.msg }

func fail(code int, format string, args ...any) error {
	return &httpError{code, fmt.Sprintf(format, args...)}
}

func (s *Server) writeErr(w http.ResponseWriter, r *http.Request, err error) {
	// Tell clients central's time so a Pi with a wrong clock can still sign.
	w.Header().Set("Date", s.now().UTC().Format(http.TimeFormat))
	var he *httpError
	if !errors.As(err, &he) {
		s.logf("sync: %s %s: %v", r.Method, r.URL.Path, err)
		he = &httpError{http.StatusInternalServerError, "internal error"}
	}
	http.Error(w, he.msg, he.code)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, body []byte, v any) error {
	if err := json.Unmarshal(body, v); err != nil {
		return fail(http.StatusBadRequest, "bad JSON: %v", err)
	}
	return nil
}

func readBody(r *http.Request) ([]byte, error) {
	b, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBody))
	if err != nil {
		return nil, fail(http.StatusRequestEntityTooLarge, "request too large")
	}
	return b, nil
}

// --- rate limiting for unauthenticated endpoints ---

func (s *Server) limited(h func(http.ResponseWriter, *http.Request, []byte) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip, _, _ := net.SplitHostPort(r.RemoteAddr)
		if !s.allow("ip:"+ip, activationLimit) {
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		body, err := readBody(r)
		if err == nil {
			err = h(w, r, body)
		}
		if err != nil {
			s.writeErr(w, r, err)
		}
	}
}

func (s *Server) allow(key string, limit int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buckets == nil {
		s.buckets = map[string]*bucket{}
	}
	now := s.now()
	b := s.buckets[key]
	if b == nil || now.Sub(b.start) > time.Minute {
		b = &bucket{start: now}
		s.buckets[key] = b
	}
	b.n++
	return b.n <= limit
}

// --- signed endpoints ---

type statusRule func(domain.Node) bool

func anyStatus(domain.Node) bool    { return true }
func activeOnly(n domain.Node) bool { return n.Status == domain.NodeStatusActive }
func canPush(n domain.Node) bool {
	return n.Status == domain.NodeStatusActive || (n.Status == domain.NodeStatusRevoked && n.KeepUnsynced)
}

type signedHandler func(w http.ResponseWriter, r *http.Request, n domain.Node, body []byte) error

// signed authenticates a request by its RFC 9421 signature with a node's
// transport key, refusing replayed nonces.
func (s *Server) signed(h signedHandler, rule statusRule) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		err := func() error {
			body, err := readBody(r)
			if err != nil {
				return err
			}
			ctx := r.Context()
			var node domain.Node
			v, err := httpsig.Verify(r, body, func(keyID string) (ed25519.PublicKey, error) {
				n, err := domain.GetNodeByTransportKey(ctx, s.App.Store.DB(), keyID)
				if err != nil {
					return nil, err
				}
				node = n
				return n.TransportPub, nil
			}, s.now(), maxSkew)
			if err != nil {
				return fail(http.StatusUnauthorized, "signature: %v", err)
			}
			if err := s.useNonce(ctx, v.Nonce); err != nil {
				return err
			}
			if !rule(node) {
				return fail(http.StatusForbidden, "node is %s", node.Status)
			}
			return h(w, r, node, body)
		}()
		if err != nil {
			s.writeErr(w, r, err)
		}
	}
}

func (s *Server) useNonce(ctx context.Context, nonce string) error {
	now := s.now().UTC()
	return s.App.Store.Update(ctx, func(tx *store.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM request_nonces WHERE expires_at < ?`, now.Format(time.RFC3339)); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO request_nonces (nonce, expires_at) VALUES (?, ?) ON CONFLICT DO NOTHING`,
			nonce, now.Add(nonceLifetime).Format(time.RFC3339))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fail(http.StatusUnauthorized, "replayed request")
		}
		return nil
	})
}

// --- activation ---

func (s *Server) fakeSalt(ctx context.Context, username string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.secret == nil {
		v, err := s.App.Store.Config(ctx, "activation_secret")
		if errors.Is(err, store.ErrNotFound) {
			b := make([]byte, 32)
			if _, err := rand.Read(b); err != nil {
				return nil, err
			}
			v = hex.EncodeToString(b)
			if err := s.App.Store.SetConfig(ctx, "activation_secret", v); err != nil {
				return nil, err
			}
		} else if err != nil {
			return nil, err
		}
		s.secret, _ = hex.DecodeString(v)
	}
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(username))
	return m.Sum(nil)[:16], nil
}

func (s *Server) handleChallenge(w http.ResponseWriter, r *http.Request, body []byte) error {
	ctx := r.Context()
	var req ChallengeRequest
	if err := readJSON(r, body, &req); err != nil {
		return err
	}
	resp := ChallengeResponse{Time: password.Default.Time, MemoryKiB: password.Default.MemoryKiB, Threads: password.Default.Threads, KeyLen: 32}
	u, err := domain.GetUserByUsername(ctx, s.App.Store.DB(), req.Username)
	switch {
	case err == nil && u.MustChangePassword:
		v, err := password.Parse(u.Verifier)
		if err != nil {
			return err
		}
		resp.Salt, resp.Time, resp.MemoryKiB, resp.Threads, resp.KeyLen = v.Salt, v.Params.Time, v.Params.MemoryKiB, v.Params.Threads, len(v.Key)
	case err == nil || errors.Is(err, domain.ErrNotFound):
		// Don't reveal whether the account exists or is awaiting activation.
		if resp.Salt, err = s.fakeSalt(ctx, req.Username); err != nil {
			return err
		}
	default:
		return err
	}
	n := make([]byte, 24)
	if _, err := rand.Read(n); err != nil {
		return err
	}
	resp.Nonce = hex.EncodeToString(n)
	now := s.now().UTC()
	err = s.App.Store.Update(ctx, func(tx *store.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM activation_nonces WHERE expires_at < ?`, now.Format(time.RFC3339)); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO activation_nonces (nonce, username, expires_at) VALUES (?, ?, ?)`,
			resp.Nonce, req.Username, now.Add(5*time.Minute).Format(time.RFC3339))
		return err
	})
	if err != nil {
		return err
	}
	writeJSON(w, resp)
	return nil
}

var errActivationFailed = &httpError{http.StatusUnauthorized, "activation failed: check the username and one-time password"}

func (s *Server) handleActivate(w http.ResponseWriter, r *http.Request, body []byte) error {
	ctx := r.Context()
	var req ActivateRequest
	if err := readJSON(r, body, &req); err != nil {
		return err
	}
	// The nonce is single-use and bound to the username it was issued for.
	now := s.now().UTC()
	var consumed int64
	err := s.App.Store.Update(ctx, func(tx *store.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM activation_nonces WHERE nonce = ? AND username = ? AND expires_at >= ?`,
			req.Nonce, req.Username, now.Format(time.RFC3339))
		if err == nil {
			consumed, _ = res.RowsAffected()
		}
		return err
	})
	if err != nil {
		return err
	}
	if consumed != 1 {
		return errActivationFailed
	}
	u, err := domain.GetUserByUsername(ctx, s.App.Store.DB(), req.Username)
	if errors.Is(err, domain.ErrNotFound) || (err == nil && !u.MustChangePassword) {
		return errActivationFailed
	}
	if err != nil {
		return err
	}
	v, err := password.Parse(u.Verifier)
	if err != nil {
		return err
	}
	enc, macKey := activationKeys(v.Key)
	if !hmac.Equal(req.Proof, requestProof(macKey, &req)) {
		return errActivationFailed
	}
	pending, err := openVerifier(enc, req.Nonce, req.SealedVerifier)
	if err != nil {
		return errActivationFailed
	}
	evPub, err1 := hex.DecodeString(req.EventPublicKey)
	trPub, err2 := hex.DecodeString(req.TransportPublicKey)
	if err1 != nil || err2 != nil {
		return fail(http.StatusBadRequest, "public keys must be hex")
	}
	words := pairing.Words(evPub, trPub)
	err = s.App.RecordActivation(ctx, req.NodeID, domain.NodeActivated{
		UserID: u.ID, Mode: "personal", EventPublicKey: req.EventPublicKey, TransportPublicKey: req.TransportPublicKey,
		PairingWords: words, PendingVerifier: pending,
	})
	var rej *store.Rejection
	if errors.As(err, &rej) {
		return fail(http.StatusConflict, "activation refused: %s", rej.Detail)
	}
	if err != nil {
		return err
	}
	resp := ActivateResponse{
		NodeID: req.NodeID, Status: domain.NodeStatusPending, PairingWords: words,
		CentralNodeID: s.App.Author.NodeID, CentralEventPub: hex.EncodeToString(s.CentralKey.Public().(ed25519.PublicKey)),
	}
	resp.MAC = responseMAC(macKey, req.Nonce, &resp)
	s.logf("sync: Pi %s activated for %s, awaiting super-user confirmation (words: %s)", req.NodeID, u.Username, words)
	writeJSON(w, resp)
	return nil
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request, n domain.Node, _ []byte) error {
	writeJSON(w, StatusResponse{Status: n.Status})
	return nil
}

// --- sync ---

func (s *Server) quarantined(ctx context.Context, nodeID string) (bool, error) {
	_, err := s.App.Store.Config(ctx, "quarantine:"+nodeID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (s *Server) handleHello(w http.ResponseWriter, r *http.Request, n domain.Node, body []byte) error {
	ctx := r.Context()
	var req HelloRequest
	if err := readJSON(r, body, &req); err != nil {
		return err
	}
	resp := HelloResponse{
		ServerTime: s.now().UTC().Format(time.RFC3339Nano), Status: n.Status,
		Wipe: n.Status == domain.NodeStatusRevoked, KeepUnsynced: n.KeepUnsynced,
	}
	if err := s.chainOwnedBy(ctx, req.ChainID, n.ID); err != nil {
		return err
	}
	seq, h, err := s.App.Store.Head(ctx, req.ChainID)
	if err != nil {
		return err
	}
	resp.CentralSeq = seq
	if seq > 0 {
		resp.CentralHash = h.String()
	}
	if resp.DurableSeq, err = domain.DurableSeq(ctx, s.App.Store.DB(), req.ChainID); err != nil {
		return err
	}
	if err := s.App.Store.DB().QueryRowContext(ctx, `SELECT coalesce(max(local_order), 0) FROM events`).Scan(&resp.StateVersion); err != nil {
		return err
	}
	if resp.Quarantined, err = s.quarantined(ctx, n.ID); err != nil {
		return err
	}
	writeJSON(w, resp)
	return nil
}

// chainOwnedBy refuses a chain that another node already writes.
func (s *Server) chainOwnedBy(ctx context.Context, chainID, nodeID string) error {
	var owner string
	err := s.App.Store.DB().QueryRowContext(ctx, `SELECT node_id FROM events WHERE chain_id = ? LIMIT 1`, chainID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if owner != nodeID {
		return fail(http.StatusForbidden, "chain %s belongs to another node", chainID)
	}
	return nil
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request, n domain.Node, body []byte) error {
	ctx := r.Context()
	if q, err := s.quarantined(ctx, n.ID); err != nil || q {
		if err != nil {
			return err
		}
		return fail(http.StatusConflict, "node is quarantined after a chain fork; a super user must investigate")
	}
	var req EventsRequest
	if err := readJSON(r, body, &req); err != nil {
		return err
	}
	var resp EventsResponse
	for _, we := range req.Events {
		e, err := FromWire(we)
		if err != nil {
			resp.Error = fmt.Sprintf("seq %d: %v", we.Seq, err)
			break
		}
		if e.NodeID != n.ID {
			return fail(http.StatusForbidden, "nodes push only their own events")
		}
		if err := s.chainOwnedBy(ctx, e.ChainID, n.ID); err != nil {
			return err
		}
		res := EventResult{Seq: e.Seq, Status: "stored"}
		err = s.App.Store.Ingest(ctx, e)
		switch {
		case errors.Is(err, store.ErrDuplicate):
			res.Status = "duplicate"
		case errors.Is(err, store.ErrFork):
			if qerr := s.App.Store.SetConfig(ctx, "quarantine:"+n.ID, fmt.Sprintf("fork at chain %s seq %d", e.ChainID, e.Seq)); qerr != nil {
				return qerr
			}
			s.logf("sync: SECURITY: node %s forked chain %s at seq %d; quarantined", n.ID, e.ChainID, e.Seq)
			resp.Error = "fork: " + err.Error()
		case err != nil:
			s.logf("sync: node %s event seq %d refused: %v", n.ID, e.Seq, err)
			resp.Error = err.Error()
		}
		if resp.Error != "" {
			break
		}
		s.App.Author.Clock.Observe(e.HLC)
		flags, err := s.App.Store.Flags(ctx, e.EventID)
		if err != nil {
			return err
		}
		for _, f := range flags {
			res.Flags = append(res.Flags, f.Flag)
		}
		resp.Results = append(resp.Results, res)
		resp.AcceptedThrough = e.Seq
		if e.Type == event.TypePayloadRedacted {
			if err := s.App.Store.Rebuild(ctx); err != nil {
				return err
			}
		}
	}
	writeJSON(w, resp)
	return nil
}

type nodeKey struct{}

// NodeFromContext returns the authenticated Pi of a /v1/fleet/ request.
func NodeFromContext(ctx context.Context) (domain.Node, bool) {
	n, ok := ctx.Value(nodeKey{}).(domain.Node)
	return n, ok
}

// fleetLimit caps live fleet requests per Pi per minute.
const fleetLimit = 120

func (s *Server) handleFleet(w http.ResponseWriter, r *http.Request, n domain.Node, body []byte) error {
	if !s.allow("fleet:"+n.ID, fleetLimit) {
		return fail(http.StatusTooManyRequests, "too many fleet requests; slow down")
	}
	r2 := r.WithContext(context.WithValue(r.Context(), nodeKey{}, n))
	r2.Body = io.NopCloser(bytes.NewReader(body))
	s.Fleet.ServeHTTP(w, r2)
	return nil
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request, n domain.Node, _ []byte) error {
	ctx := r.Context()
	chainID := r.URL.Query().Get("chain")
	if err := s.chainOwnedBy(ctx, chainID, n.ID); err != nil {
		return err
	}
	snap, err := BuildSnapshot(ctx, s.App.Store.DB(), s.App.Author.NodeID, n, chainID, s.now())
	if err != nil {
		return err
	}
	body, sig, err := SignSnapshot(snap, s.CentralKey)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(SnapshotSignatureHeader, hex.EncodeToString(sig))
	_, err = w.Write(body)
	return err
}
