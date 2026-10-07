package domain

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"time"

	"pi-fleet/internal/event"
	"pi-fleet/internal/password"
)

// Kiosk event types (DESIGN.md §6.3, kiosk mode).
const (
	TypeKioskCreated         = "kiosk.created"
	TypeKioskMemberAdded     = "kiosk.member_added"
	TypeKioskMemberRemoved   = "kiosk.member_removed"
	TypeKioskActivationReset = "kiosk.activation_reset"
	EntityKiosk              = "kiosk"

	// KioskPrefix marks a kiosk's name where a username is expected
	// during activation.
	KioskPrefix = "kiosk:"
)

var kioskNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,31}$`)

// KioskCreated makes a kiosk group at a site with a one-time activation
// password (its verifier) for the shared Pi.
type KioskCreated struct {
	SiteID             string `json:"site_id"`
	Name               string `json:"name"`
	ActivationVerifier string `json:"activation_verifier"`
}

type KioskMember struct {
	UserID string `json:"user_id"`
}

// KioskActivationReset issues a new one-time activation password, e.g.
// for a replacement kiosk Pi.
type KioskActivationReset struct {
	ActivationVerifier string `json:"activation_verifier"`
}

func init() {
	reg := func(typ string, f func() any) {
		payloadTypes[typ] = struct {
			entity string
			new    func() any
		}{EntityKiosk, f}
	}
	reg(TypeKioskCreated, func() any { return &KioskCreated{} })
	reg(TypeKioskMemberAdded, func() any { return &KioskMember{} })
	reg(TypeKioskMemberRemoved, func() any { return &KioskMember{} })
	reg(TypeKioskActivationReset, func() any { return &KioskActivationReset{} })
}

func (ap *applier) kioskCreated(p *KioskCreated) error {
	if err := ap.require(RoleSuperUser); err != nil {
		return err
	}
	if err := ap.newEntity("kiosks"); err != nil {
		return err
	}
	if !kioskNameRE.MatchString(p.Name) {
		return invalid("kiosk name %q must be 3-32 lowercase letters, digits or dashes", p.Name)
	}
	if ok, err := ap.exists(`SELECT 1 FROM sites WHERE id = ?`, p.SiteID); err != nil || !ok {
		return orInvalid(err, "site %s not found", p.SiteID)
	}
	if ok, err := ap.exists(`SELECT 1 FROM kiosks WHERE name = ?`, p.Name); err != nil || ok {
		return orConflict(err, "kiosk %s already exists", p.Name)
	}
	if _, err := password.Parse(p.ActivationVerifier); err != nil {
		return invalid("activation verifier: %v", err)
	}
	return ap.exec(`INSERT INTO kiosks (id, site_id, name, activation_verifier, activation_expires_at, version, last_event_id)
		VALUES (?, ?, ?, ?, ?, 1, ?)`, ap.e.EntityID, p.SiteID, p.Name, p.ActivationVerifier,
		ap.e.WallTime.Add(password.TemporaryLifetime).UTC().Format(time.RFC3339), ap.e.EventID)
}

func (ap *applier) targetKiosk() error {
	if ok, err := ap.exists(`SELECT 1 FROM kiosks WHERE id = ?`, ap.e.EntityID); err != nil || !ok {
		return orInvalid(err, "kiosk %s not found", ap.e.EntityID)
	}
	return nil
}

func (ap *applier) kioskMemberAdded(p *KioskMember) error {
	if err := ap.require(RoleSuperUser); err != nil {
		return err
	}
	if err := ap.targetKiosk(); err != nil {
		return err
	}
	// New users may join before choosing their password: they sign in on
	// the kiosk with their one-time password and must change it there.
	if ok, err := ap.exists(`SELECT 1 FROM users WHERE id = ? AND status != ?`, p.UserID, UserStatusDisabled); err != nil || !ok {
		return orInvalid(err, "user %s is not an enabled user", p.UserID)
	}
	if ok, err := ap.exists(`SELECT 1 FROM kiosk_members WHERE kiosk_id = ? AND user_id = ? AND removed_hlc IS NULL`, ap.e.EntityID, p.UserID); err != nil || ok {
		return orConflict(err, "already a member")
	}
	return ap.exec(`INSERT INTO kiosk_members (kiosk_id, user_id, removed_hlc) VALUES (?, ?, NULL)
		ON CONFLICT (kiosk_id, user_id) DO UPDATE SET removed_hlc = NULL`, ap.e.EntityID, p.UserID)
}

func (ap *applier) kioskMemberRemoved(p *KioskMember) error {
	if err := ap.require(RoleSuperUser); err != nil {
		return err
	}
	if ok, err := ap.exists(`SELECT 1 FROM kiosk_members WHERE kiosk_id = ? AND user_id = ? AND removed_hlc IS NULL`, ap.e.EntityID, p.UserID); err != nil || !ok {
		return orInvalid(err, "not a member")
	}
	return ap.exec(`UPDATE kiosk_members SET removed_hlc = ? WHERE kiosk_id = ? AND user_id = ?`, ap.e.HLC, ap.e.EntityID, p.UserID)
}

func (ap *applier) kioskActivationReset(p *KioskActivationReset) error {
	if err := ap.require(RoleSuperUser); err != nil {
		return err
	}
	if err := ap.targetKiosk(); err != nil {
		return err
	}
	if _, err := password.Parse(p.ActivationVerifier); err != nil {
		return invalid("activation verifier: %v", err)
	}
	return ap.exec(`UPDATE kiosks SET activation_verifier = ?, activation_expires_at = ?, version = version + 1, last_event_id = ? WHERE id = ?`,
		p.ActivationVerifier, ap.e.WallTime.Add(password.TemporaryLifetime).UTC().Format(time.RFC3339), ap.e.EventID, ap.e.EntityID)
}

// kioskMemberAt reports whether user was a member of kiosk when e was
// made: a current member, or one removed after the event (offline grace).
func kioskMemberAt(ctx context.Context, tx *sql.Tx, kioskID, userID string, e *event.Event) (bool, error) {
	var removed sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT removed_hlc FROM kiosk_members WHERE kiosk_id = ? AND user_id = ?`, kioskID, userID).Scan(&removed)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !removed.Valid || e.HLC < removed.Int64, nil
}

// Kiosk is a projected kiosk group.
type Kiosk struct {
	ID, SiteID, Name   string
	ActivationVerifier string
	ActivationExpires  time.Time
}

// GetKioskByName returns a kiosk by name.
func GetKioskByName(ctx context.Context, q Querier, name string) (Kiosk, error) {
	var k Kiosk
	var exp string
	err := q.QueryRowContext(ctx, `SELECT id, site_id, name, activation_verifier, activation_expires_at FROM kiosks WHERE name = ?`, name).
		Scan(&k.ID, &k.SiteID, &k.Name, &k.ActivationVerifier, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return k, ErrNotFound
	}
	k.ActivationExpires, _ = time.Parse(time.RFC3339, exp)
	return k, err
}

// KioskMembers returns the current members' user ids.
func KioskMembers(ctx context.Context, q Querier, kioskID string) ([]string, error) {
	return queryStrings(ctx, q, `SELECT user_id FROM kiosk_members WHERE kiosk_id = ? AND removed_hlc IS NULL ORDER BY user_id`, kioskID)
}
