// Package domain defines the equipment-management event types, validates
// each event against current state, and maintains the projections
// derived from them (DESIGN.md §4, §5.3–5.4).
package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// Event types (DESIGN.md §4.4).
const (
	TypeSiteCreated     = "site.created"
	TypeLocationCreated = "location.created"

	TypeUserCreated         = "user.created"
	TypeUserRoleChanged     = "user.role_changed"
	TypeUserDisabled        = "user.disabled"
	TypeUserPasswordChanged = "user.password_changed"
	TypeUserPasswordReset   = "user.password_reset"

	TypeAssetRegistered    = "asset.registered"
	TypeAssetUpdated       = "asset.updated"
	TypeAssetRelocated     = "asset.relocated"
	TypeAssetStatusChanged = "asset.status_changed"

	TypeWorkOrderOpened        = "workorder.opened"
	TypeWorkOrderAssigned      = "workorder.assigned"
	TypeWorkOrderClaimed       = "workorder.claimed"
	TypeWorkOrderStatusChanged = "workorder.status_changed"

	TypeUserLocked   = "user.locked"
	TypeUserUnlocked = "user.unlocked"

	TypeCalibrationRecorded = "calibration.recorded"
	TypeCalibrationVoided   = "calibration.voided"

	TypePMScheduleCreated = "pm_schedule.created"
	TypePMScheduleChanged = "pm_schedule.changed"
	TypePMScheduleEnded   = "pm_schedule.ended"

	TypePartCreated          = "part.created"
	TypeStockLocationCreated = "stock_location.created"
	TypeStockTxnRecorded     = "stock.txn_recorded"
	TypeStockTxnReversed     = "stock.txn_reversed"

	TypeNodeActivated = "node.activated"
	TypeNodeConfirmed = "node.confirmed"
	TypeNodeRejected  = "node.rejected"
	TypeNodeRevoked   = "node.revoked"
)

// Entity types.
const (
	EntitySite      = "site"
	EntityLocation  = "location"
	EntityUser      = "user"
	EntityAsset     = "asset"
	EntityWorkOrder = "work_order"
	EntityCalRecord = "calibration_record"
	EntitySchedule  = "pm_schedule"
	EntityPart      = "part"
	EntityStockLoc  = "stock_location"
	EntityStockTxn  = "stock_txn"
	EntityNode      = "node"
)

// Roles (decision D10). Each includes the permissions of those below it.
const (
	RoleUser      = "user"
	RoleMidTier   = "mid_tier"
	RoleSuperUser = "super_user"
)

var roleRank = map[string]int{RoleUser: 1, RoleMidTier: 2, RoleSuperUser: 3}

// User statuses.
const (
	UserStatusPending  = "pending_activation"
	UserStatusActive   = "active"
	UserStatusDisabled = "disabled"
)

// Asset statuses, most restrictive first (DESIGN.md §5.4 safety rule).
const (
	AssetRetired      = "retired"
	AssetOutOfService = "out_of_service"
	AssetMissing      = "missing"
	AssetInService    = "in_service"
)

var assetRestriction = map[string]int{AssetInService: 0, AssetMissing: 1, AssetOutOfService: 2, AssetRetired: 3}

// Work order statuses (DESIGN.md §4.5).
const (
	WOOpen       = "open"
	WOAssigned   = "assigned"
	WOInProgress = "in_progress"
	WOOnHold     = "on_hold"
	WOCompleted  = "completed"
	WOReviewed   = "reviewed"
	WOClosed     = "closed"
	WOCancelled  = "cancelled"
)

// System actors, and the only event types each may author.
const (
	// SystemConsole bootstraps the first super user at a console.
	SystemConsole = "system:console"
	// SystemScheduler opens work orders from PM schedules (on central).
	SystemScheduler = "system:scheduler"
	// SystemAuth locks accounts after repeated failed logins, on any node.
	SystemAuth = "system:auth"
	// SystemActivation records a Pi's activation request (on central).
	SystemActivation = "system:activation"
)

var systemActorTypes = map[string][]string{
	SystemConsole:    {TypeUserCreated},
	SystemScheduler:  {TypeWorkOrderOpened},
	SystemAuth:       {TypeUserLocked},
	SystemActivation: {TypeNodeActivated},
}

type SiteCreated struct {
	Code     string `json:"code"`
	Name     string `json:"name"`
	Timezone string `json:"timezone"`
}

type LocationCreated struct {
	SiteID   string `json:"site_id"`
	ParentID string `json:"parent_id,omitempty"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

type UserCreated struct {
	Username             string   `json:"username"`
	LegalName            string   `json:"legal_name"`
	Email                string   `json:"email"`
	Role                 string   `json:"role"`
	HomeSites            []string `json:"home_sites"`
	IdentityVerification string   `json:"identity_verification"`
	// Verifier of the initial password. For a one-time activation
	// password, Temporary is true and the account starts pending.
	Verifier  string `json:"verifier"`
	Temporary bool   `json:"temporary"`
}

type UserRoleChanged struct {
	Role string `json:"role"`
}

type UserDisabled struct {
	Reason string `json:"reason"`
}

// UserPasswordChanged is a user replacing their own password. Only the
// verifier is recorded, never the password.
type UserPasswordChanged struct {
	Verifier string `json:"verifier"`
}

// UserPasswordReset is a super user issuing a one-time password.
type UserPasswordReset struct {
	Verifier             string `json:"verifier"`
	IdentityVerification string `json:"identity_verification"`
}

type AssetRegistered struct {
	Tag                 string            `json:"tag"`
	LocationID          string            `json:"location_id"`
	Manufacturer        string            `json:"manufacturer"`
	Model               string            `json:"model"`
	Serial              string            `json:"serial"`
	RiskClass           string            `json:"risk_class"`
	IsReferenceStandard bool              `json:"is_reference_standard"`
	CustomFields        map[string]string `json:"custom_fields"`
}

// AssetUpdated changes only the fields that are present (DESIGN.md §5.4:
// central merges edits that touch different fields).
type AssetUpdated struct {
	Manufacturer        *string            `json:"manufacturer,omitempty"`
	Model               *string            `json:"model,omitempty"`
	Serial              *string            `json:"serial,omitempty"`
	RiskClass           *string            `json:"risk_class,omitempty"`
	IsReferenceStandard *bool              `json:"is_reference_standard,omitempty"`
	CustomFields        *map[string]string `json:"custom_fields,omitempty"`
}

// fields lists the projection fields an update touches.
func (u *AssetUpdated) fields() []string {
	var f []string
	add := func(present bool, name string) {
		if present {
			f = append(f, name)
		}
	}
	add(u.Manufacturer != nil, "manufacturer")
	add(u.Model != nil, "model")
	add(u.Serial != nil, "serial")
	add(u.RiskClass != nil, "risk_class")
	add(u.IsReferenceStandard != nil, "is_reference_standard")
	add(u.CustomFields != nil, "custom_fields")
	return f
}

type AssetRelocated struct {
	LocationID string `json:"location_id"`
}

type AssetStatusChanged struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
}

type WorkOrderOpened struct {
	Number   string `json:"number"`
	Type     string `json:"type"`
	AssetID  string `json:"asset_id"`
	Priority string `json:"priority"`
	Title    string `json:"title"`
	Problem  string `json:"problem"`
	DueAt    string `json:"due_at,omitempty"` // RFC 3339 date or time
	// ScheduleID links a work order generated from a PM schedule.
	ScheduleID string `json:"schedule_id,omitempty"`
	// ProcedureID is the checklist version to follow, if any.
	ProcedureID string `json:"procedure_id,omitempty"`
}

// WorkOrderAssigned grants a new lease to the assignee and ends any
// previous one (DESIGN.md §5.4).
type WorkOrderAssigned struct {
	AssigneeUserID string `json:"assignee_user_id"`
	LeaseID        string `json:"lease_id"`
}

// WorkOrderClaimed is a user taking unassigned work for themselves,
// possibly offline. The first claim central receives gets the lease.
type WorkOrderClaimed struct {
	LeaseID string `json:"lease_id"`
}

type WorkOrderStatusChanged struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason,omitempty"`
}

// UserLocked locks an account after repeated failed logins (DESIGN.md
// §6.5). The lock lasts 15 minutes, or until a super user unlocks it
// after three lockouts in 24 hours.
type UserLocked struct {
	FailedAttempts int    `json:"failed_attempts"`
	Reason         string `json:"reason"`
}

type UserUnlocked struct {
	Reason string `json:"reason"`
}

// CalPoint is one measured calibration point. Readings are decimal
// strings. AsLeft is empty when the instrument was not adjusted. The
// pass flags are the authoring node's computation; central recomputes.
type CalPoint struct {
	Parameter   string    `json:"parameter"`
	Unit        string    `json:"unit"`
	Nominal     string    `json:"nominal"`
	Tolerance   Tolerance `json:"tolerance"`
	AsFound     string    `json:"as_found"`
	AsLeft      string    `json:"as_left,omitempty"`
	AsFoundPass bool      `json:"as_found_pass"`
	AsLeftPass  bool      `json:"as_left_pass"`
}

// CalibrationRecorded is a calibration performed under a calibration
// work order's lease.
type CalibrationRecorded struct {
	WorkOrderID       string     `json:"work_order_id"`
	Procedure         string     `json:"procedure"`
	Temperature       string     `json:"temperature,omitempty"` // °C, decimal
	Humidity          string     `json:"humidity,omitempty"`    // %RH, decimal
	Adjusted          bool       `json:"adjusted"`
	Points            []CalPoint `json:"points"`
	StandardsUsed     []string   `json:"standards_used"` // reference standard asset ids
	AsFoundResult     string     `json:"as_found_result"`
	AsLeftResult      string     `json:"as_left_result"`
	CertificateSHA256 string     `json:"certificate_sha256,omitempty"`
}

type CalibrationVoided struct {
	Reason string `json:"reason"`
}

type PMScheduleCreated struct {
	AssetID      string `json:"asset_id"`
	WOType       string `json:"wo_type"` // pm, calibration or inspection
	Title        string `json:"title"`
	Procedure    string `json:"procedure"`
	IntervalDays int    `json:"interval_days"`
	GraceDays    int    `json:"grace_days"`
	FirstDue     string `json:"first_due"` // YYYY-MM-DD
	// ProcedureID is the checklist generated work orders follow.
	ProcedureID string `json:"procedure_id,omitempty"`
}

type PMScheduleChanged struct {
	Title        *string `json:"title,omitempty"`
	Procedure    *string `json:"procedure,omitempty"`
	IntervalDays *int    `json:"interval_days,omitempty"`
	GraceDays    *int    `json:"grace_days,omitempty"`
	NextDue      *string `json:"next_due,omitempty"`
	ProcedureID  *string `json:"procedure_id,omitempty"`
	Reason       string  `json:"reason"`
}

type PMScheduleEnded struct {
	Reason string `json:"reason"`
}

type PartCreated struct {
	PartNo      string `json:"part_no"`
	Description string `json:"description"`
	Unit        string `json:"unit"`
}

// StockLocationCreated makes a shared stockroom, or a personal kit or van
// when OwnerUserID is set.
type StockLocationCreated struct {
	SiteID      string `json:"site_id"`
	Name        string `json:"name"`
	OwnerUserID string `json:"owner_user_id,omitempty"`
}

// Stock transaction kinds.
const (
	StockReceive  = "receive"
	StockIssue    = "issue"
	StockReturn   = "return"
	StockAdjust   = "adjust"
	StockTransfer = "transfer"
	StockCount    = "count"
)

// StockTxnRecorded is one ledger entry (DESIGN.md §4.1: ledgers, not
// balances). Quantity is a positive amount, except for adjust where it is
// the signed change. A count carries ObservedQty instead, and the delta
// is computed against the ledger when the count is applied.
type StockTxnRecorded struct {
	Kind            string `json:"kind"`
	PartID          string `json:"part_id"`
	StockLocationID string `json:"stock_location_id"`
	ToLocationID    string `json:"to_location_id,omitempty"` // transfer
	Quantity        int64  `json:"quantity,omitempty"`
	ObservedQty     *int64 `json:"observed_qty,omitempty"` // count
	WorkOrderID     string `json:"work_order_id,omitempty"`
	Reason          string `json:"reason,omitempty"`
}

// StockTxnReversed cancels a ledger entry by adding its negation.
type StockTxnReversed struct {
	Reason string `json:"reason"`
}

// NodeActivated records a Pi that proved knowledge of its user's one-time
// password (DESIGN.md §6.3). It stays pending until a super user confirms
// the pairing words. PendingVerifier is the password the employee chose
// during activation; it takes effect on confirmation.
type NodeActivated struct {
	UserID             string `json:"user_id,omitempty"`
	KioskID            string `json:"kiosk_id,omitempty"`
	Mode               string `json:"mode"`
	EventPublicKey     string `json:"event_public_key"`     // hex
	TransportPublicKey string `json:"transport_public_key"` // hex
	PairingWords       string `json:"pairing_words"`
	PendingVerifier    string `json:"pending_verifier,omitempty"`
}

// NodeConfirmed is a super user confirming a pending Pi. PairingWords are
// the words the employee read out, which must match.
type NodeConfirmed struct {
	PairingWords string `json:"pairing_words"`
}

type NodeRejected struct {
	Reason string `json:"reason"`
}

// NodeRevoked stops a Pi syncing. With KeepUnsynced, events it made
// before the revocation are still accepted when it next connects (e.g.
// a lost Pi that turns up); otherwise none are.
type NodeRevoked struct {
	Reason       string `json:"reason"`
	KeepUnsynced bool   `json:"keep_unsynced"`
}

// payloadTypes maps each event type to its entity type and a constructor
// for its schema-version-1 payload.
var payloadTypes = map[string]struct {
	entity string
	new    func() any
}{
	TypeSiteCreated:            {EntitySite, func() any { return &SiteCreated{} }},
	TypeLocationCreated:        {EntityLocation, func() any { return &LocationCreated{} }},
	TypeUserCreated:            {EntityUser, func() any { return &UserCreated{} }},
	TypeUserRoleChanged:        {EntityUser, func() any { return &UserRoleChanged{} }},
	TypeUserDisabled:           {EntityUser, func() any { return &UserDisabled{} }},
	TypeUserPasswordChanged:    {EntityUser, func() any { return &UserPasswordChanged{} }},
	TypeUserPasswordReset:      {EntityUser, func() any { return &UserPasswordReset{} }},
	TypeAssetRegistered:        {EntityAsset, func() any { return &AssetRegistered{} }},
	TypeAssetUpdated:           {EntityAsset, func() any { return &AssetUpdated{} }},
	TypeAssetRelocated:         {EntityAsset, func() any { return &AssetRelocated{} }},
	TypeAssetStatusChanged:     {EntityAsset, func() any { return &AssetStatusChanged{} }},
	TypeWorkOrderOpened:        {EntityWorkOrder, func() any { return &WorkOrderOpened{} }},
	TypeWorkOrderAssigned:      {EntityWorkOrder, func() any { return &WorkOrderAssigned{} }},
	TypeWorkOrderClaimed:       {EntityWorkOrder, func() any { return &WorkOrderClaimed{} }},
	TypeWorkOrderStatusChanged: {EntityWorkOrder, func() any { return &WorkOrderStatusChanged{} }},
	TypeUserLocked:             {EntityUser, func() any { return &UserLocked{} }},
	TypeUserUnlocked:           {EntityUser, func() any { return &UserUnlocked{} }},
	TypeCalibrationRecorded:    {EntityCalRecord, func() any { return &CalibrationRecorded{} }},
	TypeCalibrationVoided:      {EntityCalRecord, func() any { return &CalibrationVoided{} }},
	TypePMScheduleCreated:      {EntitySchedule, func() any { return &PMScheduleCreated{} }},
	TypePMScheduleChanged:      {EntitySchedule, func() any { return &PMScheduleChanged{} }},
	TypePMScheduleEnded:        {EntitySchedule, func() any { return &PMScheduleEnded{} }},
	TypePartCreated:            {EntityPart, func() any { return &PartCreated{} }},
	TypeStockLocationCreated:   {EntityStockLoc, func() any { return &StockLocationCreated{} }},
	TypeStockTxnRecorded:       {EntityStockTxn, func() any { return &StockTxnRecorded{} }},
	TypeStockTxnReversed:       {EntityStockTxn, func() any { return &StockTxnReversed{} }},
	TypeNodeActivated:          {EntityNode, func() any { return &NodeActivated{} }},
	TypeNodeConfirmed:          {EntityNode, func() any { return &NodeConfirmed{} }},
	TypeNodeRejected:           {EntityNode, func() any { return &NodeRejected{} }},
	TypeNodeRevoked:            {EntityNode, func() any { return &NodeRevoked{} }},
}

// decode strictly parses a payload: unknown fields and trailing data are
// errors, so a payload means exactly what its schema says.
func decode(eventType, entityType string, schemaVersion int, payload []byte) (any, error) {
	pt, ok := payloadTypes[eventType]
	if !ok {
		return nil, fmt.Errorf("unknown event type %q", eventType)
	}
	if entityType != pt.entity {
		return nil, fmt.Errorf("%s must target entity type %q, not %q", eventType, pt.entity, entityType)
	}
	if schemaVersion != 1 {
		return nil, fmt.Errorf("%s schema version %d is not supported", eventType, schemaVersion)
	}
	v := pt.new()
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return nil, fmt.Errorf("%s payload: %w", eventType, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("%s payload: trailing data", eventType)
	}
	return v, nil
}
