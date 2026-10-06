// Package store is the SQLite-backed, append-only event log shared by
// nodes and central (DESIGN.md §4).
//
// Writes go through a single connection, which serialises appends so each
// chain's seq and prev_hash are assigned without races. Reads use a
// separate query-only pool.
package store

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"pi-fleet/internal/event"
	"pi-fleet/internal/hlc"
)

//go:embed migrations/*.sql
var migrations embed.FS

var (
	ErrDuplicate  = errors.New("store: event already stored")
	ErrFork       = errors.New("store: a different event already occupies this chain position")
	ErrGap        = errors.New("store: event does not follow the chain head")
	ErrUnknownKey = errors.New("store: signing key is not trusted for this node")
	ErrNotFound   = errors.New("store: not found")
	ErrRedacted   = errors.New("store: payload already redacted")
)

// Store is an open event database.
type Store struct {
	w *sql.DB // single writer connection
	r *sql.DB // query-only readers
}

// Open opens or creates the database at path and applies migrations.
func Open(path string) (*Store, error) {
	w, err := sql.Open("sqlite", dsn(path, "_txlock=immediate"))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	if err := migrate(w); err != nil {
		w.Close()
		return nil, err
	}
	r, err := sql.Open("sqlite", dsn(path, "_pragma=query_only(1)"))
	if err != nil {
		w.Close()
		return nil, err
	}
	return &Store{w: w, r: r}, nil
}

func dsn(path, extra string) string {
	q := strings.Join([]string{
		"_pragma=journal_mode(WAL)",
		"_pragma=synchronous(FULL)",
		"_pragma=foreign_keys(1)",
		"_pragma=busy_timeout(5000)",
		extra,
	}, "&")
	return (&url.URL{Scheme: "file", OmitHost: true, Path: path, RawQuery: q}).String()
}

// Close closes the database.
func (s *Store) Close() error {
	return errors.Join(s.r.Close(), s.w.Close())
}

func migrate(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		version := strings.TrimSuffix(strings.TrimPrefix(name, "migrations/"), ".sql")
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE version = ?`, version).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		body, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migration %s: %w", version, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`,
			version, time.Now().UTC().Format(time.RFC3339)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// LocalNode is this installation's identity and current chain.
type LocalNode struct {
	NodeID  string
	ChainID string
}

// LocalNode returns the stored identity, or ErrNotFound before init.
func (s *Store) LocalNode(ctx context.Context) (LocalNode, error) {
	var n LocalNode
	err := s.r.QueryRowContext(ctx, `SELECT node_id, chain_id FROM local_node WHERE singleton = 1`).Scan(&n.NodeID, &n.ChainID)
	if errors.Is(err, sql.ErrNoRows) {
		return LocalNode{}, ErrNotFound
	}
	return n, err
}

// SetLocalNode records this installation's identity and current chain.
func (s *Store) SetLocalNode(ctx context.Context, n LocalNode) error {
	_, err := s.w.ExecContext(ctx, `INSERT INTO local_node (singleton, node_id, chain_id) VALUES (1, ?, ?)
		ON CONFLICT (singleton) DO UPDATE SET node_id = excluded.node_id, chain_id = excluded.chain_id`,
		n.NodeID, n.ChainID)
	return err
}

// TrustKey records pub as a valid event-signing key for nodeID.
func (s *Store) TrustKey(ctx context.Context, nodeID string, pub ed25519.PublicKey) error {
	_, err := s.w.ExecContext(ctx, `INSERT INTO node_keys (node_id, key_id, public_key, added_at)
		VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		nodeID, event.KeyID(pub), []byte(pub), time.Now().UTC().Format(time.RFC3339))
	return err
}

// PublicKey returns a trusted key, or ErrUnknownKey.
func (s *Store) PublicKey(ctx context.Context, nodeID, keyID string) (ed25519.PublicKey, error) {
	return publicKey(ctx, s.r, nodeID, keyID)
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func publicKey(ctx context.Context, q querier, nodeID, keyID string) (ed25519.PublicKey, error) {
	var pub []byte
	err := q.QueryRowContext(ctx, `SELECT public_key FROM node_keys WHERE node_id = ? AND key_id = ?`, nodeID, keyID).Scan(&pub)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: node %s key %s", ErrUnknownKey, nodeID, keyID)
	}
	return ed25519.PublicKey(pub), err
}

// Head returns the last seq and hash of a chain; (0, zero hash) if empty.
func (s *Store) Head(ctx context.Context, chainID string) (int64, event.Hash, error) {
	return head(ctx, s.r, chainID)
}

func head(ctx context.Context, q querier, chainID string) (int64, event.Hash, error) {
	var seq int64
	var h []byte
	err := q.QueryRowContext(ctx, `SELECT seq, hash FROM events WHERE chain_id = ? ORDER BY seq DESC LIMIT 1`, chainID).Scan(&seq, &h)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, event.Hash{}, nil
	}
	if err != nil {
		return 0, event.Hash{}, err
	}
	return seq, event.Hash(h), nil
}

// MaxHLC returns the highest HLC stored, for seeding a clock at startup.
func (s *Store) MaxHLC(ctx context.Context) (int64, error) {
	var v sql.NullInt64
	err := s.r.QueryRowContext(ctx, `SELECT max(hlc) FROM events`).Scan(&v)
	return v.Int64, err
}

// Author is everything needed to write events as this node.
type Author struct {
	NodeID     string
	ChainID    string
	Signer     event.Signer
	Clock      *hlc.Clock
	Now        func() time.Time        // defaults to time.Now
	ClockState func() event.ClockState // defaults to unverified
}

func (a *Author) position(seq int64, prev event.Hash) event.Position {
	now, state := time.Now, func() event.ClockState { return event.ClockUnverified }
	if a.Now != nil {
		now = a.Now
	}
	if a.ClockState != nil {
		state = a.ClockState
	}
	return event.Position{
		NodeID:     a.NodeID,
		ChainID:    a.ChainID,
		Seq:        seq,
		PrevHash:   prev,
		HLC:        a.Clock.Now(),
		WallTime:   now(),
		ClockState: state(),
	}
}

// Append seals d as the next event in the author's chain and stores it.
func (s *Store) Append(ctx context.Context, a *Author, d event.Draft) (event.Event, error) {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return event.Event{}, err
	}
	defer tx.Rollback()
	e, err := appendTx(ctx, tx, a, d)
	if err != nil {
		return event.Event{}, err
	}
	return e, tx.Commit()
}

func appendTx(ctx context.Context, tx *sql.Tx, a *Author, d event.Draft) (event.Event, error) {
	seq, prev, err := head(ctx, tx, a.ChainID)
	if err != nil {
		return event.Event{}, err
	}
	e, err := event.Seal(d, a.position(seq+1, prev), a.Signer)
	if err != nil {
		return event.Event{}, err
	}
	return e, insert(ctx, tx, &e)
}

// Ingest verifies an event received from another node and appends it to
// that node's chain. A validly signed event that already occupies its
// position returns ErrDuplicate; a different one there returns ErrFork.
func (s *Store) Ingest(ctx context.Context, e event.Event) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	pub, err := publicKey(ctx, tx, e.NodeID, e.KeyID)
	if err != nil {
		return err
	}
	if err := event.Verify(&e, pub); err != nil {
		return err
	}
	var existing []byte
	err = tx.QueryRowContext(ctx, `SELECT hash FROM events WHERE chain_id = ? AND seq = ?`, e.ChainID, e.Seq).Scan(&existing)
	switch {
	case err == nil && event.Hash(existing) == e.Hash:
		return ErrDuplicate
	case err == nil:
		return fmt.Errorf("%w: chain %s seq %d", ErrFork, e.ChainID, e.Seq)
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	seq, prev, err := head(ctx, tx, e.ChainID)
	if err != nil {
		return err
	}
	if err := event.VerifyLink(seq, prev, &e); err != nil {
		return fmt.Errorf("%w: head is %d: %v", ErrGap, seq, err)
	}
	if err := insert(ctx, tx, &e); err != nil {
		return err
	}
	if e.Type == event.TypePayloadRedacted && e.EntityType == event.EntityEvent {
		if _, err := tx.ExecContext(ctx, `UPDATE events SET payload = NULL WHERE event_id = ? AND payload IS NOT NULL`, e.EntityID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Redact records a payload.redacted event for targetEventID and removes
// that event's payload. The target's payload_hash, and so every chain
// hash, is unchanged (DESIGN.md §3.6). Authorisation is the caller's job.
func (s *Store) Redact(ctx context.Context, a *Author, actorUserID, actorSessionID, targetEventID, reason string) (event.Event, error) {
	if strings.TrimSpace(reason) == "" {
		return event.Event{}, errors.New("store: redaction requires a reason")
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return event.Event{}, err
	}
	defer tx.Rollback()

	var payload, payloadHash []byte
	err = tx.QueryRowContext(ctx, `SELECT payload, payload_hash FROM events WHERE event_id = ?`, targetEventID).Scan(&payload, &payloadHash)
	if errors.Is(err, sql.ErrNoRows) {
		return event.Event{}, ErrNotFound
	}
	if err != nil {
		return event.Event{}, err
	}
	if payload == nil {
		return event.Event{}, ErrRedacted
	}
	body, err := json.Marshal(map[string]string{
		"reason":              reason,
		"target_payload_hash": event.Hash(payloadHash).String(),
	})
	if err != nil {
		return event.Event{}, err
	}
	e, err := appendTx(ctx, tx, a, event.Draft{
		ActorUserID:    actorUserID,
		ActorSessionID: actorSessionID,
		Type:           event.TypePayloadRedacted,
		EntityType:     event.EntityEvent,
		EntityID:       targetEventID,
		SchemaVersion:  1,
		Payload:        body,
	})
	if err != nil {
		return event.Event{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE events SET payload = NULL WHERE event_id = ?`, targetEventID); err != nil {
		return event.Event{}, err
	}
	return e, tx.Commit()
}

const eventColumns = `chain_id, seq, event_id, node_id, prev_hash, hlc, wall_time, clock_state,
	actor_user_id, actor_session_id, type, entity_type, entity_id, base_version, lease_id,
	schema_version, payload, payload_hash, hash, sig, key_id`

func insert(ctx context.Context, tx *sql.Tx, e *event.Event) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO events (`+eventColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ChainID, e.Seq, e.EventID, e.NodeID, e.PrevHash[:], e.HLC,
		e.WallTime.UTC().Format(event.WallTimeLayout), string(e.ClockState),
		e.ActorUserID, e.ActorSessionID, e.Type, e.EntityType, e.EntityID,
		e.BaseVersion, e.LeaseID, e.SchemaVersion, e.Payload,
		e.PayloadHash[:], e.Hash[:], e.Sig, e.KeyID)
	return err
}

type rowScanner interface{ Scan(dest ...any) error }

func scanEvent(row rowScanner) (event.Event, error) {
	var (
		e                       event.Event
		prev, payloadHash, hash []byte
		wall, clockState        string
	)
	err := row.Scan(&e.ChainID, &e.Seq, &e.EventID, &e.NodeID, &prev, &e.HLC, &wall, &clockState,
		&e.ActorUserID, &e.ActorSessionID, &e.Type, &e.EntityType, &e.EntityID, &e.BaseVersion,
		&e.LeaseID, &e.SchemaVersion, &e.Payload, &payloadHash, &hash, &e.Sig, &e.KeyID)
	if err != nil {
		return event.Event{}, err
	}
	e.WallTime, err = time.Parse(event.WallTimeLayout, wall)
	if err != nil {
		return event.Event{}, fmt.Errorf("store: event %s wall_time: %w", e.EventID, err)
	}
	e.ClockState = event.ClockState(clockState)
	e.PrevHash, e.PayloadHash, e.Hash = event.Hash(prev), event.Hash(payloadHash), event.Hash(hash)
	return e, nil
}

// Event returns one event by id.
func (s *Store) Event(ctx context.Context, eventID string) (event.Event, error) {
	e, err := scanEvent(s.r.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM events WHERE event_id = ?`, eventID))
	if errors.Is(err, sql.ErrNoRows) {
		return event.Event{}, ErrNotFound
	}
	return e, err
}

// Chain returns up to limit events of a chain starting at fromSeq.
func (s *Store) Chain(ctx context.Context, chainID string, fromSeq int64, limit int) ([]event.Event, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT `+eventColumns+` FROM events
		WHERE chain_id = ? AND seq >= ? ORDER BY seq LIMIT ?`, chainID, fromSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []event.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Problem is one verification failure.
type Problem struct {
	ChainID string
	Seq     int64
	EventID string
	Err     error
}

func (p Problem) String() string {
	return fmt.Sprintf("chain %s seq %d (event %s): %v", p.ChainID, p.Seq, p.EventID, p.Err)
}

// Report summarises a full verification pass.
type Report struct {
	Chains   int
	Events   int
	Redacted int
	Problems []Problem
}

// OK reports whether verification found no problems.
func (r Report) OK() bool { return len(r.Problems) == 0 }

// Verify re-walks every chain from its first event, checking signatures,
// hashes, payloads and links (DESIGN.md §5.8).
func (s *Store) Verify(ctx context.Context) (Report, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT `+eventColumns+` FROM events ORDER BY chain_id, seq`)
	if err != nil {
		return Report{}, err
	}
	defer rows.Close()

	var (
		rep      Report
		chain    string
		prevSeq  int64
		prevHash event.Hash
		keyCache = map[[2]string]ed25519.PublicKey{}
	)
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return rep, err
		}
		if e.ChainID != chain {
			chain, prevSeq, prevHash = e.ChainID, 0, event.Hash{}
			rep.Chains++
		}
		rep.Events++
		if e.Redacted() {
			rep.Redacted++
		}
		problem := func(err error) { rep.Problems = append(rep.Problems, Problem{e.ChainID, e.Seq, e.EventID, err}) }

		if err := event.VerifyLink(prevSeq, prevHash, &e); err != nil {
			problem(err)
		}
		prevSeq, prevHash = e.Seq, e.Hash

		k := [2]string{e.NodeID, e.KeyID}
		pub, ok := keyCache[k]
		if !ok {
			pub, err = s.PublicKey(ctx, e.NodeID, e.KeyID)
			if err != nil && !errors.Is(err, ErrUnknownKey) {
				return rep, err
			}
			keyCache[k] = pub
		}
		if pub == nil {
			problem(fmt.Errorf("%w: node %s key %s", ErrUnknownKey, e.NodeID, e.KeyID))
			continue
		}
		if err := event.Verify(&e, pub); err != nil {
			problem(err)
		}
	}
	return rep, rows.Err()
}
