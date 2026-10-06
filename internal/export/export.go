// Package export produces accurate and complete copies of records for
// inspection (21 CFR Part 11 §11.10(b), DESIGN.md §7.1): the audit trail
// of a work order or piece of equipment as signed events, bundled with
// the public keys needed to verify them without pi-fleet's database.
package export

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"pi-fleet/internal/event"
	"pi-fleet/internal/fleetsync"
	"pi-fleet/internal/store"
)

// Format identifies the bundle layout.
const Format = "pi-fleet-export/v1"

// Bundle is a self-contained, verifiable copy of a record's history.
type Bundle struct {
	Format      string                       `json:"format"`
	GeneratedAt time.Time                    `json:"generated_at"`
	GeneratedBy string                       `json:"generated_by"`
	Source      string                       `json:"source"` // node id and role of the exporting Pi
	Subject     Subject                      `json:"subject"`
	Complete    bool                         `json:"complete"` // false on an employee Pi, which holds only its own events
	Events      []fleetsync.WireEvent        `json:"events"`
	Keys        map[string]map[string]string `json:"keys"` // node id -> key id -> hex public key
	Flags       []Flag                       `json:"flags"`
}

// Subject is what the bundle is about.
type Subject struct {
	Type  string `json:"type"` // work_order or asset
	ID    string `json:"id"`
	Label string `json:"label"`
}

// Flag is a receive-side flag on a bundled event and any decision on it.
type Flag struct {
	EventID    string `json:"event_id"`
	Flag       string `json:"flag"`
	Detail     string `json:"detail"`
	Projected  bool   `json:"projected"`
	Resolution string `json:"resolution,omitempty"`
	Note       string `json:"note,omitempty"`
}

// The events that make up a work order's record: its own events, its
// calibrations, signatures and stock issues.
const workOrderCond = `entity_id = ?1
	OR entity_id IN (SELECT id FROM calibration_records WHERE wo_id = ?1)
	OR event_id IN (SELECT id FROM signatures WHERE target_id = ?1)
	OR entity_id IN (SELECT id FROM signatures WHERE target_id = ?1)
	OR entity_id IN (SELECT txn_id FROM stock_txns WHERE wo_id = ?1)`

// An asset's record: its own events, its schedules, and the full record
// of each of its work orders and calibrations.
const assetCond = `entity_id = ?1
	OR entity_id IN (SELECT id FROM pm_schedules WHERE asset_id = ?1)
	OR entity_id IN (SELECT id FROM work_orders WHERE asset_id = ?1)
	OR entity_id IN (SELECT id FROM calibration_records WHERE asset_id = ?1)
	OR event_id IN (SELECT s.id FROM signatures s JOIN work_orders w ON w.id = s.target_id WHERE w.asset_id = ?1)
	OR entity_id IN (SELECT s.id FROM signatures s JOIN work_orders w ON w.id = s.target_id WHERE w.asset_id = ?1)
	OR entity_id IN (SELECT t.txn_id FROM stock_txns t JOIN work_orders w ON w.id = t.wo_id WHERE w.asset_id = ?1)`

// Events returns the signed events behind a record, including decisions
// on their flags and any redactions of them, in the order stored.
func Events(ctx context.Context, st *store.Store, subjectType, id string) ([]event.Event, error) {
	cond := map[string]string{"work_order": workOrderCond, "asset": assetCond}[subjectType]
	if cond == "" {
		return nil, fmt.Errorf("export: unknown subject type %q", subjectType)
	}
	evs, err := st.EventsMatching(ctx, cond, id)
	if err != nil || len(evs) == 0 {
		return evs, err
	}
	ids := make([]string, len(evs))
	for i, e := range evs {
		ids[i] = e.EventID
	}
	more, err := st.EventsMatching(ctx, `type IN ('conflict.resolved', 'payload.redacted')
		AND entity_id IN (SELECT value FROM json_each(?))`, jsonList(ids))
	if err != nil {
		return nil, err
	}
	return append(evs, more...), nil
}

func jsonList(ids []string) string {
	return `["` + strings.Join(ids, `","`) + `"]`
}

// Build assembles a bundle for the given events.
func Build(ctx context.Context, st *store.Store, subject Subject, evs []event.Event, generatedBy, source string, complete bool, now time.Time) (Bundle, error) {
	b := Bundle{Format: Format, GeneratedAt: now.UTC(), GeneratedBy: generatedBy, Source: source, Subject: subject,
		Complete: complete, Events: []fleetsync.WireEvent{}, Keys: map[string]map[string]string{}, Flags: []Flag{}}
	for _, e := range evs {
		b.Events = append(b.Events, fleetsync.ToWire(e))
		if b.Keys[e.NodeID] == nil {
			b.Keys[e.NodeID] = map[string]string{}
		}
		if _, ok := b.Keys[e.NodeID][e.KeyID]; !ok {
			pub, err := st.PublicKey(ctx, e.NodeID, e.KeyID)
			if err != nil {
				return b, err
			}
			b.Keys[e.NodeID][e.KeyID] = hex.EncodeToString(pub)
		}
		rows, err := st.DB().QueryContext(ctx, `SELECT f.flag, f.detail, f.projected, coalesce(r.resolution, ''), coalesce(r.note, '')
			FROM event_flags f LEFT JOIN flag_resolutions r USING (event_id) WHERE f.event_id = ? ORDER BY f.flag`, e.EventID)
		if err != nil {
			return b, err
		}
		for rows.Next() {
			f := Flag{EventID: e.EventID}
			if err := rows.Scan(&f.Flag, &f.Detail, &f.Projected, &f.Resolution, &f.Note); err != nil {
				rows.Close()
				return b, err
			}
			b.Flags = append(b.Flags, f)
		}
		rows.Close()
	}
	return b, nil
}

// Report is the outcome of verifying a bundle.
type Report struct {
	Events   int
	Verified int
	Redacted int
	Problems []string
	// Keys lists the signing keys used, to compare against the master
	// Pi's records of each Pi (pi-fleet nodes).
	Keys []string
}

// Verify checks every event's payload hash, hash and signature against
// the bundled keys. It needs nothing but the bundle.
func Verify(b Bundle) Report {
	var rep Report
	for node, keys := range b.Keys {
		for id := range keys {
			rep.Keys = append(rep.Keys, node+" key "+id)
		}
	}
	for _, w := range b.Events {
		rep.Events++
		e, err := fleetsync.FromWire(w)
		if err != nil {
			rep.Problems = append(rep.Problems, fmt.Sprintf("event %s: %v", w.EventID, err))
			continue
		}
		pubHex := b.Keys[e.NodeID][e.KeyID]
		pub, err := hex.DecodeString(pubHex)
		if err != nil || len(pub) != ed25519.PublicKeySize {
			rep.Problems = append(rep.Problems, fmt.Sprintf("event %s: no key %s for node %s in the bundle", e.EventID, e.KeyID, e.NodeID))
			continue
		}
		if event.KeyID(pub) != e.KeyID {
			rep.Problems = append(rep.Problems, fmt.Sprintf("event %s: bundled key does not match key id %s", e.EventID, e.KeyID))
			continue
		}
		if err := event.Verify(&e, pub); err != nil {
			rep.Problems = append(rep.Problems, fmt.Sprintf("event %s (%s): %v", e.EventID, e.Type, err))
			continue
		}
		rep.Verified++
		if e.Redacted() {
			rep.Redacted++
		}
	}
	return rep
}
