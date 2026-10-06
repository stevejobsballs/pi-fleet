package domain

// Event types for reviewing flags and clearing quarantine.
const (
	TypeConflictResolved      = "conflict.resolved"
	TypeNodeQuarantineCleared = "node.quarantine_cleared"
	EntityFlaggedEvent        = "flagged_event"
)

// Resolutions of a flagged record (DESIGN.md §5.5). A resolution records a
// person's decision; it does not re-apply or remove the record. Any fix
// is made as an ordinary correction, which has its own audit trail.
const (
	ResolutionAcknowledged = "acknowledged" // a warning was reviewed
	ResolutionAccepted     = "accepted"     // the work is valid; corrected separately if needed
	ResolutionRejected     = "rejected"     // the record stays excluded
)

// ConflictResolved records a mid-tier decision on a flagged event, named
// by the entity id.
type ConflictResolved struct {
	Resolution string `json:"resolution"`
	Note       string `json:"note"`
}

// NodeQuarantineCleared records a super user releasing a quarantined Pi
// after investigating a chain fork.
type NodeQuarantineCleared struct {
	Reason string `json:"reason"`
}

func init() {
	payloadTypes[TypeConflictResolved] = struct {
		entity string
		new    func() any
	}{EntityFlaggedEvent, func() any { return &ConflictResolved{} }}
	payloadTypes[TypeNodeQuarantineCleared] = struct {
		entity string
		new    func() any
	}{EntityNode, func() any { return &NodeQuarantineCleared{} }}
}

func (ap *applier) conflictResolved(p *ConflictResolved) error {
	if err := ap.require(RoleMidTier); err != nil {
		return err
	}
	switch p.Resolution {
	case ResolutionAcknowledged, ResolutionAccepted, ResolutionRejected:
	default:
		return invalid("unknown resolution %q", p.Resolution)
	}
	if blank(p.Note) {
		return invalid("a resolution needs a note explaining the decision")
	}
	if ok, err := ap.exists(`SELECT 1 FROM event_flags WHERE event_id = ?`, ap.e.EntityID); err != nil || !ok {
		return orInvalid(err, "event %s has no flags to resolve", ap.e.EntityID)
	}
	if ok, err := ap.exists(`SELECT 1 FROM flag_resolutions WHERE event_id = ?`, ap.e.EntityID); err != nil || ok {
		return orConflict(err, "event %s was already resolved", ap.e.EntityID)
	}
	return ap.exec(`INSERT INTO flag_resolutions (event_id, resolution, note, resolved_by, resolved_at, last_event_id)
		VALUES (?, ?, ?, ?, ?, ?)`, ap.e.EntityID, p.Resolution, p.Note, ap.actor.id, ap.wall(), ap.e.EventID)
}

func (ap *applier) nodeQuarantineCleared(p *NodeQuarantineCleared) error {
	if err := ap.require(RoleSuperUser); err != nil {
		return err
	}
	if _, err := ap.targetNode(NodeStatusActive, NodeStatusRevoked); err != nil {
		return err
	}
	if blank(p.Reason) {
		return invalid("clearing a quarantine requires a reason")
	}
	return nil // the event is the audit record; sync reads it to lift the quarantine
}
