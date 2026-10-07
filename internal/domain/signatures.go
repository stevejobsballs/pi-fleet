package domain

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"sort"
	"time"

	"pi-fleet/internal/canon"
	"pi-fleet/internal/event"
	"pi-fleet/internal/store"
)

// Signature event types and meanings (DESIGN.md §7.2).
const (
	TypeSignatureApplied   = "signature.applied"
	TypeSignatureWithdrawn = "signature.withdrawn"
	EntitySignature        = "signature"

	MeaningPerformed = "performed"
	MeaningReviewed  = "reviewed"
	MeaningApproved  = "approved"
)

// MeaningText is shown to the signer and printed with the signature
// (Part 11 §11.50).
var MeaningText = map[string]string{
	MeaningPerformed: "I performed this work as recorded.",
	MeaningReviewed:  "I reviewed this record and confirm it is complete and correct.",
	MeaningApproved:  "I approve this record for closure.",
}

// signatureTransition maps each meaning to the work order transition it
// authorises: signing is what moves the record forward.
var signatureTransition = map[string][2]string{
	MeaningPerformed: {WOInProgress, WOCompleted},
	MeaningReviewed:  {WOCompleted, WOReviewed},
	MeaningApproved:  {WOReviewed, WOClosed},
}

// reauthWindow is how recently the signer must have re-entered their
// password (DESIGN.md §4.5).
const reauthWindow = 60 * time.Second

// SignatureApplied is an electronic signature on a work order. Its
// content hash binds it to the record as it stood (Part 11 §11.70).
type SignatureApplied struct {
	TargetType      string `json:"target_type"`
	TargetID        string `json:"target_id"`
	Meaning         string `json:"meaning"`
	MeaningText     string `json:"meaning_text"`
	ContentHash     string `json:"content_hash"`
	SignerLegalName string `json:"signer_legal_name"`
	SignerUsername  string `json:"signer_username"`
	AuthMethod      string `json:"auth_method"` // "password"
	AuthAt          string `json:"auth_at"`     // RFC 3339, when the password was re-entered
	// ClockWarningAcknowledged records that the signer saw and accepted
	// the unverified-clock warning (decision D5).
	ClockWarningAcknowledged bool `json:"clock_warning_acknowledged"`
}

// SignatureWithdrawn withdraws a signature. The original stays visible.
type SignatureWithdrawn struct {
	Reason string `json:"reason"`
}

func init() {
	payloadTypes[TypeSignatureApplied] = struct {
		entity string
		new    func() any
	}{EntitySignature, func() any { return &SignatureApplied{} }}
	payloadTypes[TypeSignatureWithdrawn] = struct {
		entity string
		new    func() any
	}{EntitySignature, func() any { return &SignatureWithdrawn{} }}
}

// WorkOrderContentHash hashes what a signature attests to: the work
// order's descriptive fields, its valid calibration records with every
// reading, its attached files (certificates, photos) by hash, and its
// checklist with every recorded step. Status and assignment are excluded, since signing changes
// them. Nodes and central compute the same value from the same state.
func WorkOrderContentHash(ctx context.Context, q Querier, woID string) (string, error) {
	w, err := GetWorkOrder(ctx, q, woID)
	if err != nil {
		return "", err
	}
	content := map[string]any{
		"work_order": map[string]any{
			"id": w.ID, "number": w.Number, "type": w.Type, "asset_id": w.AssetID,
			"title": w.Title, "problem": w.Problem, "due_at": w.DueAt,
		},
	}
	rows, err := q.QueryContext(ctx, `SELECT id, procedure, adjusted, temperature, humidity, as_found_result, as_left_result,
		certificate_sha256 FROM calibration_records WHERE wo_id = ? AND status = 'valid' ORDER BY id`, woID)
	if err != nil {
		return "", err
	}
	var cals []any
	var ids []string
	for rows.Next() {
		var id, proc, temp, hum, found, left, cert string
		var adjusted int64
		if err := rows.Scan(&id, &proc, &adjusted, &temp, &hum, &found, &left, &cert); err != nil {
			rows.Close()
			return "", err
		}
		ids = append(ids, id)
		cals = append(cals, map[string]any{
			"id": id, "procedure": proc, "adjusted": adjusted, "temperature": temp, "humidity": hum,
			"as_found_result": found, "as_left_result": left, "certificate_sha256": cert,
		})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}
	for i, id := range ids {
		points, err := queryStrings(ctx, q, `SELECT parameter || char(31) || unit || char(31) || nominal || char(31) || tolerance
			|| char(31) || as_found || char(31) || as_left FROM cal_points WHERE record_id = ? ORDER BY idx`, id)
		if err != nil {
			return "", err
		}
		standards, err := queryStrings(ctx, q, `SELECT standard_asset_id FROM cal_standards WHERE record_id = ?`, id)
		if err != nil {
			return "", err
		}
		sort.Strings(standards)
		c := cals[i].(map[string]any)
		c["points"], c["standards"] = toAny(points), toAny(standards)
	}
	content["calibrations"] = toAny(cals)
	files, err := queryStrings(ctx, q, `SELECT sha256 || ' ' || filename FROM attachments
		WHERE target_type = 'work_order' AND target_id = ? AND status = 'attached' ORDER BY sha256, filename`, woID)
	if err != nil {
		return "", err
	}
	content["attachments"] = toAny(files)
	content["procedure_id"] = w.ProcedureID
	steps, err := queryStrings(ctx, q, `SELECT step_id || char(31) || value || char(31) || note FROM checklist_results
		WHERE wo_id = ? ORDER BY step_id`, woID)
	if err != nil {
		return "", err
	}
	content["checklist"] = toAny(steps)
	b, err := canon.Marshal(content)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func queryStrings(ctx context.Context, q Querier, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func toAny[T any](s []T) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func (ap *applier) signatureApplied(p *SignatureApplied) error {
	if err := ap.require(RoleUser); err != nil {
		return err
	}
	if p.TargetType != EntityWorkOrder {
		return invalid("only work orders can be signed so far")
	}
	tr, ok := signatureTransition[p.Meaning]
	if !ok {
		return invalid("unknown signature meaning %q", p.Meaning)
	}
	if p.MeaningText != MeaningText[p.Meaning] {
		return invalid("the meaning statement does not match %q", p.Meaning)
	}
	if ok, err := ap.exists(`SELECT 1 FROM signatures WHERE id = ?`, ap.e.EventID); err != nil || ok {
		return orConflict(err, "signature already recorded")
	}
	w, err := ap.workOrderFor(p.TargetID)
	if err != nil {
		return err
	}
	if w.Status != tr[0] {
		return store.Reject(FlagStaleBase, "a %s signature needs the work order %s, but it is %s", p.Meaning, tr[0], w.Status)
	}
	u, err := GetUser(ap.ctx, ap.tx, ap.actor.id)
	if err != nil {
		return err
	}
	if p.SignerLegalName != u.LegalName || p.SignerUsername != u.Username {
		return invalid("signer name does not match the signing user's account")
	}
	if p.AuthMethod != "password" {
		return invalid("signatures require password re-entry")
	}
	authAt, err := time.Parse(time.RFC3339, p.AuthAt)
	if err != nil {
		return invalid("auth_at: %v", err)
	}
	if d := ap.e.WallTime.Sub(authAt); d < -time.Second || d > reauthWindow {
		return invalid("the password must be re-entered within %s of signing", reauthWindow)
	}
	if ap.e.ClockState == event.ClockUnverified && !p.ClockWarningAcknowledged {
		return invalid("signing with an unverified clock requires acknowledging the warning")
	}
	switch p.Meaning {
	case MeaningPerformed:
		if err := ap.checkLease(w); err != nil {
			return err
		}
	case MeaningReviewed:
		if err := ap.require(RoleMidTier); err != nil {
			return err
		}
		if ap.actor.id == w.AssignedTo {
			return store.Reject(FlagNotAuthorized, "the reviewer must be a different user from the performer")
		}
	case MeaningApproved:
		if err := ap.require(RoleMidTier); err != nil {
			return err
		}
	}
	current, err := WorkOrderContentHash(ap.ctx, ap.tx, w.ID)
	if err != nil {
		return err
	}
	if p.ContentHash != current {
		return store.Reject(FlagStaleBase, "the work order changed before it was signed")
	}
	return ap.exec(`INSERT INTO signatures (id, target_type, target_id, meaning, signer_user_id, signer_legal_name,
			signer_username, signed_at, clock_state, content_hash, sign_round, status, withdraw_reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'valid', '')`,
		ap.e.EventID, p.TargetType, w.ID, p.Meaning, u.ID, u.LegalName, u.Username, ap.wall(),
		string(ap.e.ClockState), current, w.SignRound)
}

func (ap *applier) signatureWithdrawn(p *SignatureWithdrawn) error {
	if blank(p.Reason) {
		return invalid("withdrawing a signature requires a reason")
	}
	var signer, status string
	err := ap.tx.QueryRowContext(ap.ctx, `SELECT signer_user_id, status FROM signatures WHERE id = ?`, ap.e.EntityID).Scan(&signer, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return invalid("signature %s not found", ap.e.EntityID)
	}
	if err != nil {
		return err
	}
	if signer != ap.actor.id {
		return store.Reject(FlagNotAuthorized, "only the signer can withdraw a signature")
	}
	if status != "valid" {
		return invalid("signature is already %s", status)
	}
	return ap.exec(`UPDATE signatures SET status = 'withdrawn', withdraw_reason = ? WHERE id = ?`, p.Reason, ap.e.EntityID)
}

// checkSigned requires a valid signature with the given meaning, by the
// actor, in the work order's current signing round, over its current
// content, before the matching transition.
func (ap *applier) checkSigned(w WorkOrder, to string) error {
	var meaning string
	for m, tr := range signatureTransition {
		if tr[1] == to {
			meaning = m
		}
	}
	if meaning == "" {
		return nil
	}
	current, err := WorkOrderContentHash(ap.ctx, ap.tx, w.ID)
	if err != nil {
		return err
	}
	ok, err := ap.exists(`SELECT 1 FROM signatures WHERE target_type = ? AND target_id = ? AND meaning = ?
		AND signer_user_id = ? AND sign_round = ? AND content_hash = ? AND status = 'valid'`,
		EntityWorkOrder, w.ID, meaning, ap.actor.id, w.SignRound, current)
	if err != nil {
		return err
	}
	if !ok {
		return invalid("moving %s to %s needs your %q signature (Part 11)", w.Number, to, meaning)
	}
	return nil
}

// Signature is a projected signature with its current standing.
type Signature struct {
	ID              string
	Meaning         string
	SignerLegalName string
	SignerUsername  string
	SignedAt        time.Time
	ClockState      string
	Status          string // valid, withdrawn
	WithdrawReason  string
	// Stale means the record changed after signing, or was reopened.
	Stale bool
}

// WorkOrderSignatures lists a work order's signatures, oldest first,
// marking those that no longer match the record.
func WorkOrderSignatures(ctx context.Context, q Querier, woID string) ([]Signature, error) {
	w, err := GetWorkOrder(ctx, q, woID)
	if err != nil {
		return nil, err
	}
	current, err := WorkOrderContentHash(ctx, q, woID)
	if err != nil {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, `SELECT id, meaning, signer_legal_name, signer_username, signed_at, clock_state,
		status, withdraw_reason, content_hash, sign_round FROM signatures WHERE target_type = ? AND target_id = ? ORDER BY signed_at, id`,
		EntityWorkOrder, woID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Signature
	for rows.Next() {
		var s Signature
		var signedAt, hash string
		var round int64
		if err := rows.Scan(&s.ID, &s.Meaning, &s.SignerLegalName, &s.SignerUsername, &signedAt, &s.ClockState,
			&s.Status, &s.WithdrawReason, &hash, &round); err != nil {
			return nil, err
		}
		s.SignedAt, _ = time.Parse(time.RFC3339, signedAt)
		s.Stale = hash != current || round != w.SignRound
		out = append(out, s)
	}
	return out, rows.Err()
}
