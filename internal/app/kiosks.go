package app

import (
	"context"

	"pi-fleet/internal/domain"
	"pi-fleet/internal/password"
	"pi-fleet/internal/store"
)

// CreateKiosk makes a kiosk group for a shared Pi at a site and returns
// its one-time activation password, shown once.
func (a *App) CreateKiosk(ctx context.Context, actor Actor, siteID, name string) (id, activationPassword string, err error) {
	v, temp, err := a.temporaryVerifier()
	if err != nil {
		return "", "", err
	}
	id = newID()
	return id, temp, a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeKioskCreated, domain.EntityKiosk, id, 0, "",
			domain.KioskCreated{SiteID: siteID, Name: name, ActivationVerifier: v})
	})
}

// ResetKioskActivation issues a new one-time activation password, e.g.
// for a replacement kiosk Pi.
func (a *App) ResetKioskActivation(ctx context.Context, actor Actor, kioskID string) (string, error) {
	v, temp, err := a.temporaryVerifier()
	if err != nil {
		return "", err
	}
	return temp, a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeKioskActivationReset, domain.EntityKiosk, kioskID, 0, "",
			domain.KioskActivationReset{ActivationVerifier: v})
	})
}

// AddKioskMember lets a user sign in on a kiosk.
func (a *App) AddKioskMember(ctx context.Context, actor Actor, kioskID, userID string) error {
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeKioskMemberAdded, domain.EntityKiosk, kioskID, 0, "", domain.KioskMember{UserID: userID})
	})
}

// RemoveKioskMember stops a user signing in on a kiosk from its next sync.
func (a *App) RemoveKioskMember(ctx context.Context, actor Actor, kioskID, userID string) error {
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeKioskMemberRemoved, domain.EntityKiosk, kioskID, 0, "", domain.KioskMember{UserID: userID})
	})
}

func (a *App) temporaryVerifier() (verifier, temp string, err error) {
	if temp, err = password.GenerateTemporary(); err != nil {
		return "", "", err
	}
	v, err := password.Hash(temp, a.Params)
	return v.String(), temp, err
}
