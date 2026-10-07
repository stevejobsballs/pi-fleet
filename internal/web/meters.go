package web

import (
	"context"
	"net/http"
	"strings"
	"time"

	"pi-fleet/internal/domain"
)

// meterRecord records a reading; the time is entered in the site's zone.
func (s *Server) meterRecord(w http.ResponseWriter, r *http.Request, sess *session) error {
	ctx := r.Context()
	id := r.PathValue("id")
	back := "/assets/" + id + "#meters"
	a, err := domain.GetAsset(ctx, s.App.Store.DB(), id)
	if err != nil {
		return err
	}
	loc, err := time.LoadLocation(siteTimezone(ctx, s.App.Store.DB(), a.SiteID))
	if err != nil {
		loc = time.UTC
	}
	readAt, err := time.ParseInLocation("2006-01-02T15:04", r.PostFormValue("read_at"), loc)
	if err != nil {
		return s.done(w, r, sess, back, "Not saved: enter when the meter was read.")
	}
	meter := strings.ToLower(strings.TrimSpace(r.PostFormValue("meter")))
	if _, err := s.App.RecordMeter(ctx, s.actor(sess), id, meter, strings.TrimSpace(r.PostFormValue("value")), readAt, r.PostFormValue("reset") == "yes"); err != nil {
		return s.failed(w, r, sess, back, err)
	}
	return s.done(w, r, sess, back, "Reading recorded.")
}

func (s *Server) meterVoid(w http.ResponseWriter, r *http.Request, sess *session) error {
	var assetID string
	if err := s.App.Store.DB().QueryRowContext(r.Context(), `SELECT asset_id FROM meter_readings WHERE id = ?`, r.PathValue("id")).Scan(&assetID); err != nil {
		return domain.ErrNotFound
	}
	back := "/assets/" + assetID + "#meters"
	if err := s.App.VoidMeter(r.Context(), s.actor(sess), r.PathValue("id"), strings.TrimSpace(r.PostFormValue("reason"))); err != nil {
		return s.failed(w, r, sess, back, err)
	}
	return s.done(w, r, sess, back, "Reading voided.")
}

type usageRow struct {
	Schedule domain.Schedule
	Asset    string
	Status   domain.MeterStatus
}

// usageSchedules lists active schedules with a usage trigger and how far
// along they are; dueOnly keeps those within their lead.
func usageSchedules(ctx context.Context, q domain.Querier, assetID string, dueOnly bool) ([]usageRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT p.id, a.tag FROM pm_schedules p JOIN assets a ON a.id = p.asset_id
		WHERE p.status = 'active' AND p.meter != '' AND (? = '' OR p.asset_id = ?) ORDER BY a.tag`, assetID, assetID)
	if err != nil {
		return nil, err
	}
	type idTag struct{ id, tag string }
	var list []idTag
	for rows.Next() {
		var x idTag
		rows.Scan(&x.id, &x.tag)
		list = append(list, x)
	}
	rows.Close()
	var out []usageRow
	for _, x := range list {
		sch, err := domain.GetSchedule(ctx, q, x.id)
		if err != nil {
			return nil, err
		}
		st, err := domain.ScheduleMeterStatus(ctx, q, sch)
		if err != nil {
			return nil, err
		}
		if dueOnly && !st.Due {
			continue
		}
		out = append(out, usageRow{Schedule: sch, Asset: x.tag, Status: st})
	}
	return out, nil
}
