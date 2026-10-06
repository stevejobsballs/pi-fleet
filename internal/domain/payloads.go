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
)

// Entity types.
const (
	EntitySite      = "site"
	EntityLocation  = "location"
	EntityUser      = "user"
	EntityAsset     = "asset"
	EntityWorkOrder = "work_order"
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

// SystemConsole is the actor for bootstrap commands run on the device's
// own console (DESIGN.md §6.5: the first super user).
const SystemConsole = "system:console"

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
