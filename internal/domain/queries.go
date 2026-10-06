package domain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"pi-fleet/internal/password"
)

// ErrNotFound is returned when a projection row does not exist.
var ErrNotFound = errors.New("domain: not found")

// Querier is satisfied by *sql.DB and *sql.Tx.
type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// User is the projected state of an account.
type User struct {
	ID                 string
	Username           string
	LegalName          string
	Email              string
	Role               string
	Status             string
	HomeSites          []string
	Verifier           string
	MustChangePassword bool
	PasswordChangedAt  time.Time
	PasswordExpiresAt  time.Time
	Version            int64
}

// PasswordExpired reports whether the password has expired at now.
func (u User) PasswordExpired(now time.Time) bool { return !now.Before(u.PasswordExpiresAt) }

const userColumns = `id, username, legal_name, email, role, status, home_sites, verifier,
	must_change_password, password_changed_at, password_expires_at, version`

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	var sites, changed, expires string
	err := row.Scan(&u.ID, &u.Username, &u.LegalName, &u.Email, &u.Role, &u.Status, &sites, &u.Verifier,
		&u.MustChangePassword, &changed, &expires, &u.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	if err := json.Unmarshal([]byte(sites), &u.HomeSites); err != nil {
		return User{}, err
	}
	if u.PasswordChangedAt, err = time.Parse(time.RFC3339, changed); err != nil {
		return User{}, err
	}
	if u.PasswordExpiresAt, err = time.Parse(time.RFC3339, expires); err != nil {
		return User{}, err
	}
	return u, nil
}

// GetUser returns a user by id.
func GetUser(ctx context.Context, q Querier, id string) (User, error) {
	return scanUser(q.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id))
}

// GetUserByUsername returns a user by username.
func GetUserByUsername(ctx context.Context, q Querier, username string) (User, error) {
	return scanUser(q.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE username = ?`, username))
}

// CountUsers returns how many accounts exist.
func CountUsers(ctx context.Context, q Querier) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

// PasswordHistory returns a user's recent chosen-password verifiers.
func PasswordHistory(ctx context.Context, q Querier, userID string) ([]password.Verifier, error) {
	rows, err := q.QueryContext(ctx, `SELECT verifier FROM user_password_history WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []password.Verifier
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		v, err := password.Parse(s)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Site is a projected site.
type Site struct {
	ID, Code, Name, Timezone string
}

// GetSiteByCode returns a site by its code.
func GetSiteByCode(ctx context.Context, q Querier, code string) (Site, error) {
	var s Site
	err := q.QueryRowContext(ctx, `SELECT id, code, name, timezone FROM sites WHERE code = ?`, code).Scan(&s.ID, &s.Code, &s.Name, &s.Timezone)
	if errors.Is(err, sql.ErrNoRows) {
		return Site{}, ErrNotFound
	}
	return s, err
}

// Asset is the projected state of a piece of equipment.
type Asset struct {
	ID                  string
	Tag                 string
	SiteID              string
	LocationID          string
	Manufacturer        string
	Model               string
	Serial              string
	Status              string
	RiskClass           string
	IsReferenceStandard bool
	CustomFields        map[string]string
	FieldVersions       map[string]int64
	Version             int64
}

// GetAsset returns an asset by id.
func GetAsset(ctx context.Context, q Querier, id string) (Asset, error) {
	var a Asset
	var custom, fv string
	err := q.QueryRowContext(ctx, `SELECT id, tag, site_id, location_id, manufacturer, model, serial, status,
		risk_class, is_reference_standard, custom_fields, field_versions, version FROM assets WHERE id = ?`, id).
		Scan(&a.ID, &a.Tag, &a.SiteID, &a.LocationID, &a.Manufacturer, &a.Model, &a.Serial, &a.Status,
			&a.RiskClass, &a.IsReferenceStandard, &custom, &fv, &a.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return Asset{}, ErrNotFound
	}
	if err != nil {
		return Asset{}, err
	}
	if err := json.Unmarshal([]byte(custom), &a.CustomFields); err != nil {
		return Asset{}, err
	}
	if err := json.Unmarshal([]byte(fv), &a.FieldVersions); err != nil {
		return Asset{}, err
	}
	return a, nil
}

// WorkOrder is the projected state of a work order.
type WorkOrder struct {
	ID         string
	Number     string
	Type       string
	AssetID    string
	Priority   string
	Status     string
	Title      string
	Problem    string
	DueAt      string
	OpenedBy   string
	AssignedTo string
	LeaseID    string
	Version    int64
}

// GetWorkOrder returns a work order by id.
func GetWorkOrder(ctx context.Context, q Querier, id string) (WorkOrder, error) {
	var w WorkOrder
	err := q.QueryRowContext(ctx, `SELECT id, number, type, asset_id, priority, status, title, problem, due_at,
		opened_by, assigned_to, lease_id, version FROM work_orders WHERE id = ?`, id).
		Scan(&w.ID, &w.Number, &w.Type, &w.AssetID, &w.Priority, &w.Status, &w.Title, &w.Problem, &w.DueAt,
			&w.OpenedBy, &w.AssignedTo, &w.LeaseID, &w.Version)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkOrder{}, ErrNotFound
	}
	return w, err
}
