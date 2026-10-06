package domain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata" // site time zones must resolve on minimal OS images

	"github.com/google/uuid"

	"pi-fleet/internal/event"
	"pi-fleet/internal/password"
	"pi-fleet/internal/store"
)

// Rejection flags (DESIGN.md §5.3).
const (
	FlagInvalid       = "invalid"
	FlagNotAuthorized = "non_authorized"
	FlagStaleBase     = "stale_base"
	FlagDuplicateWork = "duplicate_work"
	FlagConflict      = "conflict"
)

var (
	usernameRE = regexp.MustCompile(`^[a-z][a-z0-9._-]{2,31}$`)
	siteCodeRE = regexp.MustCompile(`^[A-Z0-9]{2,8}$`)
)

// Projector validates events against current state and maintains the
// projections. It implements store.Applier and runs identically on nodes
// and central, so central re-checks everything a node did (DESIGN.md T2).
type Projector struct {
	// LocalNodeID is this installation's node and CentralNodeID the
	// master Pi's. Only their events may use system actors such as
	// SystemConsole: the first super user is bootstrapped on central's
	// console and reaches nodes in their working sets.
	LocalNodeID   string
	CentralNodeID string
	// BlockOverdueStandards rejects calibrations that used a reference
	// standard past its own calibration due date. When false (default)
	// they are accepted and flagged standard_overdue for review.
	BlockOverdueStandards bool
}

// isCentral reports whether e was authored on the master Pi. A
// standalone installation with no central configured is its own central.
func (p *Projector) isCentral(e *event.Event) bool {
	if p.CentralNodeID == "" {
		return e.NodeID == p.LocalNodeID
	}
	return e.NodeID == p.CentralNodeID
}

var _ store.Applier = (*Projector)(nil)

// Reset empties every projection table.
func (p *Projector) Reset(ctx context.Context, tx *sql.Tx) error {
	for _, t := range []string{
		"cal_standards", "cal_points", "calibration_records", "stock_txns", "stock_levels", "stock_locations", "parts",
		"wo_leases", "work_orders", "pm_schedules", "assets", "locations", "sites",
		"user_lockouts", "user_password_history", "users",
	} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+t); err != nil {
			return err
		}
	}
	return nil
}

// Apply validates e against current state and projects it.
func (p *Projector) Apply(ctx context.Context, tx *sql.Tx, e *event.Event) error {
	switch e.Type {
	case event.TypeChainStarted:
		return nil
	case event.TypePayloadRedacted:
		return p.applyRedaction(ctx, tx, e)
	}
	body, err := effectivePayload(ctx, tx, e)
	if err != nil {
		return err
	}
	v, err := decode(e.Type, e.EntityType, e.SchemaVersion, body)
	if err != nil {
		return store.Reject(FlagInvalid, "%v", err)
	}
	a, err := p.actor(ctx, tx, e)
	if err != nil {
		return err
	}
	if a.mustChange && e.Type != TypeUserPasswordChanged {
		return store.Reject(FlagNotAuthorized, "user %s must change their password first", a.id)
	}
	ap := applier{ctx: ctx, tx: tx, e: e, actor: a, p: p}
	switch pl := v.(type) {
	case *SiteCreated:
		return ap.siteCreated(pl)
	case *LocationCreated:
		return ap.locationCreated(pl)
	case *UserCreated:
		return ap.userCreated(pl)
	case *UserRoleChanged:
		return ap.userRoleChanged(pl)
	case *UserDisabled:
		return ap.userDisabled(pl)
	case *UserPasswordChanged:
		return ap.userPasswordChanged(pl)
	case *UserPasswordReset:
		return ap.userPasswordReset(pl)
	case *AssetRegistered:
		return ap.assetRegistered(pl)
	case *AssetUpdated:
		return ap.assetUpdated(pl)
	case *AssetRelocated:
		return ap.assetRelocated(pl)
	case *AssetStatusChanged:
		return ap.assetStatusChanged(pl)
	case *WorkOrderOpened:
		return ap.workOrderOpened(pl)
	case *WorkOrderAssigned:
		return ap.workOrderAssigned(pl)
	case *WorkOrderClaimed:
		return ap.workOrderClaimed(pl)
	case *WorkOrderStatusChanged:
		return ap.workOrderStatusChanged(pl)
	case *UserLocked:
		return ap.userLocked(pl)
	case *UserUnlocked:
		return ap.userUnlocked(pl)
	case *CalibrationRecorded:
		return ap.calibrationRecorded(pl)
	case *CalibrationVoided:
		return ap.calibrationVoided(pl)
	case *PMScheduleCreated:
		return ap.scheduleCreated(pl)
	case *PMScheduleChanged:
		return ap.scheduleChanged(pl)
	case *PMScheduleEnded:
		return ap.scheduleEnded(pl)
	case *PartCreated:
		return ap.partCreated(pl)
	case *StockLocationCreated:
		return ap.stockLocationCreated(pl)
	case *StockTxnRecorded:
		return ap.stockTxnRecorded(pl)
	case *StockTxnReversed:
		return ap.stockTxnReversed(pl)
	}
	return fmt.Errorf("domain: no handler for %T", v)
}

// effectivePayload returns the event's payload, or for a redacted event
// the sanitised replacement its redaction supplied, so projections can be
// rebuilt after a redaction (DESIGN.md §3.6).
func effectivePayload(ctx context.Context, tx *sql.Tx, e *event.Event) ([]byte, error) {
	if !e.Redacted() {
		return e.Payload, nil
	}
	var body []byte
	err := tx.QueryRowContext(ctx, `SELECT payload FROM events
		WHERE type = ? AND entity_type = ? AND entity_id = ? AND payload IS NOT NULL
		ORDER BY local_order LIMIT 1`, event.TypePayloadRedacted, event.EntityEvent, e.EventID).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.Reject(FlagInvalid, "payload redacted without a replacement")
	}
	if err != nil {
		return nil, err
	}
	var r RedactionPayload
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, store.Reject(FlagInvalid, "redaction payload: %v", err)
	}
	return r.Replacement, nil
}

// RedactionPayload is the body of a payload.redacted event.
type RedactionPayload struct {
	Reason            string          `json:"reason"`
	TargetPayloadHash string          `json:"target_payload_hash"`
	Replacement       json.RawMessage `json:"replacement"`
}

func (p *Projector) applyRedaction(ctx context.Context, tx *sql.Tx, e *event.Event) error {
	a, err := p.actor(ctx, tx, e)
	if err != nil {
		return err
	}
	if !a.system && !a.atLeast(RoleMidTier) {
		return store.Reject(FlagNotAuthorized, "redaction requires mid_tier or super_user")
	}
	if e.Redacted() {
		return store.Reject(FlagInvalid, "a redaction's own payload cannot be redacted")
	}
	var r RedactionPayload
	if err := json.Unmarshal(e.Payload, &r); err != nil {
		return store.Reject(FlagInvalid, "redaction payload: %v", err)
	}
	if strings.TrimSpace(r.Reason) == "" {
		return store.Reject(FlagInvalid, "redaction requires a reason")
	}
	var targetType, targetEntity string
	var schema int
	err = tx.QueryRowContext(ctx, `SELECT type, entity_type, schema_version FROM events WHERE event_id = ?`, e.EntityID).Scan(&targetType, &targetEntity, &schema)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Reject(FlagInvalid, "redaction target %s not found", e.EntityID)
	}
	if err != nil {
		return err
	}
	if _, ok := payloadTypes[targetType]; !ok {
		return store.Reject(FlagInvalid, "events of type %s cannot be redacted", targetType)
	}
	if _, err := decode(targetType, targetEntity, schema, r.Replacement); err != nil {
		return store.Reject(FlagInvalid, "replacement: %v", err)
	}
	return nil
}

type actorInfo struct {
	id         string
	role       string
	system     bool
	mustChange bool
}

func (a actorInfo) atLeast(role string) bool { return a.system || roleRank[a.role] >= roleRank[role] }

func (p *Projector) actor(ctx context.Context, tx *sql.Tx, e *event.Event) (actorInfo, error) {
	if strings.HasPrefix(e.ActorUserID, "system:") {
		// Each system actor may author exactly one event type. Console
		// and scheduler events must come from this node or central;
		// lockouts may come from any node, since logins fail offline.
		trusted := e.NodeID == p.LocalNodeID || (p.CentralNodeID != "" && e.NodeID == p.CentralNodeID)
		allowed, known := systemActorTypes[e.ActorUserID]
		if !known || allowed != e.Type || (e.ActorUserID != SystemAuth && !trusted) {
			return actorInfo{}, store.Reject(FlagNotAuthorized, "system actor %s may not author %s from node %s", e.ActorUserID, e.Type, e.NodeID)
		}
		return actorInfo{id: e.ActorUserID, system: true}, nil
	}
	u, err := GetUser(ctx, tx, e.ActorUserID)
	if errors.Is(err, ErrNotFound) {
		return actorInfo{}, store.Reject(FlagNotAuthorized, "unknown user %s", e.ActorUserID)
	}
	if err != nil {
		return actorInfo{}, err
	}
	switch {
	case u.Status == UserStatusDisabled:
		return actorInfo{}, store.Reject(FlagNotAuthorized, "user %s is disabled", u.ID)
	case u.Status == UserStatusPending && e.Type != TypeUserPasswordChanged:
		return actorInfo{}, store.Reject(FlagNotAuthorized, "user %s is not activated", u.ID)
	}
	return actorInfo{id: u.ID, role: u.Role, mustChange: u.MustChangePassword}, nil
}

// applier carries one event through its handler.
type applier struct {
	ctx   context.Context
	tx    *sql.Tx
	e     *event.Event
	actor actorInfo
	p     *Projector
}

func (ap *applier) exec(query string, args ...any) error {
	_, err := ap.tx.ExecContext(ap.ctx, query, args...)
	return err
}

func (ap *applier) require(role string) error {
	if !ap.actor.atLeast(role) {
		return store.Reject(FlagNotAuthorized, "%s requires role %s", ap.e.Type, role)
	}
	return nil
}

// newEntity checks that a creation event names a fresh UUIDv7 id not yet
// used in table.
func (ap *applier) newEntity(table string) error {
	if u, err := uuid.Parse(ap.e.EntityID); err != nil || len(ap.e.EntityID) != 36 || u.Version() != 7 {
		return store.Reject(FlagInvalid, "new entity id %q must be a UUIDv7", ap.e.EntityID)
	}
	ok, err := ap.exists(`SELECT 1 FROM `+table+` WHERE id = ?`, ap.e.EntityID)
	if err != nil || ok {
		return orConflict(err, "%s %s already exists", ap.e.EntityType, ap.e.EntityID)
	}
	return nil
}

func (ap *applier) wall() string { return ap.e.WallTime.UTC().Format(time.RFC3339) }

func invalid(format string, args ...any) error { return store.Reject(FlagInvalid, format, args...) }

func blank(s string) bool { return strings.TrimSpace(s) == "" }

// --- sites and locations ---

func (ap *applier) siteCreated(p *SiteCreated) error {
	if err := ap.require(RoleSuperUser); err != nil {
		return err
	}
	if err := ap.newEntity("sites"); err != nil {
		return err
	}
	if !siteCodeRE.MatchString(p.Code) {
		return invalid("site code %q must be 2-8 capital letters or digits", p.Code)
	}
	if blank(p.Name) {
		return invalid("site name is required")
	}
	if _, err := time.LoadLocation(p.Timezone); err != nil || p.Timezone == "" || p.Timezone == "Local" {
		return invalid("unknown time zone %q", p.Timezone)
	}
	if exists, err := ap.exists(`SELECT 1 FROM sites WHERE code = ?`, p.Code); err != nil || exists {
		return orConflict(err, "site code %s already exists", p.Code)
	}
	return ap.exec(`INSERT INTO sites (id, code, name, timezone, version, last_event_id) VALUES (?, ?, ?, ?, 1, ?)`,
		ap.e.EntityID, p.Code, p.Name, p.Timezone, ap.e.EventID)
}

func (ap *applier) locationCreated(p *LocationCreated) error {
	if err := ap.require(RoleSuperUser); err != nil {
		return err
	}
	if err := ap.newEntity("locations"); err != nil {
		return err
	}
	if blank(p.Name) || blank(p.Kind) {
		return invalid("location name and kind are required")
	}
	if ok, err := ap.exists(`SELECT 1 FROM sites WHERE id = ?`, p.SiteID); err != nil || !ok {
		return orInvalid(err, "site %s not found", p.SiteID)
	}
	var parent any
	if p.ParentID != "" {
		if ok, err := ap.exists(`SELECT 1 FROM locations WHERE id = ? AND site_id = ?`, p.ParentID, p.SiteID); err != nil || !ok {
			return orInvalid(err, "parent location %s not found at site %s", p.ParentID, p.SiteID)
		}
		parent = p.ParentID
	}
	return ap.exec(`INSERT INTO locations (id, site_id, parent_id, name, kind, version, last_event_id) VALUES (?, ?, ?, ?, ?, 1, ?)`,
		ap.e.EntityID, p.SiteID, parent, p.Name, p.Kind, ap.e.EventID)
}

// --- users ---

func (ap *applier) userCreated(p *UserCreated) error {
	if ap.actor.system {
		// Console bootstrap may only create the first super user.
		if n, err := ap.count(`SELECT count(*) FROM users`); err != nil || n > 0 {
			return orNotAuthorized(err, "console bootstrap is only allowed before any user exists")
		}
		if p.Role != RoleSuperUser || p.Temporary {
			return store.Reject(FlagNotAuthorized, "console bootstrap must create a super user with a chosen password")
		}
	} else if err := ap.require(RoleSuperUser); err != nil {
		return err
	}
	if err := ap.newEntity("users"); err != nil {
		return err
	}
	if !usernameRE.MatchString(p.Username) {
		return invalid("username %q must be 3-32 lowercase letters, digits, '.', '_' or '-', starting with a letter", p.Username)
	}
	if blank(p.LegalName) {
		return invalid("legal name is required")
	}
	if addr, err := mail.ParseAddress(p.Email); err != nil || addr.Address != p.Email {
		return invalid("email %q is not a plain address", p.Email)
	}
	if _, ok := roleRank[p.Role]; !ok {
		return invalid("unknown role %q", p.Role)
	}
	if blank(p.IdentityVerification) {
		return invalid("identity verification method is required (Part 11 §11.100(b))")
	}
	if _, err := password.Parse(p.Verifier); err != nil {
		return invalid("verifier: %v", err)
	}
	for _, s := range p.HomeSites {
		if ok, err := ap.exists(`SELECT 1 FROM sites WHERE id = ?`, s); err != nil || !ok {
			return orInvalid(err, "home site %s not found", s)
		}
	}
	if ok, err := ap.exists(`SELECT 1 FROM users WHERE username = ?`, p.Username); err != nil || ok {
		return orConflict(err, "username %s is taken (usernames are never reused)", p.Username)
	}
	status, mustChange, lifetime := UserStatusActive, false, password.Lifetime
	if p.Temporary {
		status, mustChange, lifetime = UserStatusPending, true, password.TemporaryLifetime
	}
	sites, _ := json.Marshal(nonNil(p.HomeSites))
	if err := ap.exec(`INSERT INTO users (id, username, legal_name, email, role, status, home_sites, created_by,
			identity_verified_by, identity_verification, verifier, must_change_password,
			password_changed_at, password_expires_at, version, last_event_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)`,
		ap.e.EntityID, p.Username, p.LegalName, p.Email, p.Role, status, string(sites), ap.actor.id,
		ap.actor.id, p.IdentityVerification, p.Verifier, mustChange,
		ap.wall(), ap.e.WallTime.Add(lifetime).UTC().Format(time.RFC3339), ap.e.EventID); err != nil {
		return err
	}
	if !p.Temporary {
		return ap.pushHistory(p.Verifier)
	}
	return nil
}

func (ap *applier) targetUser() (User, error) {
	u, err := GetUser(ap.ctx, ap.tx, ap.e.EntityID)
	if errors.Is(err, ErrNotFound) {
		return User{}, invalid("user %s not found", ap.e.EntityID)
	}
	return u, err
}

func (ap *applier) bumpUser(set string, args ...any) error {
	args = append(args, ap.e.EventID, ap.e.EntityID)
	return ap.exec(`UPDATE users SET `+set+`, version = version + 1, last_event_id = ? WHERE id = ?`, args...)
}

// lastSuperUser reports whether u is the only active super user.
func (ap *applier) lastSuperUser(u User) (bool, error) {
	if u.Role != RoleSuperUser || u.Status != UserStatusActive {
		return false, nil
	}
	n, err := ap.count(`SELECT count(*) FROM users WHERE role = ? AND status = ?`, RoleSuperUser, UserStatusActive)
	return n == 1, err
}

func (ap *applier) userRoleChanged(p *UserRoleChanged) error {
	if err := ap.require(RoleSuperUser); err != nil {
		return err
	}
	u, err := ap.targetUser()
	if err != nil {
		return err
	}
	if _, ok := roleRank[p.Role]; !ok {
		return invalid("unknown role %q", p.Role)
	}
	if p.Role != RoleSuperUser {
		if last, err := ap.lastSuperUser(u); err != nil || last {
			return orConflict(err, "cannot demote the last active super user")
		}
	}
	return ap.bumpUser(`role = ?`, p.Role)
}

func (ap *applier) userDisabled(p *UserDisabled) error {
	if err := ap.require(RoleSuperUser); err != nil {
		return err
	}
	u, err := ap.targetUser()
	if err != nil {
		return err
	}
	if blank(p.Reason) {
		return invalid("disabling a user requires a reason")
	}
	if last, err := ap.lastSuperUser(u); err != nil || last {
		return orConflict(err, "cannot disable the last active super user")
	}
	return ap.bumpUser(`status = ?`, UserStatusDisabled)
}

func (ap *applier) userPasswordChanged(p *UserPasswordChanged) error {
	if ap.actor.id != ap.e.EntityID {
		return store.Reject(FlagNotAuthorized, "users may only change their own password")
	}
	if _, err := password.Parse(p.Verifier); err != nil {
		return invalid("verifier: %v", err)
	}
	if err := ap.bumpUser(`verifier = ?, must_change_password = 0, status = ?, password_changed_at = ?, password_expires_at = ?`,
		p.Verifier, UserStatusActive, ap.wall(), ap.e.WallTime.Add(password.Lifetime).UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	return ap.pushHistory(p.Verifier)
}

func (ap *applier) userPasswordReset(p *UserPasswordReset) error {
	if err := ap.require(RoleSuperUser); err != nil {
		return err
	}
	u, err := ap.targetUser()
	if err != nil {
		return err
	}
	if u.Status == UserStatusDisabled {
		return invalid("user %s is disabled", u.ID)
	}
	if blank(p.IdentityVerification) {
		return invalid("identity verification method is required for a reset")
	}
	if _, err := password.Parse(p.Verifier); err != nil {
		return invalid("verifier: %v", err)
	}
	return ap.bumpUser(`verifier = ?, must_change_password = 1, password_changed_at = ?, password_expires_at = ?`,
		p.Verifier, ap.wall(), ap.e.WallTime.Add(password.TemporaryLifetime).UTC().Format(time.RFC3339))
}

// pushHistory records a chosen (not temporary) verifier, keeping the
// newest password.HistoryDepth.
func (ap *applier) pushHistory(verifier string) error {
	if err := ap.exec(`INSERT INTO user_password_history (user_id, event_id, verifier, changed_at) VALUES (?, ?, ?, ?)`,
		ap.e.EntityID, ap.e.EventID, verifier, ap.wall()); err != nil {
		return err
	}
	return ap.exec(`DELETE FROM user_password_history WHERE user_id = ? AND event_id NOT IN (
		SELECT event_id FROM user_password_history h JOIN events e USING (event_id)
		WHERE h.user_id = ? ORDER BY e.local_order DESC LIMIT ?)`, ap.e.EntityID, ap.e.EntityID, password.HistoryDepth)
}

// --- assets ---

func (ap *applier) assetRegistered(p *AssetRegistered) error {
	if err := ap.require(RoleUser); err != nil {
		return err
	}
	if err := ap.newEntity("assets"); err != nil {
		return err
	}
	if blank(p.Tag) || blank(p.Manufacturer) || blank(p.Model) {
		return invalid("asset tag, manufacturer and model are required")
	}
	var siteID string
	err := ap.tx.QueryRowContext(ap.ctx, `SELECT site_id FROM locations WHERE id = ?`, p.LocationID).Scan(&siteID)
	if errors.Is(err, sql.ErrNoRows) {
		return invalid("location %s not found", p.LocationID)
	}
	if err != nil {
		return err
	}
	if ok, err := ap.exists(`SELECT 1 FROM assets WHERE tag = ?`, p.Tag); err != nil || ok {
		return orConflict(err, "asset tag %s already exists", p.Tag)
	}
	custom, _ := json.Marshal(nonNilMap(p.CustomFields))
	fv, _ := json.Marshal(map[string]int64{
		"manufacturer": 1, "model": 1, "serial": 1, "risk_class": 1,
		"is_reference_standard": 1, "custom_fields": 1, "location": 1, "status": 1,
	})
	return ap.exec(`INSERT INTO assets (id, tag, site_id, location_id, manufacturer, model, serial, status,
			risk_class, is_reference_standard, custom_fields, field_versions, version, last_event_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)`,
		ap.e.EntityID, p.Tag, siteID, p.LocationID, p.Manufacturer, p.Model, p.Serial, AssetInService,
		p.RiskClass, p.IsReferenceStandard, string(custom), string(fv), ap.e.EventID)
}

func (ap *applier) targetAsset() (Asset, error) {
	a, err := GetAsset(ap.ctx, ap.tx, ap.e.EntityID)
	if errors.Is(err, ErrNotFound) {
		return Asset{}, invalid("asset %s not found", ap.e.EntityID)
	}
	return a, err
}

// staleFields returns the fields changed since the author's base version.
func (ap *applier) staleFields(a Asset, fields []string) []string {
	var stale []string
	for _, f := range fields {
		if a.FieldVersions[f] > ap.e.BaseVersion {
			stale = append(stale, f)
		}
	}
	return stale
}

// bumpAsset applies set to the asset and records which fields changed.
func (ap *applier) bumpAsset(a Asset, fields []string, set string, args ...any) error {
	next := a.Version + 1
	for _, f := range fields {
		a.FieldVersions[f] = next
	}
	fv, _ := json.Marshal(a.FieldVersions)
	if set != "" {
		set += ", "
	}
	args = append(args, string(fv), ap.e.EventID, a.ID)
	return ap.exec(`UPDATE assets SET `+set+`field_versions = ?, version = version + 1, last_event_id = ? WHERE id = ?`, args...)
}

func (ap *applier) assetUpdated(p *AssetUpdated) error {
	if err := ap.require(RoleUser); err != nil {
		return err
	}
	a, err := ap.targetAsset()
	if err != nil {
		return err
	}
	fields := p.fields()
	if len(fields) == 0 {
		return invalid("asset update changes nothing")
	}
	if a.Status == AssetRetired {
		return invalid("asset %s is retired", a.Tag)
	}
	if stale := ap.staleFields(a, fields); len(stale) > 0 {
		return store.Reject(FlagStaleBase, "fields %v changed since version %d", stale, ap.e.BaseVersion)
	}
	var sets []string
	var args []any
	set := func(col string, v any) { sets = append(sets, col+" = ?"); args = append(args, v) }
	if p.Manufacturer != nil {
		if blank(*p.Manufacturer) {
			return invalid("manufacturer cannot be blank")
		}
		set("manufacturer", *p.Manufacturer)
	}
	if p.Model != nil {
		if blank(*p.Model) {
			return invalid("model cannot be blank")
		}
		set("model", *p.Model)
	}
	if p.Serial != nil {
		set("serial", *p.Serial)
	}
	if p.RiskClass != nil {
		set("risk_class", *p.RiskClass)
	}
	if p.IsReferenceStandard != nil {
		set("is_reference_standard", *p.IsReferenceStandard)
	}
	if p.CustomFields != nil {
		b, _ := json.Marshal(nonNilMap(*p.CustomFields))
		set("custom_fields", string(b))
	}
	return ap.bumpAsset(a, fields, strings.Join(sets, ", "), args...)
}

func (ap *applier) assetRelocated(p *AssetRelocated) error {
	if err := ap.require(RoleUser); err != nil {
		return err
	}
	a, err := ap.targetAsset()
	if err != nil {
		return err
	}
	if a.Status == AssetRetired {
		return invalid("asset %s is retired", a.Tag)
	}
	if stale := ap.staleFields(a, []string{"location"}); len(stale) > 0 {
		return store.Reject(FlagStaleBase, "location changed since version %d", ap.e.BaseVersion)
	}
	var siteID string
	err = ap.tx.QueryRowContext(ap.ctx, `SELECT site_id FROM locations WHERE id = ?`, p.LocationID).Scan(&siteID)
	if errors.Is(err, sql.ErrNoRows) {
		return invalid("location %s not found", p.LocationID)
	}
	if err != nil {
		return err
	}
	return ap.bumpAsset(a, []string{"location"}, `location_id = ?, site_id = ?`, p.LocationID, siteID)
}

func (ap *applier) assetStatusChanged(p *AssetStatusChanged) error {
	if err := ap.require(RoleUser); err != nil {
		return err
	}
	a, err := ap.targetAsset()
	if err != nil {
		return err
	}
	if _, ok := assetRestriction[p.Status]; !ok {
		return invalid("unknown asset status %q", p.Status)
	}
	if p.Status != AssetInService && blank(p.Reason) {
		return invalid("taking an asset out of service requires a reason")
	}
	if (p.Status == AssetRetired || a.Status == AssetRetired) && !ap.actor.atLeast(RoleMidTier) {
		return store.Reject(FlagNotAuthorized, "retiring or un-retiring an asset requires mid_tier")
	}
	if p.Status == a.Status {
		return invalid("asset %s is already %s", a.Tag, p.Status)
	}
	if len(ap.staleFields(a, []string{"status"})) > 0 {
		// Safety rule: a more restrictive status always wins, but the
		// conflict is still flagged for review (DESIGN.md §5.4).
		if assetRestriction[p.Status] <= assetRestriction[a.Status] {
			return store.Reject(FlagStaleBase, "status changed to %s since version %d", a.Status, ap.e.BaseVersion)
		}
		if err := store.Flag(ap.ctx, ap.tx, ap.e.EventID, FlagStaleBase,
			fmt.Sprintf("applied more restrictive status %s over concurrent %s", p.Status, a.Status), true); err != nil {
			return err
		}
	}
	return ap.bumpAsset(a, []string{"status"}, `status = ?`, p.Status)
}

// --- work orders ---

var woTypes = map[string]string{ // type -> minimum role to open
	"corrective": RoleUser, "inspection": RoleUser,
	"pm": RoleMidTier, "calibration": RoleMidTier, "install": RoleMidTier, "retire": RoleMidTier,
}

var priorities = map[string]bool{"low": true, "normal": true, "high": true, "urgent": true}

func (ap *applier) workOrderOpened(p *WorkOrderOpened) error {
	minRole, ok := woTypes[p.Type]
	if !ok {
		return invalid("unknown work order type %q", p.Type)
	}
	if err := ap.require(minRole); err != nil {
		return err
	}
	if err := ap.newEntity("work_orders"); err != nil {
		return err
	}
	if !priorities[p.Priority] {
		return invalid("unknown priority %q", p.Priority)
	}
	if blank(p.Number) || blank(p.Title) {
		return invalid("work order number and title are required")
	}
	if p.DueAt != "" {
		if _, err := parseDue(p.DueAt); err != nil {
			return invalid("due_at %q must be YYYY-MM-DD or RFC 3339", p.DueAt)
		}
	}
	a, err := GetAsset(ap.ctx, ap.tx, p.AssetID)
	if errors.Is(err, ErrNotFound) {
		return invalid("asset %s not found", p.AssetID)
	}
	if err != nil {
		return err
	}
	if a.Status == AssetRetired {
		return invalid("asset %s is retired", a.Tag)
	}
	if ok, err := ap.exists(`SELECT 1 FROM work_orders WHERE number = ?`, p.Number); err != nil || ok {
		return orConflict(err, "work order number %s already exists", p.Number)
	}
	if ap.actor.id == SystemScheduler && p.ScheduleID == "" {
		return store.Reject(FlagNotAuthorized, "the scheduler may only open scheduled work orders")
	}
	if p.ScheduleID != "" {
		if err := ap.linkSchedule(p); err != nil {
			return err
		}
	}
	return ap.exec(`INSERT INTO work_orders (id, number, type, asset_id, priority, status, title, problem, due_at,
			opened_by, assigned_to, lease_id, schedule_id, version, last_event_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', '', ?, 1, ?)`,
		ap.e.EntityID, p.Number, p.Type, p.AssetID, p.Priority, WOOpen, p.Title, p.Problem, p.DueAt,
		ap.actor.id, p.ScheduleID, ap.e.EventID)
}

func parseDue(s string) (time.Time, error) {
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}

func (ap *applier) targetWorkOrder() (WorkOrder, error) {
	w, err := GetWorkOrder(ap.ctx, ap.tx, ap.e.EntityID)
	if errors.Is(err, ErrNotFound) {
		return WorkOrder{}, invalid("work order %s not found", ap.e.EntityID)
	}
	return w, err
}

func (ap *applier) bumpWorkOrder(set string, args ...any) error {
	args = append(args, ap.e.EventID, ap.e.EntityID)
	return ap.exec(`UPDATE work_orders SET `+set+`, version = version + 1, last_event_id = ? WHERE id = ?`, args...)
}

func (ap *applier) grantLease(w WorkOrder, leaseID, userID string) error {
	if u, err := uuid.Parse(leaseID); err != nil || len(leaseID) != 36 || u.Version() != 7 {
		return invalid("lease id %q must be a UUIDv7", leaseID)
	}
	if ok, err := ap.exists(`SELECT 1 FROM wo_leases WHERE lease_id = ?`, leaseID); err != nil || ok {
		return orInvalid(err, "lease id %s already used", leaseID)
	}
	if err := ap.endLease(w); err != nil {
		return err
	}
	return ap.exec(`INSERT INTO wo_leases (lease_id, wo_id, user_id, granted_event) VALUES (?, ?, ?, ?)`,
		leaseID, w.ID, userID, ap.e.EventID)
}

func (ap *applier) endLease(w WorkOrder) error {
	if w.LeaseID == "" {
		return nil
	}
	return ap.exec(`UPDATE wo_leases SET ended_hlc = ? WHERE lease_id = ? AND ended_hlc IS NULL`, ap.e.HLC, w.LeaseID)
}

func (ap *applier) workOrderAssigned(p *WorkOrderAssigned) error {
	if err := ap.require(RoleMidTier); err != nil {
		return err
	}
	w, err := ap.targetWorkOrder()
	if err != nil {
		return err
	}
	switch w.Status {
	case WOOpen, WOAssigned, WOInProgress, WOOnHold:
	default:
		return invalid("work order %s is %s and cannot be assigned", w.Number, w.Status)
	}
	u, err := GetUser(ap.ctx, ap.tx, p.AssigneeUserID)
	if errors.Is(err, ErrNotFound) || (err == nil && u.Status != UserStatusActive) {
		return invalid("assignee %s is not an active user", p.AssigneeUserID)
	}
	if err != nil {
		return err
	}
	if err := ap.grantLease(w, p.LeaseID, u.ID); err != nil {
		return err
	}
	status := w.Status
	if status == WOOpen {
		status = WOAssigned
	}
	return ap.bumpWorkOrder(`assigned_to = ?, lease_id = ?, status = ?`, u.ID, p.LeaseID, status)
}

func (ap *applier) workOrderClaimed(p *WorkOrderClaimed) error {
	if err := ap.require(RoleUser); err != nil {
		return err
	}
	w, err := ap.targetWorkOrder()
	if err != nil {
		return err
	}
	if w.LeaseID != "" {
		// Someone else's claim or assignment reached central first. The
		// later claimant's work is kept but flagged (DESIGN.md §5.4).
		return store.Reject(FlagDuplicateWork, "work order %s is already held by %s", w.Number, w.AssignedTo)
	}
	if w.Status != WOOpen {
		return invalid("work order %s is %s and cannot be claimed", w.Number, w.Status)
	}
	if err := ap.grantLease(w, p.LeaseID, ap.actor.id); err != nil {
		return err
	}
	return ap.bumpWorkOrder(`assigned_to = ?, lease_id = ?, status = ?`, ap.actor.id, p.LeaseID, WOAssigned)
}

// woTransition describes who may move a work order between two states.
type woTransition struct {
	holder       bool // performed by the lease holder under their lease
	minRole      string
	needsReason  bool
	endsLease    bool
	twoPersonRev bool // reviewer must differ from the performer
}

var woTransitions = map[[2]string]woTransition{
	{WOAssigned, WOInProgress}:  {holder: true},
	{WOInProgress, WOOnHold}:    {holder: true},
	{WOOnHold, WOInProgress}:    {holder: true},
	{WOInProgress, WOCompleted}: {holder: true},
	{WOCompleted, WOReviewed}:   {minRole: RoleMidTier, twoPersonRev: true},
	{WOReviewed, WOClosed}:      {minRole: RoleMidTier, endsLease: true},
	{WOCompleted, WOInProgress}: {minRole: RoleMidTier, needsReason: true}, // reopen
	{WOReviewed, WOInProgress}:  {minRole: RoleMidTier, needsReason: true}, // reopen
}

func (ap *applier) workOrderStatusChanged(p *WorkOrderStatusChanged) error {
	w, err := ap.targetWorkOrder()
	if err != nil {
		return err
	}
	if p.From != w.Status {
		return store.Reject(FlagStaleBase, "work order %s is %s, not %s", w.Number, w.Status, p.From)
	}
	t, ok := woTransitions[[2]string{p.From, p.To}]
	if !ok && p.To == WOCancelled && p.From != WOClosed && p.From != WOCancelled {
		t, ok = woTransition{minRole: RoleMidTier, needsReason: true, endsLease: true}, true
	}
	if !ok {
		return invalid("work order cannot go from %s to %s", p.From, p.To)
	}
	if t.needsReason && blank(p.Reason) {
		return invalid("%s → %s requires a reason", p.From, p.To)
	}
	if t.holder {
		if err := ap.checkLease(w); err != nil {
			return err
		}
	} else if err := ap.require(t.minRole); err != nil {
		return err
	}
	if t.twoPersonRev && ap.actor.id == w.AssignedTo {
		return store.Reject(FlagNotAuthorized, "the reviewer must be a different user from the performer")
	}
	if p.To == WOCompleted {
		if err := ap.checkCompletion(w); err != nil {
			return err
		}
	}
	if err := ap.scheduleFollowsWorkOrder(w, p.To); err != nil {
		return err
	}
	if t.endsLease {
		if err := ap.endLease(w); err != nil {
			return err
		}
		return ap.bumpWorkOrder(`status = ?, lease_id = ''`, p.To)
	}
	return ap.bumpWorkOrder(`status = ?`, p.To)
}

// checkLease accepts an event made under the actor's lease on w, either
// the active lease or an ended one if the event predates its end
// (offline grace, DESIGN.md §5.4).
func (ap *applier) checkLease(w WorkOrder) error {
	if ap.e.LeaseID == "" {
		return store.Reject(FlagNotAuthorized, "work on %s must be made under a lease", w.Number)
	}
	var userID string
	var ended sql.NullInt64
	err := ap.tx.QueryRowContext(ap.ctx, `SELECT user_id, ended_hlc FROM wo_leases WHERE lease_id = ? AND wo_id = ?`,
		ap.e.LeaseID, w.ID).Scan(&userID, &ended)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Reject(FlagNotAuthorized, "lease %s is not a lease on %s", ap.e.LeaseID, w.Number)
	}
	if err != nil {
		return err
	}
	if userID != ap.actor.id {
		return store.Reject(FlagNotAuthorized, "lease %s belongs to another user", ap.e.LeaseID)
	}
	if ended.Valid && ap.e.HLC >= ended.Int64 {
		return store.Reject(FlagNotAuthorized, "lease %s ended before this event", ap.e.LeaseID)
	}
	return nil
}

// --- helpers ---

func (ap *applier) exists(query string, args ...any) (bool, error) {
	var x int
	err := ap.tx.QueryRowContext(ap.ctx, query, args...).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (ap *applier) count(query string, args ...any) (int, error) {
	var n int
	err := ap.tx.QueryRowContext(ap.ctx, query, args...).Scan(&n)
	return n, err
}

func orConflict(err error, format string, args ...any) error {
	if err != nil {
		return err
	}
	return store.Reject(FlagConflict, format, args...)
}

func orInvalid(err error, format string, args ...any) error {
	if err != nil {
		return err
	}
	return invalid(format, args...)
}

func orNotAuthorized(err error, format string, args ...any) error {
	if err != nil {
		return err
	}
	return store.Reject(FlagNotAuthorized, format, args...)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
