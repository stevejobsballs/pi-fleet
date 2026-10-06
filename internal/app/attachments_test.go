package app

import (
	"errors"
	"testing"

	"pi-fleet/internal/blobs"
	"pi-fleet/internal/domain"
)

var certificate = []byte("%PDF-1.7\nCalibration certificate 4411\n%%EOF")

func TestAttachmentRules(t *testing.T) {
	e := newEnv(t)
	mid, tech, other := e.activeUser("mona", domain.RoleMidTier), e.activeUser("tess", domain.RoleUser), e.activeUser("otto", domain.RoleUser)
	asset := e.asset("A1")
	wo := e.openHeld(mid, tech, "pm", asset)

	id, err := e.app.AddAttachment(e.ctx, tech, domain.EntityWorkOrder, wo, "../../cert 4411.pdf", "certificate", certificate)
	e.must(err)
	a, err := domain.GetAttachment(e.ctx, e.st.DB(), id)
	e.must(err)
	if a.Filename != "cert 4411.pdf" || a.MIME != blobs.PDF || !e.app.Blobs.Has(a.SHA256) {
		t.Fatalf("attachment = %+v", a)
	}
	_, err = e.app.AddAttachment(e.ctx, other, domain.EntityWorkOrder, wo, "x.pdf", "", certificate)
	wantRejection(t, err, domain.FlagNotAuthorized)
	if _, err := e.app.AddAttachment(e.ctx, tech, domain.EntityAsset, asset, "page.html", "", []byte("<html>")); !errors.Is(err, blobs.ErrType) {
		t.Fatalf("HTML accepted: %v", err)
	}
	_, err = e.app.AddAttachment(e.ctx, other, domain.EntityAsset, asset, "label.pdf", "asset label", certificate)
	e.must(err) // anyone can add files to equipment

	// A file added after signing makes the signature stale.
	e.must(e.sign(tech, wo, domain.MeaningPerformed))
	_, err = e.app.AddAttachment(e.ctx, mid, domain.EntityWorkOrder, wo, "photo.pdf", "after the fact", []byte("%PDF-1.4 extra"))
	e.must(err)
	sigs, _ := domain.WorkOrderSignatures(e.ctx, e.st.DB(), wo)
	if len(sigs) != 1 || !sigs[0].Stale {
		t.Fatalf("signature should be stale after a new file: %+v", sigs)
	}

	// Only mid-tier users can delete the file itself.
	wantRejection(t, e.app.DetachAttachment(e.ctx, tech, id, "wrong file", true), domain.FlagNotAuthorized)
	e.must(e.app.DetachAttachment(e.ctx, mid, id, "certificate shows a patient label", true))
	if e.app.Blobs.Has(a.SHA256) {
		t.Fatal("purged file still stored")
	}
	if a, _ = domain.GetAttachment(e.ctx, e.st.DB(), id); a.Status != "purged" {
		t.Fatalf("status = %s", a.Status)
	}
	// The same file attached to the equipment is purged with it.
	var live int
	e.st.DB().QueryRow(`SELECT count(*) FROM attachments WHERE sha256 = ? AND status != 'purged'`, a.SHA256).Scan(&live)
	if live != 0 {
		t.Fatal("another attachment of the purged file survived")
	}
	wantRejection(t, e.app.DetachAttachment(e.ctx, mid, id, "again", true), domain.FlagInvalid)

	// Rebuilding gives the same attachments.
	e.must(e.st.Rebuild(e.ctx))
	if a, _ = domain.GetAttachment(e.ctx, e.st.DB(), id); a.Status != "purged" {
		t.Fatalf("after rebuild: %s", a.Status)
	}
}
