package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"pi-fleet/internal/domain"
)

type mergeData struct {
	Group domain.DuplicateGroup
}

// mergePage shows the records sharing a MasterID side by side, to choose
// the one to keep.
func (s *Server) mergePage(w http.ResponseWriter, r *http.Request, sess *session) error {
	groups, err := domain.Duplicates(r.Context(), s.App.Store.DB(), r.URL.Query().Get("master_id"))
	if err != nil {
		return err
	}
	if len(groups) == 0 {
		return s.done(w, r, sess, "/review", "No duplicate records have that MasterID.")
	}
	return s.render(w, r, sess, "merge", "Merge duplicate equipment", mergeData{Group: groups[0]})
}

// mergeDo merges every other record with the MasterID into the one kept.
func (s *Server) mergeDo(w http.ResponseWriter, r *http.Request, sess *session) error {
	ctx, f := r.Context(), r.PostFormValue
	masterID, keepID := f("master_id"), f("keep")
	back := "/assets/merge?master_id=" + url.QueryEscape(masterID)
	reason := strings.TrimSpace(f("reason"))
	if err := s.phiCheck(r, reason); err != nil {
		return s.failed(w, r, sess, back, err)
	}
	var integrate bool
	switch f("integrate") {
	case "yes":
		integrate = true
	case "no":
	default:
		return s.done(w, r, sess, back, "Not saved: answer whether to add the merged records' service history to the kept record's timeline.")
	}
	groups, err := domain.Duplicates(ctx, s.App.Store.DB(), masterID)
	if err != nil {
		return err
	}
	if len(groups) == 0 {
		return s.done(w, r, sess, "/review", "No duplicate records have that MasterID.")
	}
	keep, err := domain.GetAsset(ctx, s.App.Store.DB(), keepID)
	if err != nil || keep.MasterID != masterID || keep.MergedInto != "" {
		return s.done(w, r, sess, back, "Not saved: choose which record to keep.")
	}
	n := 0
	for _, a := range groups[0].Assets {
		if a.ID == keep.ID {
			continue
		}
		if err := s.App.MergeAssets(ctx, s.actor(sess), keep.ID, keep.Version, a.ID, reason, integrate); err != nil {
			return s.failed(w, r, sess, back, err)
		}
		n++
	}
	msg := fmt.Sprintf("Merged %d records into this one.", n)
	if n == 1 {
		msg = "Merged 1 record into this one."
	}
	if integrate {
		msg += " Their service history is now part of this record's timeline."
	} else {
		msg += " Their service history stays with them; they are linked below."
	}
	return s.done(w, r, sess, "/assets/"+keep.ID, msg)
}
