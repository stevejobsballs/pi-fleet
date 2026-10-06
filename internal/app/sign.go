package app

import (
	"context"
	"errors"
	"time"

	"pi-fleet/internal/domain"
	"pi-fleet/internal/event"
	"pi-fleet/internal/store"
)

var (
	// ErrClockUnverified asks the signer to acknowledge that this Pi's
	// clock is unverified before signing (decision D5).
	ErrClockUnverified = errors.New("app: this Pi's clock is unverified; acknowledge the warning to sign anyway")
	ErrNotYourSession  = errors.New("app: the password does not belong to the signed-in user")
)

// ClockState is the clock state new events will carry.
func (a *App) ClockState() event.ClockState {
	if a.Author.ClockState == nil {
		return event.ClockUnverified
	}
	return a.Author.ClockState()
}

// Sign applies the actor's electronic signature to a work order with the
// given meaning and moves it forward: performed completes it, reviewed
// reviews it, approved closes it (DESIGN.md §7). The signer re-enters
// their password, and wrong passwords count towards lockout.
func (a *App) Sign(ctx context.Context, actor Actor, woID, meaning, pw string, acknowledgeClock bool) error {
	u, err := domain.GetUser(ctx, a.Store.DB(), actor.UserID)
	if err != nil {
		return err
	}
	authed, err := a.Authenticate(ctx, u.Username, pw)
	if err != nil {
		return err
	}
	if authed.ID != actor.UserID {
		return ErrNotYourSession
	}
	if a.ClockState() == event.ClockUnverified && !acknowledgeClock {
		return ErrClockUnverified
	}
	authAt := a.now().UTC().Format(time.RFC3339)
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		w, err := domain.GetWorkOrder(ctx, tx, woID)
		if err != nil {
			return err
		}
		hash, err := domain.WorkOrderContentHash(ctx, tx, woID)
		if err != nil {
			return err
		}
		lease := ""
		if w.AssignedTo == actor.UserID {
			lease = w.LeaseID
		}
		if err := a.emit(ctx, tx, actor, domain.TypeSignatureApplied, domain.EntitySignature, newID(), 0, lease, domain.SignatureApplied{
			TargetType: domain.EntityWorkOrder, TargetID: woID, Meaning: meaning, MeaningText: domain.MeaningText[meaning],
			ContentHash: hash, SignerLegalName: u.LegalName, SignerUsername: u.Username,
			AuthMethod: "password", AuthAt: authAt, ClockWarningAcknowledged: acknowledgeClock,
		}); err != nil {
			return err
		}
		to := map[string]string{domain.MeaningPerformed: domain.WOCompleted, domain.MeaningReviewed: domain.WOReviewed, domain.MeaningApproved: domain.WOClosed}[meaning]
		return a.emit(ctx, tx, actor, domain.TypeWorkOrderStatusChanged, domain.EntityWorkOrder, woID, w.Version, lease,
			domain.WorkOrderStatusChanged{From: w.Status, To: to})
	})
}

// WithdrawSignature withdraws the actor's own signature.
func (a *App) WithdrawSignature(ctx context.Context, actor Actor, signatureID, reason string) error {
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeSignatureWithdrawn, domain.EntitySignature, signatureID, 0, "",
			domain.SignatureWithdrawn{Reason: reason})
	})
}
