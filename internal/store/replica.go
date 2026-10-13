package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"pi-fleet/internal/event"
)

// A standby Pi keeps a copy of the master Pi's database: it starts from a
// snapshot, then copies the master's events in the master's own order
// (local_order) and applies them, so its projections match the master's.

// ErrOutOfStep is returned when copied events don't continue the copy.
var ErrOutOfStep = errors.New("store: copied events don't follow on from this copy")

// LastLocalOrder is the position of the last event stored (0 if none).
func (s *Store) LastLocalOrder(ctx context.Context) (int64, error) {
	var n int64
	err := s.r.QueryRowContext(ctx, `SELECT coalesce(max(local_order), 0) FROM events`).Scan(&n)
	return n, err
}

// Replicate stores events copied from the master Pi at the positions they
// have there, each checked as Ingest checks it (a trusted signature, and
// following its chain), and applies them. A redaction (or an event copied
// already redacted) is followed by a Rebuild, as on the master.
func (s *Store) Replicate(ctx context.Context, evs []event.Event, orders []int64) error {
	if len(evs) != len(orders) {
		return errors.New("store: events and positions don't match")
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rebuild := false
	for i := range evs {
		e := &evs[i]
		var last int64
		if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(local_order), 0) FROM events`).Scan(&last); err != nil {
			return err
		}
		if orders[i] != last+1 {
			return fmt.Errorf("%w: next is %d, got %d", ErrOutOfStep, last+1, orders[i])
		}
		pub, err := publicKey(ctx, tx, e.NodeID, e.KeyID)
		if err != nil {
			return fmt.Errorf("event %s: %w", e.EventID, err)
		}
		if err := event.Verify(e, pub); err != nil {
			return fmt.Errorf("event %s: %w", e.EventID, err)
		}
		seq, prev, err := head(ctx, tx, e.ChainID)
		if err != nil {
			return err
		}
		if err := event.VerifyLink(seq, prev, e); err != nil {
			return fmt.Errorf("%w: event %s: %v", ErrOutOfStep, e.EventID, err)
		}
		if err := insert(ctx, tx, e); err != nil {
			return err
		}
		if e.Redacted() {
			rebuild = true // its replacement comes with its redaction, later
		} else if err := s.apply(ctx, tx, e); err != nil {
			return err
		}
		if e.Type == event.TypePayloadRedacted && e.EntityType == event.EntityEvent {
			if _, err := tx.ExecContext(ctx, `UPDATE events SET payload = NULL WHERE event_id = ? AND payload IS NOT NULL`, e.EntityID); err != nil {
				return err
			}
			rebuild = true
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if rebuild {
		return s.Rebuild(ctx)
	}
	return nil
}

// ReplicaHead is the position and hash of the last event stored, to
// compare a copy with the master.
func (s *Store) ReplicaHead(ctx context.Context) (int64, []byte, error) {
	var n int64
	var h []byte
	err := s.r.QueryRowContext(ctx, `SELECT local_order, hash FROM events ORDER BY local_order DESC LIMIT 1`).Scan(&n, &h)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil, nil
	}
	return n, h, err
}
