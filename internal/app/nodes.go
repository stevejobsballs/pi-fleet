package app

import (
	"context"

	"pi-fleet/internal/domain"
	"pi-fleet/internal/store"
)

// activation is the actor that records Pi activation requests on central.
var activation = Actor{UserID: domain.SystemActivation, SessionID: "activation"}

// RecordActivation records a Pi whose activation proof checked out. It
// stays pending until a super user confirms it (decision D15).
func (a *App) RecordActivation(ctx context.Context, nodeID string, p domain.NodeActivated) error {
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, activation, domain.TypeNodeActivated, domain.EntityNode, nodeID, 0, "", p)
	})
}

// ConfirmNode activates a pending Pi. pairingWords are the words the
// employee read off their Pi, which must match.
func (a *App) ConfirmNode(ctx context.Context, actor Actor, nodeID, pairingWords string) error {
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeNodeConfirmed, domain.EntityNode, nodeID, 0, "",
			domain.NodeConfirmed{PairingWords: pairingWords})
	})
}

// RejectNode refuses a pending Pi.
func (a *App) RejectNode(ctx context.Context, actor Actor, nodeID, reason string) error {
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeNodeRejected, domain.EntityNode, nodeID, 0, "", domain.NodeRejected{Reason: reason})
	})
}

// RevokeNode stops a Pi syncing; it wipes itself on next contact.
func (a *App) RevokeNode(ctx context.Context, actor Actor, nodeID, reason string, keepUnsynced bool) error {
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeNodeRevoked, domain.EntityNode, nodeID, 0, "",
			domain.NodeRevoked{Reason: reason, KeepUnsynced: keepUnsynced})
	})
}
