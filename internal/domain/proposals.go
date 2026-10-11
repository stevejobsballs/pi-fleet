package domain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Checklist proposals: anyone can build a checklist, but only mid-tier and
// super users publish one. Others' checklists wait, as proposals, for one
// of them to approve (which publishes the next version) or reject them.

const (
	TypeProcedureProposed         = "procedure.proposed"
	TypeProcedureProposalReviewed = "procedure.proposal_reviewed"
	EntityProposal                = "procedure_proposal"

	ProposalPending  = "pending"
	ProposalApproved = "approved"
	ProposalRejected = "rejected"
)

// ProcedureProposed proposes a checklist (or its next version).
type ProcedureProposed struct {
	ProcedurePublished
	BasedOn string `json:"based_on,omitempty"` // the version it was started from
}

// ProcedureProposalReviewed approves (publishing it as PublishedID) or
// rejects a proposal; a rejection needs a reason.
type ProcedureProposalReviewed struct {
	Approved    bool   `json:"approved"`
	Note        string `json:"note,omitempty"`
	PublishedID string `json:"published_id,omitempty"`
}

func init() {
	payloadTypes[TypeProcedureProposed] = struct {
		entity string
		new    func() any
	}{EntityProposal, func() any { return &ProcedureProposed{} }}
	payloadTypes[TypeProcedureProposalReviewed] = struct {
		entity string
		new    func() any
	}{EntityProposal, func() any { return &ProcedureProposalReviewed{} }}
}

// validateChecklist checks what publishing and proposing have in common.
func validateChecklist(p ProcedurePublished) error {
	if blank(p.Name) || len(p.Name) > 120 {
		return invalid("a checklist needs a name of at most 120 characters")
	}
	if len(p.Description) > 4000 || len(p.Reference) > 300 || len(p.Category) > 80 {
		return invalid("the description, reference or category is too long")
	}
	if len(p.AppliesTo) > 100 {
		return invalid("a checklist can name at most 100 manufacturers and models")
	}
	for _, m := range p.AppliesTo {
		if blank(m.Manufacturer) || len(m.Manufacturer) > 120 || len(m.Model) > 120 {
			return invalid("each piece of equipment it is for needs a manufacturer (and, if only one model, the model)")
		}
	}
	return validateSteps(p.Steps)
}

func (ap *applier) procedureProposed(p *ProcedureProposed) error {
	if err := ap.require(RoleUser); err != nil {
		return err
	}
	if err := ap.newEntity("procedure_proposals"); err != nil {
		return err
	}
	if err := validateChecklist(p.ProcedurePublished); err != nil {
		return err
	}
	if err := ap.checkKind(p.ProcedurePublished); err != nil {
		return err
	}
	steps, _ := json.Marshal(p.Steps)
	applies, _ := json.Marshal(nonNilMatches(p.AppliesTo))
	return ap.exec(`INSERT INTO procedure_proposals (id, name, description, reference, category, applies_to, steps, based_on,
			proposed_by, proposed_at, status, last_event_id, custom, template_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ap.e.EntityID, p.Name, p.Description, p.Reference, p.Category, string(applies), string(steps), p.BasedOn,
		ap.actor.id, ap.wall(), ProposalPending, ap.e.EventID, p.Custom, p.Template)
}

func (ap *applier) procedureProposalReviewed(p *ProcedureProposalReviewed) error {
	if err := ap.require(RoleMidTier); err != nil {
		return err
	}
	pr, err := GetProposal(ap.ctx, ap.tx, ap.e.EntityID)
	if errors.Is(err, ErrNotFound) {
		return invalid("checklist proposal %s not found", ap.e.EntityID)
	}
	if err != nil {
		return err
	}
	if pr.Status != ProposalPending {
		return orConflict(nil, "this checklist was already %s", pr.Status)
	}
	if !p.Approved {
		if blank(p.Note) {
			return invalid("rejecting a checklist needs a reason, which the person who made it sees")
		}
		return ap.exec(`UPDATE procedure_proposals SET status = ?, reviewed_by = ?, review_note = ?, last_event_id = ? WHERE id = ?`,
			ProposalRejected, ap.actor.id, p.Note, ap.e.EventID, pr.ID)
	}
	// Publish it, as the next version of its name.
	if ok, err := ap.exists(`SELECT 1 FROM procedures WHERE id = ?`, p.PublishedID); err != nil || ok || len(p.PublishedID) != 36 {
		return orInvalid(err, "approving needs a new id for the published version")
	}
	var latest int
	if err := ap.tx.QueryRowContext(ap.ctx, `SELECT coalesce(max(version), 0) FROM procedures WHERE name = ?`, pr.Name).Scan(&latest); err != nil {
		return err
	}
	if err := ap.checkKind(pr.ProcedurePublished); err != nil {
		return err
	}
	steps, _ := json.Marshal(pr.Steps)
	applies, _ := json.Marshal(nonNilMatches(pr.AppliesTo))
	if err := ap.exec(`INSERT INTO procedures (id, name, version, steps, status, published_by, published_at, last_event_id, description, reference, category, applies_to,
			custom, template_id)
		VALUES (?, ?, ?, ?, 'active', ?, ?, ?, ?, ?, ?, ?, ?, ?)`, p.PublishedID, pr.Name, latest+1, string(steps), ap.actor.id, ap.wall(), ap.e.EventID,
		pr.Description, pr.Reference, pr.Category, string(applies), pr.Custom, pr.Template); err != nil {
		return err
	}
	return ap.exec(`UPDATE procedure_proposals SET status = ?, reviewed_by = ?, review_note = ?, published_id = ?, last_event_id = ? WHERE id = ?`,
		ProposalApproved, ap.actor.id, p.Note, p.PublishedID, ap.e.EventID, pr.ID)
}

// Proposal is a checklist waiting for approval (or approved or rejected).
type Proposal struct {
	ProcedurePublished
	ID, BasedOn, Status, ReviewNote, PublishedID string
	ProposedByID, ProposedBy, ReviewedBy         string // names
	ProposedAt                                   time.Time
}

const proposalColumns = `p.id, p.name, p.description, p.reference, p.category, p.custom, p.template_id, p.applies_to, p.steps, p.based_on, p.status, p.review_note,
	p.published_id, p.proposed_by, coalesce((SELECT legal_name FROM users WHERE id = p.proposed_by), p.proposed_by),
	coalesce((SELECT legal_name FROM users WHERE id = p.reviewed_by), p.reviewed_by), p.proposed_at FROM procedure_proposals p`

func scanProposal(r interface{ Scan(...any) error }) (Proposal, error) {
	var p Proposal
	var applies, steps, at string
	err := r.Scan(&p.ID, &p.Name, &p.Description, &p.Reference, &p.Category, &p.Custom, &p.Template, &applies, &steps, &p.BasedOn, &p.Status, &p.ReviewNote,
		&p.PublishedID, &p.ProposedByID, &p.ProposedBy, &p.ReviewedBy, &at)
	json.Unmarshal([]byte(applies), &p.AppliesTo)
	json.Unmarshal([]byte(steps), &p.Steps)
	p.ProposedAt, _ = time.Parse(time.RFC3339, at)
	return p, err
}

// GetProposal returns a checklist proposal.
func GetProposal(ctx context.Context, q Querier, id string) (Proposal, error) {
	p, err := scanProposal(q.QueryRowContext(ctx, `SELECT `+proposalColumns+` WHERE p.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// ListProposals lists proposals: those waiting (byUser "" for everyone's),
// or the latest 20 decided ones of one person.
func ListProposals(ctx context.Context, q Querier, status, byUser string) ([]Proposal, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+proposalColumns+` WHERE p.status = ? AND (? = '' OR p.proposed_by = ?)
		ORDER BY p.proposed_at DESC LIMIT 50`, status, byUser, byUser)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Proposal
	for rows.Next() {
		p, err := scanProposal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
