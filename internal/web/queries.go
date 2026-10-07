package web

import (
	"context"
	"database/sql"
	"time"

	"pi-fleet/internal/domain"
)

// Read models for pages. Writes never happen here.

func scanAll[T any](rows *sql.Rows, err error, scan func(*sql.Rows) (T, error)) ([]T, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

type assetRow struct {
	ID, Tag, Manufacturer, Model, Serial, Status, Site, Location, NextDue string
	Reference                                                             bool
	MasterID, MergedIntoTag                                               string
}

func listAssets(ctx context.Context, q domain.Querier, search string) ([]assetRow, error) {
	like := "%" + search + "%"
	rows, err := q.QueryContext(ctx, `SELECT a.id, a.tag, a.manufacturer, a.model, a.serial, a.status, s.code, l.name,
			coalesce((SELECT min(next_due) FROM pm_schedules p WHERE p.asset_id = a.id AND p.status = 'active'), ''),
			a.is_reference_standard, a.master_id, coalesce((SELECT k.tag FROM assets k WHERE k.id = a.merged_into), '')
		FROM assets a JOIN sites s ON s.id = a.site_id JOIN locations l ON l.id = a.location_id
		WHERE CASE WHEN ? = '' THEN a.merged_into = ''
			ELSE a.tag LIKE ? OR a.manufacturer LIKE ? OR a.model LIKE ? OR a.serial LIKE ? OR l.name LIKE ? OR a.master_id LIKE ? END
		ORDER BY a.tag LIMIT 500`, search, like, like, like, like, like, like)
	return scanAll(rows, err, func(r *sql.Rows) (assetRow, error) {
		var a assetRow
		err := r.Scan(&a.ID, &a.Tag, &a.Manufacturer, &a.Model, &a.Serial, &a.Status, &a.Site, &a.Location, &a.NextDue, &a.Reference,
			&a.MasterID, &a.MergedIntoTag)
		return a, err
	})
}

type option struct{ ID, Label string }

func locationOptions(ctx context.Context, q domain.Querier) ([]option, error) {
	rows, err := q.QueryContext(ctx, `SELECT l.id, s.code || ' · ' || l.name FROM locations l JOIN sites s ON s.id = l.site_id ORDER BY s.code, l.name`)
	return scanAll(rows, err, func(r *sql.Rows) (option, error) {
		var o option
		return o, r.Scan(&o.ID, &o.Label)
	})
}

func siteOptions(ctx context.Context, q domain.Querier) ([]option, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, code || ' · ' || name FROM sites ORDER BY code`)
	return scanAll(rows, err, func(r *sql.Rows) (option, error) {
		var o option
		return o, r.Scan(&o.ID, &o.Label)
	})
}

func userOptions(ctx context.Context, q domain.Querier) ([]option, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, legal_name || ' (' || username || ')' FROM users WHERE status = 'active' ORDER BY legal_name`)
	return scanAll(rows, err, func(r *sql.Rows) (option, error) {
		var o option
		return o, r.Scan(&o.ID, &o.Label)
	})
}

func standardOptions(ctx context.Context, q domain.Querier, exclude string) ([]option, error) {
	rows, err := q.QueryContext(ctx, `SELECT a.id, a.tag || ' · ' || a.manufacturer || ' ' || a.model ||
			coalesce(' · due ' || (SELECT min(next_due) FROM pm_schedules p WHERE p.asset_id = a.id AND p.wo_type = 'calibration' AND p.status = 'active'), '')
		FROM assets a WHERE a.is_reference_standard = 1 AND a.status = 'in_service' AND a.id != ? ORDER BY a.tag`, exclude)
	return scanAll(rows, err, func(r *sql.Rows) (option, error) {
		var o option
		return o, r.Scan(&o.ID, &o.Label)
	})
}

type woRow struct {
	ID, Number, Type, Title, Status, Priority, DueAt, Asset, AssignedTo string
}

func listWorkOrders(ctx context.Context, q domain.Querier, view, userID string) ([]woRow, error) {
	where := map[string]string{
		"mine":       "w.assigned_to = ? AND w.status NOT IN ('closed', 'cancelled')",
		"open":       "? != '' AND w.status NOT IN ('closed', 'cancelled')",
		"unassigned": "? != '' AND w.status = 'open'",
		"review":     "? != '' AND w.status IN ('completed', 'reviewed')",
		"all":        "? != ''",
	}[view]
	if where == "" {
		where = "w.assigned_to = ? AND w.status NOT IN ('closed', 'cancelled')"
	}
	rows, err := q.QueryContext(ctx, `SELECT w.id, w.number, w.type, w.title, w.status, w.priority, w.due_at, a.tag,
			coalesce((SELECT legal_name FROM users u WHERE u.id = w.assigned_to), '')
		FROM work_orders w JOIN assets a ON a.id = w.asset_id
		WHERE `+where+` ORDER BY CASE w.priority WHEN 'urgent' THEN 0 WHEN 'high' THEN 1 WHEN 'normal' THEN 2 ELSE 3 END,
			w.due_at = '', w.due_at, w.number LIMIT 500`, userID)
	return scanAll(rows, err, func(r *sql.Rows) (woRow, error) {
		var w woRow
		return w, r.Scan(&w.ID, &w.Number, &w.Type, &w.Title, &w.Status, &w.Priority, &w.DueAt, &w.Asset, &w.AssignedTo)
	})
}

type dueRow struct {
	ScheduleID, Title, Type, NextDue, Asset, AssetID, Site, WorkOrder, WorkOrderID string
}

func dueSoon(ctx context.Context, q domain.Querier, now time.Time) ([]dueRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT p.id, p.title, p.wo_type, p.next_due, a.tag, a.id, s.code,
			coalesce(w.number, ''), p.open_wo_id
		FROM pm_schedules p JOIN assets a ON a.id = p.asset_id JOIN sites s ON s.id = a.site_id
		LEFT JOIN work_orders w ON w.id = p.open_wo_id
		WHERE p.status = 'active' AND p.next_due <= ? ORDER BY p.next_due, a.tag LIMIT 200`,
		now.Add(31*24*time.Hour).Format("2006-01-02"))
	return scanAll(rows, err, func(r *sql.Rows) (dueRow, error) {
		var d dueRow
		return d, r.Scan(&d.ScheduleID, &d.Title, &d.Type, &d.NextDue, &d.Asset, &d.AssetID, &d.Site, &d.WorkOrder, &d.WorkOrderID)
	})
}

type calPointRow struct {
	Parameter, Unit, Nominal, Lower, Upper, AsFound, AsLeft string
	FoundPass, LeftPass                                     bool
}

type calRow struct {
	ID, Procedure, PerformedBy, AsFound, AsLeft, Status, VoidReason, Temperature, Humidity, WorkOrder string
	PerformedAt                                                                                       time.Time
	Adjusted                                                                                          bool
	Points                                                                                            []calPointRow
	Standards                                                                                         []string
	Flags                                                                                             []string
}

func calibrations(ctx context.Context, q domain.Querier, where string, arg string) ([]calRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT c.id, c.procedure, coalesce(u.legal_name, c.performed_by), c.as_found_result,
			c.as_left_result, c.status, c.void_reason, c.temperature, c.humidity, coalesce(w.number, ''), c.performed_at, c.adjusted
		FROM calibration_records c LEFT JOIN users u ON u.id = c.performed_by LEFT JOIN work_orders w ON w.id = c.wo_id
		WHERE `+where+` ORDER BY c.performed_at DESC`, arg)
	recs, err := scanAll(rows, err, func(r *sql.Rows) (calRow, error) {
		var c calRow
		var at string
		err := r.Scan(&c.ID, &c.Procedure, &c.PerformedBy, &c.AsFound, &c.AsLeft, &c.Status, &c.VoidReason,
			&c.Temperature, &c.Humidity, &c.WorkOrder, &at, &c.Adjusted)
		c.PerformedAt, _ = time.Parse(time.RFC3339, at)
		return c, err
	})
	if err != nil {
		return nil, err
	}
	for i := range recs {
		pr, err := q.QueryContext(ctx, `SELECT parameter, unit, nominal, lower_limit, upper_limit, as_found, as_left,
			as_found_pass, as_left_pass FROM cal_points WHERE record_id = ? ORDER BY idx`, recs[i].ID)
		if recs[i].Points, err = scanAll(pr, err, func(r *sql.Rows) (calPointRow, error) {
			var p calPointRow
			return p, r.Scan(&p.Parameter, &p.Unit, &p.Nominal, &p.Lower, &p.Upper, &p.AsFound, &p.AsLeft, &p.FoundPass, &p.LeftPass)
		}); err != nil {
			return nil, err
		}
		sr, err := q.QueryContext(ctx, `SELECT a.tag || CASE WHEN cs.due_at_time_of_use = '' THEN ' (untracked)'
				ELSE ' (due ' || cs.due_at_time_of_use || ')' END
			FROM cal_standards cs JOIN assets a ON a.id = cs.standard_asset_id WHERE cs.record_id = ? ORDER BY a.tag`, recs[i].ID)
		if recs[i].Standards, err = scanAll(sr, err, func(r *sql.Rows) (string, error) {
			var s string
			return s, r.Scan(&s)
		}); err != nil {
			return nil, err
		}
		fr, err := q.QueryContext(ctx, `SELECT f.flag || ': ' || f.detail FROM event_flags f JOIN events e USING (event_id)
			WHERE e.entity_id = ? ORDER BY f.flag`, recs[i].ID)
		if recs[i].Flags, err = scanAll(fr, err, func(r *sql.Rows) (string, error) {
			var s string
			return s, r.Scan(&s)
		}); err != nil {
			return nil, err
		}
	}
	return recs, nil
}

type scheduleRow struct {
	ID, Asset, AssetID, Type, Title, Procedure, NextDue, Status, OpenWO string
	Interval, Grace                                                     int
}

func listSchedules(ctx context.Context, q domain.Querier, assetID string) ([]scheduleRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT p.id, a.tag, a.id, p.wo_type, p.title, p.procedure, p.next_due, p.status,
			coalesce(w.number, ''), p.interval_days, p.grace_days
		FROM pm_schedules p JOIN assets a ON a.id = p.asset_id LEFT JOIN work_orders w ON w.id = p.open_wo_id
		WHERE ? = '' OR p.asset_id = ? ORDER BY p.status, p.next_due LIMIT 500`, assetID, assetID)
	return scanAll(rows, err, func(r *sql.Rows) (scheduleRow, error) {
		var s scheduleRow
		return s, r.Scan(&s.ID, &s.Asset, &s.AssetID, &s.Type, &s.Title, &s.Procedure, &s.NextDue, &s.Status, &s.OpenWO, &s.Interval, &s.Grace)
	})
}

type stockRow struct {
	Part, PartNo, Location, Unit string
	Qty                          int64
}

type stockLocationRow struct {
	ID, Name, Site, Owner string
}

type partRow struct{ ID, PartNo, Description, Unit string }

type inventoryData struct {
	Levels    []stockRow
	Locations []stockLocationRow
	Parts     []partRow
	Sites     []option
	Kinds     []string
}

func loadInventory(ctx context.Context, q domain.Querier) (inventoryData, error) {
	var d inventoryData
	var err error
	rows, err := q.QueryContext(ctx, `SELECT p.description, p.part_no, l.name || coalesce(' (' || u.legal_name || ')', ''), p.unit, sl.qty
		FROM stock_levels sl JOIN parts p ON p.id = sl.part_id JOIN stock_locations l ON l.id = sl.location_id
		LEFT JOIN users u ON u.id = l.owner_user_id AND l.owner_user_id != ''
		WHERE sl.qty != 0 ORDER BY p.part_no, l.name`)
	if d.Levels, err = scanAll(rows, err, func(r *sql.Rows) (stockRow, error) {
		var s stockRow
		return s, r.Scan(&s.Part, &s.PartNo, &s.Location, &s.Unit, &s.Qty)
	}); err != nil {
		return d, err
	}
	rows, err = q.QueryContext(ctx, `SELECT l.id, l.name, s.code, coalesce(u.legal_name, '')
		FROM stock_locations l JOIN sites s ON s.id = l.site_id LEFT JOIN users u ON u.id = l.owner_user_id ORDER BY s.code, l.name`)
	if d.Locations, err = scanAll(rows, err, func(r *sql.Rows) (stockLocationRow, error) {
		var l stockLocationRow
		return l, r.Scan(&l.ID, &l.Name, &l.Site, &l.Owner)
	}); err != nil {
		return d, err
	}
	rows, err = q.QueryContext(ctx, `SELECT id, part_no, description, unit FROM parts ORDER BY part_no`)
	if d.Parts, err = scanAll(rows, err, func(r *sql.Rows) (partRow, error) {
		var p partRow
		return p, r.Scan(&p.ID, &p.PartNo, &p.Description, &p.Unit)
	}); err != nil {
		return d, err
	}
	d.Sites, err = siteOptions(ctx, q)
	d.Kinds = []string{"receive", "issue", "return", "transfer", "count", "adjust"}
	return d, err
}

type userRow struct {
	ID, Username, LegalName, Email, Role, Status string
	Expires, LockedUntil                         time.Time
}

func listUsers(ctx context.Context, q domain.Querier) ([]userRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, username, legal_name, email, role, status, password_expires_at, locked_until FROM users ORDER BY username`)
	return scanAll(rows, err, func(r *sql.Rows) (userRow, error) {
		var u userRow
		var exp, locked string
		err := r.Scan(&u.ID, &u.Username, &u.LegalName, &u.Email, &u.Role, &u.Status, &exp, &locked)
		u.Expires, _ = time.Parse(time.RFC3339, exp)
		u.LockedUntil, _ = time.Parse(time.RFC3339, locked)
		return u, err
	})
}

type flagRow struct {
	EventID, Type, Actor, Flag, Detail, Node, EntityType, EntityID string
	WallTime, Resolution, ResolutionNote, ResolvedBy               string
	Projected                                                      bool
}

// listFlags returns flagged records, unresolved first.
func listFlags(ctx context.Context, q domain.Querier, all bool) ([]flagRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT e.event_id, e.type, coalesce(u.legal_name, e.actor_user_id), f.flag, f.detail, e.node_id,
			e.entity_type, e.entity_id, e.wall_time, coalesce(r.resolution, ''), coalesce(r.note, ''),
			coalesce((SELECT legal_name FROM users WHERE id = r.resolved_by), ''), f.projected
		FROM event_flags f JOIN events e USING (event_id) LEFT JOIN users u ON u.id = e.actor_user_id
		LEFT JOIN flag_resolutions r ON r.event_id = f.event_id
		WHERE ? OR r.event_id IS NULL
		ORDER BY r.event_id IS NOT NULL, e.local_order DESC LIMIT 500`, all)
	return scanAll(rows, err, func(r *sql.Rows) (flagRow, error) {
		var f flagRow
		return f, r.Scan(&f.EventID, &f.Type, &f.Actor, &f.Flag, &f.Detail, &f.Node, &f.EntityType, &f.EntityID,
			&f.WallTime, &f.Resolution, &f.ResolutionNote, &f.ResolvedBy, &f.Projected)
	})
}

type siteRow struct {
	ID, Code, Name, Timezone string
	Locations                []option
}

func listSites(ctx context.Context, q domain.Querier) ([]siteRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, code, name, timezone FROM sites ORDER BY code`)
	sites, err := scanAll(rows, err, func(r *sql.Rows) (siteRow, error) {
		var s siteRow
		return s, r.Scan(&s.ID, &s.Code, &s.Name, &s.Timezone)
	})
	if err != nil {
		return nil, err
	}
	for i := range sites {
		rows, err := q.QueryContext(ctx, `SELECT id, name || ' (' || kind || ')' FROM locations WHERE site_id = ? ORDER BY name`, sites[i].ID)
		if sites[i].Locations, err = scanAll(rows, err, func(r *sql.Rows) (option, error) {
			var o option
			return o, r.Scan(&o.ID, &o.Label)
		}); err != nil {
			return nil, err
		}
	}
	return sites, nil
}

func siteTimezone(ctx context.Context, q domain.Querier, siteID string) string {
	var tz string
	q.QueryRowContext(ctx, `SELECT timezone FROM sites WHERE id = ?`, siteID).Scan(&tz)
	return tz
}
