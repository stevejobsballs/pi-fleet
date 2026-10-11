package web

import (
	"net/http"
	"net/url"
	"strings"

	"pi-fleet/internal/domain"
)

// A work order's work done and test equipment used, recorded by the
// person doing the work while it is in progress.

func (s *Server) workOrderWorkDone(w http.ResponseWriter, r *http.Request, sess *session) error {
	id := r.PathValue("id")
	back := "/work-orders/" + id + "#work-done"
	text := strings.TrimSpace(r.PostFormValue("work_done"))
	if err := s.phiCheck(r, text); err != nil {
		return s.failed(w, r, sess, back, err)
	}
	if err := s.App.RecordWorkDone(r.Context(), s.actor(sess), id, text); err != nil {
		return s.failed(w, r, sess, back, err)
	}
	return s.done(w, r, sess, back, "Work done saved.")
}

func (s *Server) workOrderTestEquipmentAdd(w http.ResponseWriter, r *http.Request, sess *session) error {
	id := r.PathValue("id")
	back := "/work-orders/" + id + "#test-equipment"
	asset := r.PostFormValue("asset")
	if asset == "" {
		return s.done(w, r, sess, back, "Not added: choose the test equipment first.")
	}
	if err := s.App.AddTestEquipment(r.Context(), s.actor(sess), id, asset); err != nil {
		return s.failed(w, r, sess, back, err)
	}
	return s.done(w, r, sess, back, "Test equipment added.")
}

func (s *Server) workOrderTestEquipmentRemove(w http.ResponseWriter, r *http.Request, sess *session) error {
	id := r.PathValue("id")
	back := "/work-orders/" + id + "#test-equipment"
	if err := s.App.RemoveTestEquipment(r.Context(), s.actor(sess), id, r.PostFormValue("asset")); err != nil {
		return s.failed(w, r, sess, back, err)
	}
	return s.done(w, r, sess, back, "Test equipment taken off the list.")
}

// newTestEquipmentData is the form for registering test equipment that
// isn't in pi-fleet yet, from a work order, to add it there and carry on.
type newTestEquipmentData struct {
	WO        domain.WorkOrder
	Locations []option
	F         url.Values
	Error     string
}

func (d newTestEquipmentData) BackLink() (string, string) {
	return "/work-orders/" + d.WO.ID + "#test-equipment", "work order " + d.WO.Number
}

func (s *Server) renderNewTestEquipment(w http.ResponseWriter, r *http.Request, sess *session, d newTestEquipmentData) error {
	ctx, q := r.Context(), s.App.Store.DB()
	wo, err := domain.GetWorkOrder(ctx, q, r.PathValue("id"))
	if err != nil {
		return err
	}
	d.WO = wo
	if d.Locations, err = locationOptions(ctx, q); err != nil {
		return err
	}
	if d.F == nil {
		d.F = url.Values{}
		// Most likely kept where the work is done: the equipment's place.
		if a, err := domain.GetAsset(ctx, q, wo.AssetID); err == nil {
			d.F.Set("location", a.LocationID)
		}
	}
	return s.render(w, r, sess, "test_equipment_new", "Add test equipment", d)
}

func (s *Server) testEquipmentNew(w http.ResponseWriter, r *http.Request, sess *session) error {
	return s.renderNewTestEquipment(w, r, sess, newTestEquipmentData{})
}

// testEquipmentCreate registers the test equipment as a reference
// standard, lists it as used on the work order, and goes back there.
func (s *Server) testEquipmentCreate(w http.ResponseWriter, r *http.Request, sess *session) error {
	ctx, f := r.Context(), r.PostFormValue
	id := r.PathValue("id")
	again := func(msg string) error {
		return s.renderNewTestEquipment(w, r, sess, newTestEquipmentData{F: r.PostForm, Error: "Not added: " + msg})
	}
	asset, err := s.App.RegisterAsset(ctx, s.actor(sess), domain.AssetRegistered{
		Tag: strings.TrimSpace(f("tag")), LocationID: f("location"), Manufacturer: strings.TrimSpace(f("manufacturer")),
		Model: strings.TrimSpace(f("model")), Serial: strings.TrimSpace(f("serial")), IsReferenceStandard: true,
	})
	if err != nil {
		if msg := message(err); msg != "" {
			return again(msg)
		}
		return err
	}
	back := "/work-orders/" + id + "#test-equipment"
	if err := s.App.AddTestEquipment(ctx, s.actor(sess), id, asset); err != nil {
		if msg := message(err); msg != "" {
			return s.done(w, r, sess, back, "Registered, but not added to this work order: "+msg)
		}
		return err
	}
	msg := "Registered " + strings.TrimSpace(f("tag")) + " and added it to this work order."
	if roleRank[sess.User.Role] < roleRank[domain.RoleMidTier] {
		msg += " A mid-tier user sets up its calibration schedule; until then it shows as untracked."
	} else {
		msg += " Set up its calibration schedule on its page, so overdue use is caught."
	}
	return s.done(w, r, sess, back, msg)
}
