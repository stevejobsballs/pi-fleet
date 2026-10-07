package fleetsync

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"pi-fleet/internal/domain"
	"pi-fleet/internal/store"
)

// WorkingSetHorizon is how far ahead due work is included (decision D8).
const WorkingSetHorizon = 31 * 24 * time.Hour

// Snapshot is a node's working set as of one point in central's history.
type Snapshot struct {
	CentralNodeID string          `json:"central_node_id"`
	NodeID        string          `json:"node_id"`
	ChainID       string          `json:"chain_id"`
	AckedSeq      int64           `json:"acked_seq"` // the node's events up to here are reflected
	StateVersion  int64           `json:"state_version"`
	GeneratedAt   string          `json:"generated_at"`
	Tables        []SnapshotTable `json:"tables"`
	Flags         []SnapshotFlag  `json:"flags"` // central's flags on this node's events
}

// SnapshotTable holds rows of one projection table.
type SnapshotTable struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
}

// SnapshotFlag is a flag central put on one of the node's events.
type SnapshotFlag struct {
	EventID   string `json:"event_id"`
	Flag      string `json:"flag"`
	Detail    string `json:"detail"`
	Projected bool   `json:"projected"`
}

// snapshotTables lists, in load order, every table a snapshot may carry.
// A node accepts no others.
var snapshotTables = []string{
	"sites", "locations", "users", "user_password_history", "user_lockouts", "kiosks", "kiosk_members", "nodes",
	"assets", "pm_schedules", "work_orders", "wo_leases",
	"calibration_records", "cal_points", "cal_standards", "signatures", "attachments",
	"parts", "stock_locations", "stock_levels", "flag_resolutions",
	"procedures", "checklist_results", "labor_entries", "meter_readings",
}

// BuildSnapshot assembles a node's working set (DESIGN.md §5.6, decision
// D12): everything due anywhere in the fleet within the horizon, all open
// work, reference standards, all equipment at the user's home sites, the
// last three calibrations of each included asset, sites, locations, parts
// and stock, and the account data of the node's own user only.
func BuildSnapshot(ctx context.Context, db *sql.DB, centralNodeID string, node domain.Node, chainID string, now time.Time) (*Snapshot, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true}) // one consistent view
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	snap := &Snapshot{CentralNodeID: centralNodeID, NodeID: node.ID, ChainID: chainID, GeneratedAt: now.UTC().Format(time.RFC3339)}
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(seq), 0) FROM events WHERE chain_id = ? AND node_id = ?`, chainID, node.ID).Scan(&snap.AckedSeq); err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(local_order), 0) FROM events`).Scan(&snap.StateVersion); err != nil {
		return nil, err
	}

	// Who uses this Pi, and which sites are "home": its owner and their
	// home sites, or a kiosk's current members and the kiosk's site.
	people, sites := `[]`, `[]`
	if node.Mode == "kiosk" {
		if people, err = idSet(ctx, tx, `SELECT user_id FROM kiosk_members WHERE kiosk_id = ? AND removed_hlc IS NULL`, node.KioskID); err != nil {
			return nil, err
		}
		if sites, err = idSet(ctx, tx, `SELECT site_id FROM kiosks WHERE id = ?`, node.KioskID); err != nil {
			return nil, err
		}
	} else {
		people = `["` + node.BoundUserID + `"]`
		if err := tx.QueryRowContext(ctx, `SELECT home_sites FROM users WHERE id = ?`, node.BoundUserID).Scan(&sites); err != nil {
			return nil, err
		}
	}
	var members []string
	json.Unmarshal([]byte(people), &members)

	horizon := now.Add(WorkingSetHorizon).UTC().Format("2006-01-02")
	// Equipment at the user's home sites is included too, so breakdowns
	// can be logged offline against equipment that isn't due for anything.
	assets, err := idSet(ctx, tx, `
		SELECT asset_id FROM pm_schedules WHERE status = 'active' AND next_due <= ?
		UNION SELECT asset_id FROM work_orders WHERE status NOT IN ('closed', 'cancelled')
		UNION SELECT id FROM assets WHERE is_reference_standard = 1
		UNION SELECT p.asset_id FROM pm_schedules p WHERE p.status = 'active' AND p.meter != ''
			AND CAST((SELECT total FROM meter_readings m WHERE m.asset_id = p.asset_id AND m.meter = p.meter AND m.status = 'recorded'
				ORDER BY m.read_at DESC, m.id DESC LIMIT 1) AS REAL)
			>= CAST(p.meter_baseline AS REAL) + 0.8 * CAST(p.meter_interval AS REAL)
		UNION SELECT id FROM assets WHERE status != 'retired'
			AND site_id IN (SELECT value FROM json_each(?))`, horizon, sites)
	if err != nil {
		return nil, err
	}
	if assets, err = idSet(ctx, tx, withMerged, assets); err != nil {
		return nil, err
	}
	records, err := idSet(ctx, tx, `
		SELECT id FROM (
			SELECT id, row_number() OVER (PARTITION BY asset_id ORDER BY performed_at DESC, id DESC) AS rn
			FROM calibration_records WHERE status = 'valid' AND asset_id IN (SELECT value FROM json_each(?)))
		WHERE rn <= 3
		UNION SELECT c.id FROM calibration_records c JOIN work_orders w ON w.id = c.wo_id
		WHERE w.status NOT IN ('closed', 'cancelled')`, assets)
	if err != nil {
		return nil, err
	}
	workOrders, err := idSet(ctx, tx, `
		SELECT id FROM work_orders WHERE status NOT IN ('closed', 'cancelled')
		UNION SELECT wo_id FROM calibration_records WHERE id IN (SELECT value FROM json_each(?))`, records)
	if err != nil {
		return nil, err
	}
	// Assets of included work orders (closed ones referenced by history).
	if assets, err = idSet(ctx, tx, `SELECT value FROM json_each(?) UNION SELECT asset_id FROM work_orders WHERE id IN (SELECT value FROM json_each(?))`, assets, workOrders); err != nil {
		return nil, err
	}
	if assets, err = idSet(ctx, tx, withMerged, assets); err != nil {
		return nil, err
	}

	in := func(col string) string { return col + ` IN (SELECT value FROM json_each(?))` }
	specs := []struct {
		name, where string
		args        []any
	}{
		{"sites", "1", nil},
		{"locations", "1", nil},
		{"users", "1", nil},
		{"user_password_history", in("user_id"), []any{people}},
		{"user_lockouts", in("user_id"), []any{people}},
		{"kiosks", "id = ?", []any{node.KioskID}},
		{"kiosk_members", "kiosk_id = ?", []any{node.KioskID}},
		{"nodes", "id = ?", []any{node.ID}},
		{"assets", in("id"), []any{assets}},
		{"pm_schedules", in("asset_id"), []any{assets}},
		{"work_orders", in("id"), []any{workOrders}},
		{"wo_leases", in("wo_id"), []any{workOrders}},
		{"calibration_records", in("id"), []any{records}},
		{"cal_points", in("record_id"), []any{records}},
		{"cal_standards", in("record_id"), []any{records}},
		{"signatures", "target_type = 'work_order' AND " + in("target_id"), []any{workOrders}},
		{"attachments", "(target_type = 'work_order' AND " + in("target_id") + ") OR (target_type = 'asset' AND " + in("target_id") + ")",
			[]any{workOrders, assets}},
		{"parts", "1", nil},
		{"stock_locations", "1", nil},
		{"stock_levels", "1", nil},
		{"procedures", "1", nil},
		{"checklist_results", in("wo_id"), []any{workOrders}},
		{"labor_entries", in("wo_id"), []any{workOrders}},
		// Recent readings of included equipment; the oldest carries
		// central's running total, so new readings on the Pi add up right.
		{"meter_readings", `id IN (SELECT id FROM (SELECT id, row_number() OVER (PARTITION BY asset_id, meter
			ORDER BY read_at DESC, id DESC) AS rn FROM meter_readings WHERE status = 'recorded' AND ` + in("asset_id") + `) WHERE rn <= 20)`,
			[]any{assets}},
		{"flag_resolutions", "event_id IN (SELECT event_id FROM events WHERE node_id = ?)", []any{node.ID}},
	}
	for _, sp := range specs {
		t, err := dumpTable(ctx, tx, sp.name, sp.where, sp.args...)
		if err != nil {
			return nil, fmt.Errorf("snapshot %s: %w", sp.name, err)
		}
		switch sp.name {
		case "users": // only this Pi's users' verifiers leave central
			blankColumnUnless(&t, "verifier", "id", members...)
		case "nodes":
			blankColumnUnless(&t, "pending_verifier", "id")
		case "kiosks":
			blankColumnUnless(&t, "activation_verifier", "id")
		}
		snap.Tables = append(snap.Tables, t)
	}

	rows, err := tx.QueryContext(ctx, `SELECT f.event_id, f.flag, f.detail, f.projected
		FROM event_flags f JOIN events e USING (event_id) WHERE e.node_id = ? ORDER BY f.event_id, f.flag`, node.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var f SnapshotFlag
		if err := rows.Scan(&f.EventID, &f.Flag, &f.Detail, &f.Projected); err != nil {
			return nil, err
		}
		snap.Flags = append(snap.Flags, f)
	}
	return snap, rows.Err()
}

// withMerged adds to a set of asset ids the records merged into them and
// the records they were merged into, so a Pi shows a kept record with its
// merged history, and work against a merged record leads to the kept one.
const withMerged = `SELECT value FROM json_each(?1)
	UNION SELECT id FROM assets WHERE merged_into IN (SELECT value FROM json_each(?1))
	UNION SELECT merged_into FROM assets WHERE merged_into != '' AND id IN (SELECT value FROM json_each(?1))`

// idSet runs a query returning one id column and gives the ids as a JSON
// array, ready to pass to json_each.
func idSet(ctx context.Context, tx *sql.Tx, query string, args ...any) (string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	b, _ := json.Marshal(ids)
	return string(b), rows.Err()
}

func dumpTable(ctx context.Context, tx *sql.Tx, name, where string, args ...any) (SnapshotTable, error) {
	rows, err := tx.QueryContext(ctx, `SELECT * FROM `+name+` WHERE `+where, args...)
	if err != nil {
		return SnapshotTable{}, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return SnapshotTable{}, err
	}
	t := SnapshotTable{Name: name, Columns: cols, Rows: [][]any{}}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return SnapshotTable{}, err
		}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				vals[i] = map[string]string{"hex": hex.EncodeToString(b)}
			}
		}
		t.Rows = append(t.Rows, vals)
	}
	return t, rows.Err()
}

// blankColumnUnless empties column col in every row whose keyCol is not
// one of keep.
func blankColumnUnless(t *SnapshotTable, col, keyCol string, keep ...string) {
	ci, ki := -1, -1
	for i, c := range t.Columns {
		switch c {
		case col:
			ci = i
		case keyCol:
			ki = i
		}
	}
	for _, r := range t.Rows {
		if !slices.Contains(keep, fmt.Sprint(r[ki])) {
			r[ci] = ""
		}
	}
}

// SignSnapshot encodes a snapshot and signs the exact bytes.
func SignSnapshot(s *Snapshot, key ed25519.PrivateKey) (body, sig []byte, err error) {
	if body, err = json.Marshal(s); err != nil {
		return nil, nil, err
	}
	return body, ed25519.Sign(key, snapshotDigest(body)), nil
}

func snapshotDigest(body []byte) []byte {
	sum := sha256.Sum256(append([]byte(snapshotDomain), body...))
	return sum[:]
}

// ErrBadSnapshot means a snapshot failed verification.
var ErrBadSnapshot = errors.New("fleetsync: snapshot signature or contents invalid")

// ApplySnapshot verifies a snapshot from central and rebases this node
// onto it: projections are replaced by the snapshot's state and the
// node's own events after AckedSeq are re-applied on top.
func ApplySnapshot(ctx context.Context, st *store.Store, body, sig []byte, centralPub ed25519.PublicKey, nodeID string) (*Snapshot, error) {
	if !ed25519.Verify(centralPub, snapshotDigest(body), sig) {
		return nil, ErrBadSnapshot
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var snap Snapshot
	if err := dec.Decode(&snap); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadSnapshot, err)
	}
	if snap.NodeID != nodeID {
		return nil, fmt.Errorf("%w: snapshot is for node %s", ErrBadSnapshot, snap.NodeID)
	}
	load := func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `PRAGMA defer_foreign_keys = ON`); err != nil {
			return err
		}
		allowed := map[string]bool{}
		for _, t := range snapshotTables {
			allowed[t] = true
		}
		for _, t := range snap.Tables {
			if err := loadTable(ctx, tx, t, allowed); err != nil {
				return fmt.Errorf("snapshot table %s: %w", t.Name, err)
			}
		}
		for _, f := range snap.Flags {
			if err := store.Flag(ctx, tx, f.EventID, f.Flag, f.Detail, f.Projected); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO ws_snapshot (singleton, body, sig, received_at) VALUES (1, ?, ?, ?)
			ON CONFLICT (singleton) DO UPDATE SET body = excluded.body, sig = excluded.sig, received_at = excluded.received_at`,
			body, sig, time.Now().UTC().Format(time.RFC3339))
		return err
	}
	if err := st.Rebase(ctx, load, snap.ChainID, snap.AckedSeq); err != nil {
		return nil, err
	}
	return &snap, nil
}

// ReapplyStoredSnapshot rebuilds a node's projections from the last
// snapshot it received plus its own unacknowledged events.
func ReapplyStoredSnapshot(ctx context.Context, st *store.Store, centralPub ed25519.PublicKey, nodeID string) error {
	var body, sig []byte
	err := st.DB().QueryRowContext(ctx, `SELECT body, sig FROM ws_snapshot WHERE singleton = 1`).Scan(&body, &sig)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("fleetsync: no snapshot received yet")
	}
	if err != nil {
		return err
	}
	_, err = ApplySnapshot(ctx, st, body, sig, centralPub, nodeID)
	return err
}

func loadTable(ctx context.Context, tx *sql.Tx, t SnapshotTable, allowed map[string]bool) error {
	if !allowed[t.Name] {
		return errors.New("table not allowed in a snapshot")
	}
	actual := map[string]bool{}
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, t.Name)
	if err != nil {
		return err
	}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			rows.Close()
			return err
		}
		actual[c] = true
	}
	rows.Close()
	if len(t.Columns) == 0 {
		return errors.New("no columns")
	}
	for _, c := range t.Columns {
		if !actual[c] {
			return fmt.Errorf("unknown column %q", c)
		}
	}
	query := `INSERT INTO ` + t.Name + ` (` + strings.Join(t.Columns, ", ") + `) VALUES (` +
		strings.TrimSuffix(strings.Repeat("?, ", len(t.Columns)), ", ") + `)`
	for _, r := range t.Rows {
		if len(r) != len(t.Columns) {
			return errors.New("row width does not match columns")
		}
		args := make([]any, len(r))
		for i, v := range r {
			if args[i], err = sqlValue(v); err != nil {
				return fmt.Errorf("column %s: %w", t.Columns[i], err)
			}
		}
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return err
		}
	}
	return nil
}

func sqlValue(v any) (any, error) {
	switch x := v.(type) {
	case nil, string:
		return x, nil
	case json.Number:
		return strconv.ParseInt(string(x), 10, 64)
	case map[string]any:
		h, ok := x["hex"].(string)
		if !ok || len(x) != 1 {
			return nil, errors.New("unexpected object")
		}
		return hex.DecodeString(h)
	}
	return nil, fmt.Errorf("unexpected value of type %T", v)
}
