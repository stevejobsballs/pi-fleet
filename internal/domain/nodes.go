package domain

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"pi-fleet/internal/event"
	"pi-fleet/internal/pairing"
	"pi-fleet/internal/password"
	"pi-fleet/internal/store"
)

// Node statuses.
const (
	NodeStatusPending  = "pending_confirmation"
	NodeStatusActive   = "active"
	NodeStatusRejected = "rejected"
	NodeStatusRevoked  = "revoked"
)

// Node is a projected Pi.
type Node struct {
	ID             string
	Mode           string
	BoundUserID    string
	EventPub       ed25519.PublicKey
	TransportPub   ed25519.PublicKey
	TransportKeyID string
	PairingWords   string
	Status         string
	ActivatedAt    string
	KeepUnsynced   bool
	RevokedHLC     int64  // 0 unless revoked
	KioskID        string // kiosk mode; empty for a personal Pi
	Version        int64
}

const nodeColumns = `id, mode, bound_user_id, event_pub, transport_pub, transport_key_id, pairing_words,
	status, activated_at, keep_unsynced, coalesce(revoked_hlc, 0), kiosk_id, version`

func scanNode(row interface{ Scan(...any) error }) (Node, error) {
	var n Node
	var ev, tr []byte
	err := row.Scan(&n.ID, &n.Mode, &n.BoundUserID, &ev, &tr, &n.TransportKeyID, &n.PairingWords,
		&n.Status, &n.ActivatedAt, &n.KeepUnsynced, &n.RevokedHLC, &n.KioskID, &n.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return Node{}, ErrNotFound
	}
	n.EventPub, n.TransportPub = ev, tr
	return n, err
}

// GetNode returns a node by id.
func GetNode(ctx context.Context, q Querier, id string) (Node, error) {
	return scanNode(q.QueryRowContext(ctx, `SELECT `+nodeColumns+` FROM nodes WHERE id = ?`, id))
}

// GetNodeByTransportKey returns the node that owns a transport key id.
func GetNodeByTransportKey(ctx context.Context, q Querier, keyID string) (Node, error) {
	return scanNode(q.QueryRowContext(ctx, `SELECT `+nodeColumns+` FROM nodes WHERE transport_key_id = ?`, keyID))
}

// ListNodes returns nodes with the given status, or all if status is "".
func ListNodes(ctx context.Context, q Querier, status string) ([]Node, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+nodeColumns+` FROM nodes WHERE ? = '' OR status = ? ORDER BY activated_at`, status, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// checkNodeBinding enforces that a Pi records events only for the user
// it is bound to (DESIGN.md S3), and nothing after it was revoked.
// Nodes that aren't in the table (central itself) are not checked here.
func checkNodeBinding(ctx context.Context, tx *sql.Tx, e *event.Event) error {
	n, err := GetNode(ctx, tx, e.NodeID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	switch n.Status {
	case NodeStatusActive:
	case NodeStatusRevoked:
		if !n.KeepUnsynced || e.HLC >= n.RevokedHLC {
			return store.Reject(FlagNotAuthorized, "node %s was revoked", n.ID)
		}
	default:
		return store.Reject(FlagNotAuthorized, "node %s is %s", n.ID, n.Status)
	}
	if e.ActorUserID == SystemAuth {
		return nil
	}
	if n.Mode == "kiosk" {
		ok, err := kioskMemberAt(ctx, tx, n.KioskID, e.ActorUserID, e)
		if err != nil {
			return err
		}
		if !ok {
			return store.Reject(FlagNotAuthorized, "user is not a member of kiosk %s", n.KioskID)
		}
		return nil
	}
	if e.ActorUserID != n.BoundUserID {
		return store.Reject(FlagNotAuthorized, "node %s is bound to another user", n.ID)
	}
	return nil
}

func decodeKey(s string) (ed25519.PublicKey, error) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("public keys must be 64 hex characters")
	}
	return b, nil
}

func (ap *applier) nodeActivated(p *NodeActivated) error {
	if err := ap.newEntity("nodes"); err != nil {
		return err
	}
	var userID string
	switch p.Mode {
	case "personal":
		u, err := GetUser(ap.ctx, ap.tx, p.UserID)
		if errors.Is(err, ErrNotFound) {
			return invalid("user %s not found", p.UserID)
		}
		if err != nil {
			return err
		}
		if u.Status == UserStatusDisabled || !u.MustChangePassword || u.PasswordExpired(ap.e.WallTime) {
			return store.Reject(FlagNotAuthorized, "user %s has no unexpired one-time password", u.Username)
		}
		if ok, err := ap.exists(`SELECT 1 FROM nodes WHERE bound_user_id = ? AND mode = 'personal' AND status IN (?, ?)`,
			u.ID, NodeStatusPending, NodeStatusActive); err != nil || ok {
			return orConflict(err, "user %s already has a Pi (one personal Pi per user)", u.Username)
		}
		if _, err := password.Parse(p.PendingVerifier); err != nil {
			return invalid("pending verifier: %v", err)
		}
		userID = u.ID
	case "kiosk":
		var verifier, expires string
		err := ap.tx.QueryRowContext(ap.ctx, `SELECT activation_verifier, activation_expires_at FROM kiosks WHERE id = ?`, p.KioskID).Scan(&verifier, &expires)
		if errors.Is(err, sql.ErrNoRows) {
			return invalid("kiosk %s not found", p.KioskID)
		}
		if err != nil {
			return err
		}
		exp, _ := time.Parse(time.RFC3339, expires)
		if verifier == "" || !ap.e.WallTime.Before(exp) {
			return store.Reject(FlagNotAuthorized, "kiosk has no unexpired one-time activation password")
		}
		if ok, err := ap.exists(`SELECT 1 FROM nodes WHERE kiosk_id = ? AND status IN (?, ?)`, p.KioskID, NodeStatusPending, NodeStatusActive); err != nil || ok {
			return orConflict(err, "this kiosk already has a Pi")
		}
		if p.PendingVerifier != "" || p.UserID != "" {
			return invalid("kiosk activations carry no user or password")
		}
	default:
		return invalid("unknown node mode %q", p.Mode)
	}
	evPub, err := decodeKey(p.EventPublicKey)
	if err != nil {
		return invalid("event key: %v", err)
	}
	trPub, err := decodeKey(p.TransportPublicKey)
	if err != nil {
		return invalid("transport key: %v", err)
	}
	if evPub.Equal(trPub) {
		return invalid("event and transport keys must differ")
	}
	if p.PairingWords != pairing.Words(evPub, trPub) {
		return invalid("pairing words do not match the keys")
	}
	trID := event.KeyID(trPub)
	if ok, err := ap.exists(`SELECT 1 FROM nodes WHERE transport_key_id = ?`, trID); err != nil || ok {
		return orConflict(err, "transport key already registered")
	}
	return ap.exec(`INSERT INTO nodes (id, mode, bound_user_id, kiosk_id, event_pub, transport_pub, transport_key_id, pairing_words,
			status, activated_at, confirmed_by, pending_verifier, version, last_event_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, 1, ?)`,
		ap.e.EntityID, p.Mode, userID, p.KioskID, []byte(evPub), []byte(trPub), trID, p.PairingWords,
		NodeStatusPending, ap.wall(), p.PendingVerifier, ap.e.EventID)
}

func (ap *applier) targetNode(status ...string) (Node, error) {
	n, err := GetNode(ap.ctx, ap.tx, ap.e.EntityID)
	if errors.Is(err, ErrNotFound) {
		return Node{}, invalid("node %s not found", ap.e.EntityID)
	}
	if err != nil {
		return Node{}, err
	}
	for _, s := range status {
		if n.Status == s {
			return n, nil
		}
	}
	return Node{}, invalid("node %s is %s", n.ID, n.Status)
}

func (ap *applier) bumpNode(set string, args ...any) error {
	args = append(args, ap.e.EventID, ap.e.EntityID)
	return ap.exec(`UPDATE nodes SET `+set+`, version = version + 1, last_event_id = ? WHERE id = ?`, args...)
}

func (ap *applier) nodeConfirmed(p *NodeConfirmed) error {
	if err := ap.require(RoleSuperUser); err != nil {
		return err
	}
	n, err := ap.targetNode(NodeStatusPending)
	if err != nil {
		return err
	}
	if p.PairingWords != n.PairingWords {
		return invalid("the pairing words read out do not match this Pi")
	}
	var pending string
	if err := ap.tx.QueryRowContext(ap.ctx, `SELECT pending_verifier FROM nodes WHERE id = ?`, n.ID).Scan(&pending); err != nil {
		return err
	}
	if err := ap.bumpNode(`status = ?, confirmed_by = ?, pending_verifier = ''`, NodeStatusActive, ap.actor.id); err != nil {
		return err
	}
	if _, err := ap.tx.ExecContext(ap.ctx, `INSERT INTO node_keys (node_id, key_id, public_key, added_at)
		VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`, n.ID, event.KeyID(n.EventPub), []byte(n.EventPub), ap.wall()); err != nil {
		return err
	}
	if n.Mode == "kiosk" {
		// The kiosk's activation password is single use.
		return ap.exec(`UPDATE kiosks SET activation_verifier = '' WHERE id = ?`, n.KioskID)
	}
	// The password the employee chose while activating now takes effect.
	if err := ap.exec(`UPDATE users SET verifier = ?, must_change_password = 0, status = ?, password_changed_at = ?,
			password_expires_at = ?, version = version + 1, last_event_id = ? WHERE id = ?`,
		pending, UserStatusActive, ap.wall(), ap.e.WallTime.Add(password.Lifetime).UTC().Format(time.RFC3339),
		ap.e.EventID, n.BoundUserID); err != nil {
		return err
	}
	return ap.pushHistoryFor(n.BoundUserID, pending)
}

func (ap *applier) nodeRejected(p *NodeRejected) error {
	if err := ap.require(RoleSuperUser); err != nil {
		return err
	}
	if _, err := ap.targetNode(NodeStatusPending); err != nil {
		return err
	}
	if blank(p.Reason) {
		return invalid("rejecting a Pi requires a reason")
	}
	return ap.bumpNode(`status = ?, pending_verifier = ''`, NodeStatusRejected)
}

func (ap *applier) nodeRevoked(p *NodeRevoked) error {
	if err := ap.require(RoleSuperUser); err != nil {
		return err
	}
	if _, err := ap.targetNode(NodeStatusPending, NodeStatusActive); err != nil {
		return err
	}
	if blank(p.Reason) {
		return invalid("revoking a Pi requires a reason")
	}
	return ap.bumpNode(`status = ?, revoked_hlc = ?, keep_unsynced = ?, pending_verifier = ''`, NodeStatusRevoked, ap.e.HLC, p.KeepUnsynced)
}
