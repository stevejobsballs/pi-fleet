package web

import (
	"net/http"
	"strconv"
	"strings"

	"pi-fleet/internal/domain"
)

// assetEdit changes only the details the user actually changed, compared
// with the values the form was rendered with, so concurrent edits to
// other details merge (DESIGN.md §5.4).
func (s *Server) assetEdit(w http.ResponseWriter, r *http.Request, sess *session) error {
	id := r.PathValue("id")
	back := "/assets/" + id
	f := r.PostFormValue
	var u domain.AssetUpdated
	changed := func(name string) (string, bool) {
		v := strings.TrimSpace(f(name))
		return v, v != f("orig_"+name)
	}
	if v, ok := changed("manufacturer"); ok {
		u.Manufacturer = &v
	}
	if v, ok := changed("model"); ok {
		u.Model = &v
	}
	if v, ok := changed("serial"); ok {
		u.Serial = &v
	}
	if v, ok := changed("risk_class"); ok {
		u.RiskClass = &v
	}
	if v, ok := changed("master_id"); ok {
		u.MasterID = &v
	}
	ref := f("reference") == "yes"
	if ref != (f("orig_reference") == "yes") {
		u.IsReferenceStandard = &ref
	}
	if u == (domain.AssetUpdated{}) {
		return s.done(w, r, sess, back, "Nothing changed.")
	}
	if err := s.App.UpdateAsset(r.Context(), s.actor(sess), id, formInt(r, "version"), u); err != nil {
		return s.failed(w, r, sess, back, err)
	}
	return s.done(w, r, sess, back, "Details saved.")
}

func (s *Server) scheduleChange(w http.ResponseWriter, r *http.Request, sess *session) error {
	f := r.PostFormValue
	c := domain.PMScheduleChanged{Reason: strings.TrimSpace(f("reason"))}
	if v := strings.TrimSpace(f("title")); v != "" {
		c.Title = &v
	}
	if v, err := strconv.Atoi(f("interval")); err == nil {
		c.IntervalDays = &v
	}
	if v, err := strconv.Atoi(f("grace")); err == nil {
		c.GraceDays = &v
	}
	if v := f("next_due"); v != "" {
		c.NextDue = &v
	}
	if _, ok := r.PostForm["meter"]; ok {
		meter, interval, lead := strings.ToLower(strings.TrimSpace(f("meter"))), strings.TrimSpace(f("meter_interval")), strings.TrimSpace(f("meter_lead"))
		c.Meter, c.MeterInterval, c.MeterLead = &meter, &interval, &lead
	}
	if err := s.App.ChangeSchedule(r.Context(), s.actor(sess), r.PathValue("id"), c); err != nil {
		return s.failed(w, r, sess, "/schedules", err)
	}
	return s.done(w, r, sess, "/schedules", "Schedule changed.")
}

func (s *Server) scheduleEnd(w http.ResponseWriter, r *http.Request, sess *session) error {
	if err := s.App.EndSchedule(r.Context(), s.actor(sess), r.PathValue("id"), strings.TrimSpace(r.PostFormValue("reason"))); err != nil {
		return s.failed(w, r, sess, "/schedules", err)
	}
	return s.done(w, r, sess, "/schedules", "Schedule ended.")
}

func (s *Server) flagResolve(w http.ResponseWriter, r *http.Request, sess *session) error {
	note := strings.TrimSpace(r.PostFormValue("note"))
	if err := s.phiCheck(r, note); err != nil {
		return s.failed(w, r, sess, "/review", err)
	}
	if err := s.App.ResolveFlag(r.Context(), s.actor(sess), r.PathValue("id"), r.PostFormValue("resolution"), note); err != nil {
		return s.failed(w, r, sess, "/review", err)
	}
	return s.done(w, r, sess, "/review", "Decision recorded.")
}

func (s *Server) nodeUnquarantine(w http.ResponseWriter, r *http.Request, sess *session) error {
	if err := s.App.ClearQuarantine(r.Context(), s.actor(sess), r.PathValue("id"), strings.TrimSpace(r.PostFormValue("reason"))); err != nil {
		return s.failed(w, r, sess, "/admin/nodes", err)
	}
	return s.done(w, r, sess, "/admin/nodes", "Quarantine lifted. The Pi can sync again.")
}
