// Package app implements the commands users and administrators issue.
// Each command reads current state, checks what needs the plaintext
// input (passwords), and appends events in one transaction; the domain
// projector then re-validates every event exactly as central will.
package app

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"pi-fleet/internal/domain"
	"pi-fleet/internal/event"
	"pi-fleet/internal/password"
	"pi-fleet/internal/store"
)

var (
	ErrBadCredentials     = errors.New("app: wrong username or password")
	ErrMustChangePassword = errors.New("app: password must be changed before continuing")
	ErrTemporaryExpired   = errors.New("app: one-time password has expired; ask a super user for a new one")
	ErrAlreadyBootstrap   = errors.New("app: users already exist; the console bootstrap can only create the first super user")
	ErrNotLeaseHolder     = errors.New("app: you do not hold this work order")
	ErrLocked             = errors.New("app: account is locked; try again later or ask a super user")
)

// MaxFailedLogins is how many wrong passwords in a row lock an account
// (DESIGN.md §6.5).
const MaxFailedLogins = 5

// auth is the actor that records lockouts.
var auth = Actor{UserID: domain.SystemAuth, SessionID: "auth"}

// App runs commands as one node.
type App struct {
	Store  *store.Store
	Author *store.Author
	// Params are the Argon2id costs for new verifiers.
	Params password.Params
	// Now defaults to time.Now.
	Now func() time.Time
}

// Actor is the authenticated user issuing a command.
type Actor struct {
	UserID    string
	SessionID string
}

// Console is the actor for bootstrap commands at the device console.
var Console = Actor{UserID: domain.SystemConsole, SessionID: "console"}

func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func newID() string { return uuid.Must(uuid.NewV7()).String() }

func draft(actor Actor, typ, entityType, entityID string, base int64, lease string, payload any) (event.Draft, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return event.Draft{}, err
	}
	return event.Draft{
		ActorUserID:    actor.UserID,
		ActorSessionID: actor.SessionID,
		Type:           typ,
		EntityType:     entityType,
		EntityID:       entityID,
		BaseVersion:    base,
		LeaseID:        lease,
		SchemaVersion:  1,
		Payload:        body,
	}, nil
}

// emit appends one event built from payload.
func (a *App) emit(ctx context.Context, tx *store.Tx, actor Actor, typ, entityType, entityID string, base int64, lease string, payload any) error {
	d, err := draft(actor, typ, entityType, entityID, base, lease, payload)
	if err != nil {
		return err
	}
	_, err = tx.Append(ctx, a.Author, d)
	return err
}

// --- users and passwords ---

// NewUser describes an account to create.
type NewUser struct {
	Username             string
	LegalName            string
	Email                string
	Role                 string
	HomeSites            []string
	IdentityVerification string // how the super user verified identity
}

// BootstrapSuperUser creates the first super user from the device
// console with a password they choose (DESIGN.md §6.5).
func (a *App) BootstrapSuperUser(ctx context.Context, u NewUser, pw string) (string, error) {
	if err := password.CheckPolicy(pw, u.Username); err != nil {
		return "", err
	}
	v, err := password.Hash(pw, a.Params)
	if err != nil {
		return "", err
	}
	id := newID()
	u.Role = domain.RoleSuperUser
	err = a.Store.Update(ctx, func(tx *store.Tx) error {
		if n, err := domain.CountUsers(ctx, tx); err != nil || n > 0 {
			return errors.Join(err, ErrAlreadyBootstrap)
		}
		return a.emit(ctx, tx, Console, domain.TypeUserCreated, domain.EntityUser, id, 0, "", userCreated(u, v, false))
	})
	return id, err
}

func userCreated(u NewUser, v password.Verifier, temporary bool) domain.UserCreated {
	return domain.UserCreated{
		Username:             u.Username,
		LegalName:            u.LegalName,
		Email:                u.Email,
		Role:                 u.Role,
		HomeSites:            u.HomeSites,
		IdentityVerification: u.IdentityVerification,
		Verifier:             v.String(),
		Temporary:            temporary,
	}
}

// CreateUser creates an account with a generated one-time activation
// password, which is returned once for the super user to hand over
// (DESIGN.md §6.3).
func (a *App) CreateUser(ctx context.Context, actor Actor, u NewUser) (id, activationPassword string, err error) {
	activationPassword, err = password.GenerateTemporary()
	if err != nil {
		return "", "", err
	}
	v, err := password.Hash(activationPassword, a.Params)
	if err != nil {
		return "", "", err
	}
	id = newID()
	err = a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeUserCreated, domain.EntityUser, id, 0, "", userCreated(u, v, true))
	})
	if err != nil {
		return "", "", err
	}
	return id, activationPassword, nil
}

// ChangeRole sets a user's role.
func (a *App) ChangeRole(ctx context.Context, actor Actor, userID, role string) error {
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeUserRoleChanged, domain.EntityUser, userID, 0, "", domain.UserRoleChanged{Role: role})
	})
}

// DisableUser disables an account. Accounts are never deleted.
func (a *App) DisableUser(ctx context.Context, actor Actor, userID, reason string) error {
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeUserDisabled, domain.EntityUser, userID, 0, "", domain.UserDisabled{Reason: reason})
	})
}

// ResetPassword issues a new one-time password for a user whose identity
// the super user has verified.
func (a *App) ResetPassword(ctx context.Context, actor Actor, userID, identityVerification string) (string, error) {
	temp, err := password.GenerateTemporary()
	if err != nil {
		return "", err
	}
	v, err := password.Hash(temp, a.Params)
	if err != nil {
		return "", err
	}
	err = a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeUserPasswordReset, domain.EntityUser, userID, 0, "",
			domain.UserPasswordReset{Verifier: v.String(), IdentityVerification: identityVerification})
	})
	if err != nil {
		return "", err
	}
	return temp, nil
}

// ChangePassword replaces the actor's own password. It works offline:
// only the new verifier is recorded (DESIGN.md §6.5).
func (a *App) ChangePassword(ctx context.Context, actor Actor, current, next string) error {
	u, err := domain.GetUser(ctx, a.Store.DB(), actor.UserID)
	if err != nil {
		return err
	}
	if err := a.checkPassword(u, current); err != nil && !errors.Is(err, ErrMustChangePassword) {
		return err
	}
	if err := password.CheckPolicy(next, u.Username); err != nil {
		return err
	}
	history, err := domain.PasswordHistory(ctx, a.Store.DB(), u.ID)
	if err != nil {
		return err
	}
	if err := password.CheckHistory(next, history); err != nil {
		return err
	}
	v, err := password.Hash(next, a.Params)
	if err != nil {
		return err
	}
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeUserPasswordChanged, domain.EntityUser, u.ID, u.Version, "",
			domain.UserPasswordChanged{Verifier: v.String()})
	})
}

// dummy keeps the timing of unknown-username logins close to real ones.
var dummy, _ = password.Hash("pi-fleet-dummy-password", password.Params{Time: 1, MemoryKiB: 64, Threads: 1})

// Authenticate checks a username and password. If the password must be
// changed (one-time password, or expired after 31 days) it returns the
// user together with ErrMustChangePassword: the session may then only be
// used to change the password. Five wrong passwords in a row lock the
// account for 15 minutes; three lockouts in 24 hours lock it until a
// super user unlocks it.
func (a *App) Authenticate(ctx context.Context, username, pw string) (domain.User, error) {
	u, err := domain.GetUserByUsername(ctx, a.Store.DB(), username)
	if errors.Is(err, domain.ErrNotFound) {
		dummy.Check(pw)
		return domain.User{}, ErrBadCredentials
	}
	if err != nil {
		return domain.User{}, err
	}
	err = a.checkPassword(u, pw)
	if u.Locked(a.now()) {
		return domain.User{}, ErrLocked
	}
	switch {
	case errors.Is(err, ErrBadCredentials):
		if lerr := a.recordFailure(ctx, u); lerr != nil {
			return domain.User{}, errors.Join(err, lerr)
		}
		return domain.User{}, err
	case err != nil && !errors.Is(err, ErrMustChangePassword):
		return domain.User{}, err
	}
	if cerr := a.Store.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM login_failures WHERE username = ?`, u.Username)
		return err
	}); cerr != nil {
		return domain.User{}, cerr
	}
	if err != nil {
		return u, err
	}
	return u, nil
}

// recordFailure counts a failed login on this device and locks the
// account at MaxFailedLogins.
func (a *App) recordFailure(ctx context.Context, u domain.User) error {
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		var n int
		err := tx.QueryRowContext(ctx, `INSERT INTO login_failures (username, failures, last_failure_at) VALUES (?, 1, ?)
			ON CONFLICT (username) DO UPDATE SET failures = failures + 1, last_failure_at = excluded.last_failure_at
			RETURNING failures`, u.Username, a.now().UTC().Format(time.RFC3339)).Scan(&n)
		if err != nil || n < MaxFailedLogins {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM login_failures WHERE username = ?`, u.Username); err != nil {
			return err
		}
		return a.emit(ctx, tx, auth, domain.TypeUserLocked, domain.EntityUser, u.ID, 0, "",
			domain.UserLocked{FailedAttempts: n, Reason: "too many failed logins"})
	})
}

func (a *App) checkPassword(u domain.User, pw string) error {
	v, err := password.Parse(u.Verifier)
	if err != nil {
		// No usable verifier: on an employee Pi, every account but the
		// Pi's own user has its verifier withheld by central.
		dummy.Check(pw)
		return ErrBadCredentials
	}
	if !v.Check(pw) || u.Status == domain.UserStatusDisabled {
		return ErrBadCredentials
	}
	expired := u.PasswordExpired(a.now())
	switch {
	case u.MustChangePassword && expired:
		return ErrTemporaryExpired
	case u.MustChangePassword || expired:
		return ErrMustChangePassword
	}
	return nil
}

// --- sites and locations ---

// CreateSite creates a site.
func (a *App) CreateSite(ctx context.Context, actor Actor, code, name, timezone string) (string, error) {
	id := newID()
	return id, a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeSiteCreated, domain.EntitySite, id, 0, "",
			domain.SiteCreated{Code: code, Name: name, Timezone: timezone})
	})
}

// CreateLocation creates a location at a site, optionally under a parent.
func (a *App) CreateLocation(ctx context.Context, actor Actor, siteID, parentID, name, kind string) (string, error) {
	id := newID()
	return id, a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeLocationCreated, domain.EntityLocation, id, 0, "",
			domain.LocationCreated{SiteID: siteID, ParentID: parentID, Name: name, Kind: kind})
	})
}

// --- assets ---

// RegisterAsset registers a piece of equipment.
func (a *App) RegisterAsset(ctx context.Context, actor Actor, p domain.AssetRegistered) (string, error) {
	id := newID()
	return id, a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeAssetRegistered, domain.EntityAsset, id, 0, "", p)
	})
}

// UpdateAsset changes the fields present in p. baseVersion is the asset
// version the user was looking at.
func (a *App) UpdateAsset(ctx context.Context, actor Actor, assetID string, baseVersion int64, p domain.AssetUpdated) error {
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeAssetUpdated, domain.EntityAsset, assetID, baseVersion, "", p)
	})
}

// RelocateAsset moves an asset to another location, at any site.
func (a *App) RelocateAsset(ctx context.Context, actor Actor, assetID string, baseVersion int64, locationID string) error {
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeAssetRelocated, domain.EntityAsset, assetID, baseVersion, "",
			domain.AssetRelocated{LocationID: locationID})
	})
}

// SetAssetStatus changes an asset's service status.
func (a *App) SetAssetStatus(ctx context.Context, actor Actor, assetID string, baseVersion int64, status, reason string) error {
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeAssetStatusChanged, domain.EntityAsset, assetID, baseVersion, "",
			domain.AssetStatusChanged{Status: status, Reason: reason})
	})
}

// --- work orders ---

// NewWorkOrder describes a work order to open.
type NewWorkOrder struct {
	Type     string
	AssetID  string
	Priority string
	Title    string
	Problem  string
	DueAt    string

	scheduleID string // set by the scheduler
}

// NodeShortCode is a short code for a node used in human-facing numbers
// (DESIGN.md §4.1): four Crockford base32 characters of the node id hash.
func NodeShortCode(nodeID string) string {
	sum := sha256.Sum256([]byte(nodeID))
	enc := base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)
	return enc.EncodeToString(sum[:])[:4]
}

// OpenWorkOrder opens a work order numbered SITE-WO-NODE-NNNNN, unique
// across the fleet without coordination.
func (a *App) OpenWorkOrder(ctx context.Context, actor Actor, w NewWorkOrder) (id, number string, err error) {
	id = newID()
	err = a.Store.Update(ctx, func(tx *store.Tx) error {
		var siteCode string
		err := tx.QueryRowContext(ctx, `SELECT s.code FROM assets a JOIN sites s ON s.id = a.site_id WHERE a.id = ?`, w.AssetID).Scan(&siteCode)
		if err != nil {
			return fmt.Errorf("app: asset %s: %w", w.AssetID, err)
		}
		prefix := fmt.Sprintf("%s-WO-%s-", siteCode, NodeShortCode(a.Author.NodeID))
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM work_orders WHERE number LIKE ? || '%'`, prefix).Scan(&n); err != nil {
			return err
		}
		number = fmt.Sprintf("%s%05d", prefix, n+1)
		return a.emit(ctx, tx, actor, domain.TypeWorkOrderOpened, domain.EntityWorkOrder, id, 0, "", domain.WorkOrderOpened{
			Number: number, Type: w.Type, AssetID: w.AssetID, Priority: w.Priority,
			Title: w.Title, Problem: w.Problem, DueAt: w.DueAt, ScheduleID: w.scheduleID,
		})
	})
	return id, number, err
}

// AssignWorkOrder assigns (or reassigns) a work order, granting the
// assignee a new lease.
func (a *App) AssignWorkOrder(ctx context.Context, actor Actor, woID, assigneeID string) (leaseID string, err error) {
	leaseID = newID()
	return leaseID, a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeWorkOrderAssigned, domain.EntityWorkOrder, woID, 0, "",
			domain.WorkOrderAssigned{AssigneeUserID: assigneeID, LeaseID: leaseID})
	})
}

// ClaimWorkOrder takes unassigned work for the actor.
func (a *App) ClaimWorkOrder(ctx context.Context, actor Actor, woID string) (leaseID string, err error) {
	leaseID = newID()
	return leaseID, a.Store.Update(ctx, func(tx *store.Tx) error {
		return a.emit(ctx, tx, actor, domain.TypeWorkOrderClaimed, domain.EntityWorkOrder, woID, 0, "",
			domain.WorkOrderClaimed{LeaseID: leaseID})
	})
}

// ChangeWorkOrderStatus moves a work order to a new status. Work by the
// assignee is recorded under their current lease.
func (a *App) ChangeWorkOrderStatus(ctx context.Context, actor Actor, woID, to, reason string) error {
	return a.Store.Update(ctx, func(tx *store.Tx) error {
		w, err := domain.GetWorkOrder(ctx, tx, woID)
		if err != nil {
			return err
		}
		lease := ""
		if w.AssignedTo == actor.UserID {
			lease = w.LeaseID
		}
		return a.emit(ctx, tx, actor, domain.TypeWorkOrderStatusChanged, domain.EntityWorkOrder, woID, w.Version, lease,
			domain.WorkOrderStatusChanged{From: w.Status, To: to, Reason: reason})
	})
}

// --- redaction ---

// Redact removes an event's payload, substituting a sanitised
// replacement in projections, and rebuilds them (DESIGN.md §3.6).
func (a *App) Redact(ctx context.Context, actor Actor, eventID, reason string, replacement any) error {
	body, err := json.Marshal(replacement)
	if err != nil {
		return err
	}
	if _, err := a.Store.Redact(ctx, a.Author, actor.UserID, actor.SessionID, eventID, reason, body); err != nil {
		return err
	}
	return a.Store.Rebuild(ctx)
}
