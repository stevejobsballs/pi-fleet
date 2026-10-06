package app

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"pi-fleet/internal/domain"
	"pi-fleet/internal/event"
)

func TestSignatureNeedsPasswordAndClockAcknowledgement(t *testing.T) {
	e := newEnv(t)
	mid, tech := e.activeUser("mona", domain.RoleMidTier), e.activeUser("tess", domain.RoleUser)
	wo := e.openHeld(mid, tech, "pm", e.asset("A1"))

	if err := e.app.Sign(e.ctx, tech, wo, domain.MeaningPerformed, "wrong-password", true); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("wrong password: %v", err)
	}
	// Someone else's password, typed at Tess's session, is refused.
	if err := e.app.Sign(e.ctx, tech, wo, domain.MeaningPerformed, "tumbleweed-gasket-42", true); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("another user's password: %v", err)
	}
	// This node's clock is unverified: the signer must acknowledge it.
	if err := e.app.Sign(e.ctx, tech, wo, domain.MeaningPerformed, "brass-kettle-orchard-7", false); !errors.Is(err, ErrClockUnverified) {
		t.Fatalf("unacknowledged clock: %v", err)
	}
	e.app.Author.ClockState = func() event.ClockState { return event.ClockVerified }
	e.must(e.app.Sign(e.ctx, tech, wo, domain.MeaningPerformed, "brass-kettle-orchard-7", false))

	sigs, err := domain.WorkOrderSignatures(e.ctx, e.st.DB(), wo)
	e.must(err)
	if len(sigs) != 1 || sigs[0].SignerLegalName != "Tess" || sigs[0].Meaning != domain.MeaningPerformed ||
		!sigs[0].SignedAt.Equal(e.now) || sigs[0].ClockState != "verified" || sigs[0].Stale || sigs[0].Status != "valid" {
		t.Fatalf("signatures = %+v", sigs)
	}
	w, _ := domain.GetWorkOrder(e.ctx, e.st.DB(), wo)
	if w.Status != domain.WOCompleted {
		t.Fatalf("status after performed signature = %s", w.Status)
	}
}

func TestSignatureLockout(t *testing.T) {
	e := newEnv(t)
	mid, tech := e.activeUser("mona", domain.RoleMidTier), e.activeUser("tess", domain.RoleUser)
	wo := e.openHeld(mid, tech, "pm", e.asset("A1"))
	for i := 0; i < MaxFailedLogins; i++ {
		e.app.Sign(e.ctx, tech, wo, domain.MeaningPerformed, "wrong-password", true)
	}
	if err := e.sign(tech, wo, domain.MeaningPerformed); !errors.Is(err, ErrLocked) {
		t.Fatalf("signing after five wrong passwords: %v", err)
	}
}

func TestReopenMakesSignaturesStale(t *testing.T) {
	e := newEnv(t)
	c := e.calSetup()
	rec, err := e.app.RecordCalibration(e.ctx, c.tech, calibration(c.wo, []string{c.standard}, false, voltage("120.1", "")))
	e.must(err)
	e.must(e.sign(c.tech, c.wo, domain.MeaningPerformed))

	// The reviewer finds a problem and reopens; the calibration is redone.
	e.must(e.app.ChangeWorkOrderStatus(e.ctx, c.mid, c.wo, domain.WOInProgress, "standard serial wrong"))
	// The old signature no longer counts: completing needs a new one.
	if err := e.app.ChangeWorkOrderStatus(e.ctx, c.tech, c.wo, domain.WOCompleted, ""); err == nil {
		t.Fatal("re-completed without a new signature")
	}
	e.must(e.app.VoidCalibration(e.ctx, c.tech, rec, "standard serial wrong"))
	_, err = e.app.RecordCalibration(e.ctx, c.tech, calibration(c.wo, []string{c.standard}, false, voltage("120.2", "")))
	e.must(err)
	e.must(e.sign(c.tech, c.wo, domain.MeaningPerformed))
	e.must(e.sign(c.mid, c.wo, domain.MeaningReviewed))
	e.must(e.sign(c.mid, c.wo, domain.MeaningApproved))

	sigs, err := domain.WorkOrderSignatures(e.ctx, e.st.DB(), c.wo)
	e.must(err)
	var got []string
	for _, s := range sigs {
		state := "current"
		if s.Stale {
			state = "stale"
		}
		got = append(got, s.Meaning+"/"+state)
	}
	if !reflect.DeepEqual(got, []string{"performed/stale", "performed/current", "reviewed/current", "approved/current"}) {
		t.Fatalf("signatures = %v", got)
	}
}

func TestForgedSignaturesRejected(t *testing.T) {
	e := newEnv(t)
	mid, tech := e.activeUser("mona", domain.RoleMidTier), e.activeUser("tess", domain.RoleUser)
	wo := e.openHeld(mid, tech, "pm", e.asset("A1"))
	w, _ := domain.GetWorkOrder(e.ctx, e.st.DB(), wo)
	hash, err := domain.WorkOrderContentHash(e.ctx, e.st.DB(), wo)
	e.must(err)
	good := domain.SignatureApplied{
		TargetType: domain.EntityWorkOrder, TargetID: wo, Meaning: domain.MeaningPerformed, MeaningText: domain.MeaningText[domain.MeaningPerformed],
		ContentHash: hash, SignerLegalName: "Tess", SignerUsername: "tess", AuthMethod: "password",
		AuthAt: e.now.UTC().Format(time.RFC3339), ClockWarningAcknowledged: true,
	}
	tests := []struct {
		name   string
		mutate func(*domain.SignatureApplied)
		flag   string
	}{
		{"content changed", func(s *domain.SignatureApplied) { s.ContentHash = strings.Repeat("a", 64) }, domain.FlagStaleBase},
		{"someone else's name", func(s *domain.SignatureApplied) { s.SignerLegalName = "Mona" }, domain.FlagInvalid},
		{"stale re-authentication", func(s *domain.SignatureApplied) {
			s.AuthAt = e.now.Add(-5 * time.Minute).UTC().Format(time.RFC3339)
		}, domain.FlagInvalid},
		{"no password", func(s *domain.SignatureApplied) { s.AuthMethod = "none" }, domain.FlagInvalid},
		{"clock warning not acknowledged", func(s *domain.SignatureApplied) { s.ClockWarningAcknowledged = false }, domain.FlagInvalid},
		{"altered meaning text", func(s *domain.SignatureApplied) { s.MeaningText = "I did nothing." }, domain.FlagInvalid},
	}
	r := e.remote()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := good
			tt.mutate(&p)
			ev := r.write(e, tech, domain.TypeSignatureApplied, domain.EntitySignature, newID(), 0, w.LeaseID, p)
			e.must(e.st.Ingest(e.ctx, ev))
			if got := e.flags(ev); !reflect.DeepEqual(got, []string{tt.flag + "/false"}) {
				t.Fatalf("flags = %v", got)
			}
		})
	}
	// A signature by someone without the lease.
	ev := r.write(e, mid, domain.TypeSignatureApplied, domain.EntitySignature, newID(), 0, "",
		func() domain.SignatureApplied { p := good; p.SignerLegalName, p.SignerUsername = "Mona", "mona"; return p }())
	e.must(e.st.Ingest(e.ctx, ev))
	if got := e.flags(ev); !reflect.DeepEqual(got, []string{"non_authorized/false"}) {
		t.Fatalf("non-holder flags = %v", got)
	}
}

func TestWithdrawSignature(t *testing.T) {
	e := newEnv(t)
	mid, tech := e.activeUser("mona", domain.RoleMidTier), e.activeUser("tess", domain.RoleUser)
	wo := e.openHeld(mid, tech, "pm", e.asset("A1"))
	e.must(e.sign(tech, wo, domain.MeaningPerformed))
	sigs, _ := domain.WorkOrderSignatures(e.ctx, e.st.DB(), wo)
	wantRejection(t, e.app.WithdrawSignature(e.ctx, mid, sigs[0].ID, "not mine to withdraw"), domain.FlagNotAuthorized)
	wantRejection(t, e.app.WithdrawSignature(e.ctx, tech, sigs[0].ID, ""), domain.FlagInvalid)
	e.must(e.app.WithdrawSignature(e.ctx, tech, sigs[0].ID, "signed the wrong work order"))
	// A withdrawn performed signature can't support review.
	e.must(e.app.ChangeWorkOrderStatus(e.ctx, mid, wo, domain.WOInProgress, "signature withdrawn"))
	sigs, _ = domain.WorkOrderSignatures(e.ctx, e.st.DB(), wo)
	if sigs[0].Status != "withdrawn" || sigs[0].WithdrawReason != "signed the wrong work order" {
		t.Fatalf("withdrawn signature = %+v", sigs[0])
	}
}
