package domain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"time"

	"pi-fleet/internal/store"
)

// Meter event types (DESIGN.md §4.3–4.4).
const (
	TypeMeterRead   = "meter.read"
	TypeMeterVoided = "meter.voided"
	EntityMeter     = "meter_reading"

	// FlagMeterInconsistent marks a reading that is higher than a later
	// one (often a backdated or mistyped reading).
	FlagMeterInconsistent = "meter_inconsistent"
)

var meterNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// MeterRead is a reading of one of an asset's meters (hours, cycles...).
// Reset marks the first reading after the meter was replaced or reset.
type MeterRead struct {
	AssetID string `json:"asset_id"`
	Meter   string `json:"meter"`
	Value   string `json:"value"`   // decimal
	ReadAt  string `json:"read_at"` // RFC 3339
	Reset   bool   `json:"reset"`
}

type MeterVoided struct {
	Reason string `json:"reason"`
}

func init() {
	payloadTypes[TypeMeterRead] = struct {
		entity string
		new    func() any
	}{EntityMeter, func() any { return &MeterRead{} }}
	payloadTypes[TypeMeterVoided] = struct {
		entity string
		new    func() any
	}{EntityMeter, func() any { return &MeterVoided{} }}
}

type reading struct {
	id, value, readAt, total string
	reset                    bool
}

// meterReadings returns an asset meter's recorded readings in reading
// order (time, then id, so every Pi orders them the same way).
func meterReadings(ctx context.Context, q Querier, assetID, meter string) ([]reading, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, value, read_at, total, reset FROM meter_readings
		WHERE asset_id = ? AND meter = ? AND status = 'recorded' ORDER BY read_at, id`, assetID, meter)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []reading
	for rows.Next() {
		var r reading
		if err := rows.Scan(&r.id, &r.value, &r.readAt, &r.total, &r.reset); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// recomputeTotals rewrites the cumulative usage of a meter's readings.
// Usage accrues as the difference between consecutive readings; a reset
// reading counts its whole value (the new meter started at zero). The
// first reading anchors the total: on central that is the first reading
// ever (total 0); on a Pi it is the oldest reading in its snapshot, which
// carries central's total.
func (ap *applier) recomputeTotals(assetID, meter string) error {
	rs, err := meterReadings(ap.ctx, ap.tx, assetID, meter)
	if err != nil || len(rs) == 0 {
		return err
	}
	anchor := orZero(rs[0].total)
	if ap.p.CentralNodeID == "" {
		anchor = "0" // central holds every reading: the first one starts at zero
	}
	total, err := parseDecimal(anchor)
	if err != nil {
		return err
	}
	prev, _ := parseDecimal(rs[0].value)
	for i, r := range rs {
		v, _ := parseDecimal(r.value)
		if i > 0 {
			inc := new(big.Rat).Sub(v, prev)
			if r.reset {
				inc = v
			}
			if inc.Sign() > 0 {
				total = new(big.Rat).Add(total, inc)
			}
		}
		prev = v
		if t := formatLimit(total); t != r.total {
			if err := ap.exec(`UPDATE meter_readings SET total = ? WHERE id = ?`, t, r.id); err != nil {
				return err
			}
		}
	}
	return nil
}

func orZero(s string) string {
	if s == "" {
		return "0"
	}
	return s
}

func (ap *applier) meterRead(p *MeterRead) error {
	if err := ap.require(RoleUser); err != nil {
		return err
	}
	if err := ap.newEntity("meter_readings"); err != nil {
		return err
	}
	if !meterNameRE.MatchString(p.Meter) {
		return invalid("meter name %q must be lowercase letters, digits or _ (e.g. hours, cycles)", p.Meter)
	}
	v, err := parseDecimal(p.Value)
	if err != nil || v.Sign() < 0 {
		return invalid("reading %q must be a non-negative decimal", p.Value)
	}
	at, err := time.Parse(time.RFC3339, p.ReadAt)
	if err != nil {
		return invalid("read_at %q must be an RFC 3339 time", p.ReadAt)
	}
	if at.After(ap.e.WallTime.Add(5 * time.Minute)) {
		return invalid("a reading can't be in the future")
	}
	if a, err := GetAsset(ap.ctx, ap.tx, p.AssetID); errors.Is(err, ErrNotFound) {
		return invalid("asset %s not found", p.AssetID)
	} else if err != nil {
		return err
	} else if a.Status == AssetRetired {
		return invalid("asset %s is retired", a.Tag)
	}
	readAt := at.UTC().Format(time.RFC3339)

	// Readings only go up, unless the meter was reset or replaced.
	rs, err := meterReadings(ap.ctx, ap.tx, p.AssetID, p.Meter)
	if err != nil {
		return err
	}
	var before, after *reading
	for i := range rs {
		if rs[i].readAt < readAt || (rs[i].readAt == readAt && rs[i].id < ap.e.EntityID) {
			before = &rs[i]
		} else if after == nil {
			after = &rs[i]
		}
	}
	if before != nil && !p.Reset {
		pv, _ := parseDecimal(before.value)
		if v.Cmp(pv) < 0 {
			return invalid("%s is lower than the reading of %s at %s; tick \"meter was reset or replaced\" if it was", p.Value, before.value, before.readAt)
		}
	}
	if err := ap.exec(`INSERT INTO meter_readings (id, asset_id, meter, value, read_at, reset, total, recorded_by, status, void_reason, last_event_id)
		VALUES (?, ?, ?, ?, ?, ?, '', ?, 'recorded', '', ?)`, ap.e.EntityID, p.AssetID, p.Meter, p.Value, readAt, p.Reset, ap.actor.id, ap.e.EventID); err != nil {
		return err
	}
	if before == nil && after != nil && after.total != "" {
		// Now the earliest reading: anchor it so the totals that follow
		// keep their values.
		if err := ap.exec(`UPDATE meter_readings SET total = ? WHERE id = ?`, after.total, ap.e.EntityID); err != nil {
			return err
		}
	}
	if after != nil && !after.reset {
		if av, _ := parseDecimal(after.value); v.Cmp(av) > 0 {
			if err := ap.warn(FlagMeterInconsistent, "%s at %s is higher than the later reading of %s at %s", p.Value, readAt, after.value, after.readAt); err != nil {
				return err
			}
		}
	}
	return ap.recomputeTotals(p.AssetID, p.Meter)
}

func (ap *applier) meterVoided(p *MeterVoided) error {
	var assetID, meter, by, status string
	err := ap.tx.QueryRowContext(ap.ctx, `SELECT asset_id, meter, recorded_by, status FROM meter_readings WHERE id = ?`, ap.e.EntityID).Scan(&assetID, &meter, &by, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return invalid("meter reading %s not found", ap.e.EntityID)
	}
	if err != nil {
		return err
	}
	if by != ap.actor.id && !ap.actor.atLeast(RoleMidTier) {
		return store.Reject(FlagNotAuthorized, "only the person who recorded a reading, or a mid-tier user, can void it")
	}
	if status != "recorded" || blank(p.Reason) {
		return invalid("voiding a reading needs a reason, once")
	}
	if err := ap.exec(`UPDATE meter_readings SET status = 'voided', void_reason = ?, last_event_id = ? WHERE id = ?`, p.Reason, ap.e.EventID, ap.e.EntityID); err != nil {
		return err
	}
	return ap.recomputeTotals(assetID, meter)
}

// MeterTotal returns a meter's current cumulative usage and its latest
// reading ("0" and "" when it has none).
func MeterTotal(ctx context.Context, q Querier, assetID, meter string) (total, latest string, err error) {
	err = q.QueryRowContext(ctx, `SELECT total, value FROM meter_readings WHERE asset_id = ? AND meter = ? AND status = 'recorded'
		ORDER BY read_at DESC, id DESC LIMIT 1`, assetID, meter).Scan(&total, &latest)
	if errors.Is(err, sql.ErrNoRows) {
		return "0", "", nil
	}
	return total, latest, err
}

// Meter is a meter's latest state, for display.
type Meter struct {
	Name, Latest, Total, ReadAt string
}

// AssetMeters lists an asset's meters with their latest readings.
func AssetMeters(ctx context.Context, q Querier, assetID string) ([]Meter, error) {
	names, err := queryStrings(ctx, q, `SELECT DISTINCT meter FROM meter_readings WHERE asset_id = ? AND status = 'recorded' ORDER BY meter`, assetID)
	if err != nil {
		return nil, err
	}
	out := make([]Meter, 0, len(names))
	for _, n := range names {
		m := Meter{Name: n}
		if err := q.QueryRowContext(ctx, `SELECT value, total, read_at FROM meter_readings WHERE asset_id = ? AND meter = ? AND status = 'recorded'
			ORDER BY read_at DESC, id DESC LIMIT 1`, assetID, n).Scan(&m.Latest, &m.Total, &m.ReadAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// MeterReadingRow is one reading, for display.
type MeterReadingRow struct {
	ID, Meter, Value, ReadAt, Total, RecordedBy, RecordedByID, Status, VoidReason string
	Reset                                                                         bool
}

// RecentMeterReadings lists an asset's latest readings.
func RecentMeterReadings(ctx context.Context, q Querier, assetID string, limit int) ([]MeterReadingRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT m.id, m.meter, m.value, m.read_at, m.total, coalesce(u.legal_name, m.recorded_by), m.recorded_by,
		m.status, m.void_reason, m.reset FROM meter_readings m LEFT JOIN users u ON u.id = m.recorded_by
		WHERE m.asset_id = ? ORDER BY m.read_at DESC, m.id DESC LIMIT ?`, assetID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MeterReadingRow
	for rows.Next() {
		var r MeterReadingRow
		if err := rows.Scan(&r.ID, &r.Meter, &r.Value, &r.ReadAt, &r.Total, &r.RecordedBy, &r.RecordedByID, &r.Status, &r.VoidReason, &r.Reset); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MeterStatus describes a schedule's usage trigger.
type MeterStatus struct {
	Meter          string
	Used, Interval string // usage since the last completion, and the interval
	Due            bool   // within the lead of the interval
	Remaining      string
	Configured     bool
}

// ScheduleMeterStatus reports how close a schedule is to its usage
// trigger.
func ScheduleMeterStatus(ctx context.Context, q Querier, s Schedule) (MeterStatus, error) {
	st := MeterStatus{Meter: s.Meter, Interval: s.MeterInterval, Configured: s.Meter != ""}
	if !st.Configured {
		return st, nil
	}
	total, _, err := MeterTotal(ctx, q, s.AssetID, s.Meter)
	if err != nil {
		return st, err
	}
	t, err1 := parseDecimal(total)
	base, err2 := parseDecimal(orZero(s.MeterBaseline))
	iv, err3 := parseDecimal(s.MeterInterval)
	lead, err4 := parseDecimal(orZero(s.MeterLead))
	if err := errors.Join(err1, err2, err3, err4); err != nil {
		return st, fmt.Errorf("schedule %s meter: %w", s.ID, err)
	}
	used := new(big.Rat).Sub(t, base)
	remaining := new(big.Rat).Sub(iv, used)
	st.Used, st.Remaining = formatLimit(used), formatLimit(remaining)
	st.Due = remaining.Cmp(lead) <= 0
	return st, nil
}
