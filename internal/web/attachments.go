package web

import (
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"

	"pi-fleet/internal/blobs"
	"pi-fleet/internal/domain"
)

// maxFiles is how many files one form may carry (equipment registration
// takes photos and documents together); maxUpload bounds such a form.
const (
	maxFiles  = 5
	maxUpload = maxFiles*blobs.MaxSize + 1<<20
)

func (s *Server) attachmentUpload(targetType string) handler {
	return func(w http.ResponseWriter, r *http.Request, sess *session) error {
		id := r.PathValue("id")
		back := "/assets/" + id
		if targetType == domain.EntityWorkOrder {
			back = "/work-orders/" + id
		}
		_, hdr, err := r.FormFile("file")
		if err != nil {
			return s.done(w, r, sess, back, "Not saved: choose a file (JPEG, PNG or PDF, at most 10 MiB).")
		}
		if err := s.attachTo(r, sess, targetType, id, hdr, strings.TrimSpace(r.PostFormValue("description"))); err != nil {
			return s.failed(w, r, sess, back, err)
		}
		return s.done(w, r, sess, back, "File attached. Location and camera details were removed from photos.")
	}
}

// attachFile attaches an uploaded file to an asset.
func (s *Server) attachFile(r *http.Request, sess *session, assetID string, hdr *multipart.FileHeader, desc string) error {
	return s.attachTo(r, sess, domain.EntityAsset, assetID, hdr, desc)
}

func (s *Server) attachTo(r *http.Request, sess *session, targetType, id string, hdr *multipart.FileHeader, desc string) error {
	f, err := hdr.Open()
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, blobs.MaxSize+1))
	if err != nil {
		return err
	}
	if err := s.phiCheck(r, desc, hdr.Filename); err != nil {
		return err
	}
	_, err = s.App.AddAttachment(r.Context(), s.actor(sess), targetType, id, hdr.Filename, desc, data)
	if errors.Is(err, blobs.ErrType) || errors.Is(err, blobs.ErrTooLarge) || errors.Is(err, blobs.ErrMalformed) {
		return userErr("%s", strings.TrimPrefix(err.Error(), "blobs: "))
	}
	return err
}

func attachmentBack(a domain.Attachment) string {
	if a.TargetType == domain.EntityWorkOrder {
		return "/work-orders/" + a.TargetID
	}
	return "/assets/" + a.TargetID
}

func (s *Server) attachmentDownload(w http.ResponseWriter, r *http.Request, sess *session) error {
	ctx := r.Context()
	a, err := domain.GetAttachment(ctx, s.App.Store.DB(), r.PathValue("id"))
	if err != nil {
		return err
	}
	if a.Status == "purged" {
		http.Error(w, "this file was deleted: "+a.DetachReason, http.StatusGone)
		return nil
	}
	data, err := s.App.Blobs.Get(a.SHA256)
	if errors.Is(err, blobs.ErrNotFound) && s.Fleet != nil {
		data, err = s.Fleet.FetchBlob(ctx, a.SHA256)
	}
	if err != nil {
		return s.done(w, r, sess, attachmentBack(a), "That file isn't available yet. It stays on the Pi that added it until that Pi syncs, and this Pi needs a connection to fetch it.")
	}
	h := w.Header()
	h.Set("Content-Type", a.MIME)
	h.Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(a.Filename))
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	_, err = w.Write(data)
	return err
}

func (s *Server) attachmentDetach(w http.ResponseWriter, r *http.Request, sess *session) error {
	ctx := r.Context()
	a, err := domain.GetAttachment(ctx, s.App.Store.DB(), r.PathValue("id"))
	if err != nil {
		return err
	}
	back := attachmentBack(a)
	purge := r.PostFormValue("purge") == "yes"
	if err := s.App.DetachAttachment(ctx, s.actor(sess), a.ID, strings.TrimSpace(r.PostFormValue("reason")), purge); err != nil {
		return s.failed(w, r, sess, back, err)
	}
	if purge {
		return s.done(w, r, sess, back, "File deleted. Its hash and your reason stay in the audit trail.")
	}
	return s.done(w, r, sess, back, "Attachment removed.")
}
