package domain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"pi-fleet/internal/store"
)

// Event types for checklists and labour (DESIGN.md §4.3–4.4).
const (
	TypeProcedurePublished        = "procedure.published"
	TypeProcedureRetired          = "procedure.retired"
	TypeWorkOrderProcedureSet     = "workorder.procedure_set"
	TypeWorkOrderStepRecorded     = "workorder.step_recorded"
	TypeWorkOrderChecklistAdded   = "workorder.checklist_added"
	TypeWorkOrderChecklistRemoved = "workorder.checklist_removed"
	TypeLaborLogged               = "labor.logged"
	TypeLaborReversed             = "labor.reversed"
	EntityProcedure               = "procedure_version"
	EntityLabor                   = "labor_entry"
)

// Kinds of checklist step. Headings and instructions take no answer.
const (
	StepCheck    = "check"    // pass, fail or n/a
	StepNumber   = "number"   // a reading, with optional limits
	StepText     = "text"     // a short answer
	StepLongText = "longtext" // notes, several lines
	StepChoice   = "choice"   // one of a list; some options may count as a fail
	StepMulti    = "multi"    // any of a list (tick all that apply)
	StepDate     = "date"     // YYYY-MM-DD
	StepPhoto    = "photo"    // one or more photos; answered once one is added
	StepHeading  = "heading"  // starts a section
	StepInfo     = "info"     // an instruction or warning to read
)

// StepKinds are the kinds of step, in the order offered.
var StepKinds = []string{StepCheck, StepNumber, StepChoice, StepMulti, StepText, StepLongText, StepDate, StepPhoto, StepHeading, StepInfo}

// Inputless reports whether a kind of step takes no answer.
func Inputless(kind string) bool { return kind == StepHeading || kind == StepInfo }

// CategoryOptions suggest what a checklist is for.
var CategoryOptions = []string{"Preventive maintenance", "Electrical safety", "Performance check", "Calibration", "Inspection",
	"Incoming inspection", "Repair", "Decommissioning", "Other"}

var stepIDRE = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// Step is one checklist step.
type Step struct {
	ID    string `json:"id"`
	Text  string `json:"text"`
	Kind  string `json:"kind"`            // one of StepKinds
	Help  string `json:"help,omitempty"`  // how to do it, shown under the step
	Unit  string `json:"unit,omitempty"`  // number; check: of the measured value
	Lower string `json:"lower,omitempty"` // number: inclusive limits, decimals
	Upper string `json:"upper,omitempty"`
	// Options are the answers to a choice or multi step.
	Options  []StepOption `json:"options,omitempty"`
	Required bool         `json:"required"`
	// Critical: if it fails, the work order can't be completed while the
	// equipment is in service (take it out of service first).
	Critical bool `json:"critical,omitempty"`
	// NoteOnFail: a failing answer needs a note saying why.
	NoteOnFail bool `json:"note_on_fail,omitempty"`
	// ShowIf: the step applies only when an earlier step has this answer
	// (for a multi step, when it includes it).
	ShowIf *StepCondition `json:"show_if,omitempty"`
}

// StepOption is one answer to a choice or multi step.
type StepOption struct {
	Label string `json:"label"`
	Fails bool   `json:"fails,omitempty"` // choosing it fails the step
}

// StepCondition makes a step depend on an earlier step's answer.
type StepCondition struct {
	Step   string `json:"step"`
	Equals string `json:"equals"`
}

// ProcedurePublished publishes a checklist version. Versions are
// immutable; to change a checklist, publish the next version.
type ProcedurePublished struct {
	Name        string `json:"name"`
	Version     int    `json:"version"`
	Description string `json:"description,omitempty"`
	Reference   string `json:"reference,omitempty"` // the standard or manual it follows
	Category    string `json:"category,omitempty"`
	// AppliesTo is the equipment it is for; empty, any equipment.
	AppliesTo []EquipmentMatch `json:"applies_to,omitempty"`
	Steps     []Step           `json:"steps"`
}

// EquipmentMatch names equipment a checklist is for: a manufacturer's
// model, or (Model empty) every model of the manufacturer.
type EquipmentMatch struct {
	Manufacturer string `json:"manufacturer"`
	Model        string `json:"model,omitempty"`
}

// Matches reports whether equipment of this manufacturer and model is
// meant (ignoring case and surrounding spaces).
func (m EquipmentMatch) Matches(manufacturer, model string) bool {
	return normName(m.Manufacturer) == normName(manufacturer) && (m.Model == "" || normName(m.Model) == normName(model))
}

func normName(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

// WorkOrderChecklistAdded adds a checklist to a work order: the first
// becomes its checklist, later ones are added beside it.
type WorkOrderChecklistAdded struct {
	ProcedureID string `json:"procedure_id"`
}

// WorkOrderChecklistRemoved removes a checklist nothing was recorded in.
type WorkOrderChecklistRemoved struct {
	ProcedureID string `json:"procedure_id"`
}

type ProcedureRetired struct {
	Reason string `json:"reason"`
}

// WorkOrderProcedureSet chooses the checklist for a work order before
// work starts.
type WorkOrderProcedureSet struct {
	ProcedureID string `json:"procedure_id"`
}

// WorkOrderStepRecorded records (or re-records) one checklist step. An
// empty ProcedureID means the work order's first checklist.
type WorkOrderStepRecorded struct {
	ProcedureID string `json:"procedure_id,omitempty"`
	StepID      string `json:"step_id"`
	// Measured is the value measured to decide a pass/fail step, if any.
	Measured string `json:"measured,omitempty"`
	Value    string `json:"value"`
	Note     string `json:"note,omitempty"`
}

// LaborLogged is time a user spent on a work order.
type LaborLogged struct {
	WorkOrderID string `json:"work_order_id"`
	Minutes     int    `json:"minutes"`
	Date        string `json:"date"` // YYYY-MM-DD
	Note        string `json:"note,omitempty"`
}

type LaborReversed struct {
	Reason string `json:"reason"`
}

func init() {
	reg := func(typ, entity string, f func() any) {
		payloadTypes[typ] = struct {
			entity string
			new    func() any
		}{entity, f}
	}
	reg(TypeProcedurePublished, EntityProcedure, func() any { return &ProcedurePublished{} })
	reg(TypeProcedureRetired, EntityProcedure, func() any { return &ProcedureRetired{} })
	reg(TypeWorkOrderProcedureSet, EntityWorkOrder, func() any { return &WorkOrderProcedureSet{} })
	reg(TypeWorkOrderStepRecorded, EntityWorkOrder, func() any { return &WorkOrderStepRecorded{} })
	reg(TypeWorkOrderChecklistAdded, EntityWorkOrder, func() any { return &WorkOrderChecklistAdded{} })
	reg(TypeWorkOrderChecklistRemoved, EntityWorkOrder, func() any { return &WorkOrderChecklistRemoved{} })
	reg(TypeLaborLogged, EntityLabor, func() any { return &LaborLogged{} })
	reg(TypeLaborReversed, EntityLabor, func() any { return &LaborReversed{} })
}

func validateSteps(steps []Step) error {
	if len(steps) == 0 || len(steps) > 300 {
		return invalid("a checklist needs 1 to 300 steps")
	}
	seen := map[string]Step{}
	answers := 0
	for i, st := range steps {
		n := i + 1
		if !stepIDRE.MatchString(st.ID) || seen[st.ID].ID != "" {
			return invalid("step %d: id %q must be unique, 1-32 lowercase letters, digits, - or _", n, st.ID)
		}
		if blank(st.Text) || len(st.Text) > 500 || len(st.Help) > 2000 {
			return invalid("step %d needs text (at most 500 characters; help at most 2000)", n)
		}
		if st.Kind != StepNumber && (st.Lower != "" || st.Upper != "") {
			return invalid("step %d: only reading steps have limits", n)
		}
		if st.Kind != StepNumber && st.Kind != StepCheck && st.Unit != "" {
			return invalid("step %d: only readings and pass/fail steps have a unit", n)
		}
		if len(st.Unit) > 20 {
			return invalid("step %d: the unit is too long", n)
		}
		if st.Kind != StepChoice && st.Kind != StepMulti && len(st.Options) > 0 {
			return invalid("step %d: only choice steps have options", n)
		}
		switch st.Kind {
		case StepCheck, StepText, StepLongText, StepDate, StepPhoto:
		case StepNumber:
			for _, v := range []string{st.Lower, st.Upper} {
				if v != "" {
					if _, err := parseDecimal(v); err != nil {
						return invalid("step %d limit: %v", n, err)
					}
				}
			}
			if st.Lower != "" && st.Upper != "" {
				lo, _ := parseDecimal(st.Lower)
				hi, _ := parseDecimal(st.Upper)
				if lo.Cmp(hi) > 0 {
					return invalid("step %d: lower limit exceeds upper limit", n)
				}
			}
		case StepChoice, StepMulti:
			if len(st.Options) < 2 || len(st.Options) > 40 {
				return invalid("step %d: a choice needs 2 to 40 options", n)
			}
			labels := map[string]bool{}
			for _, o := range st.Options {
				if blank(o.Label) || len(o.Label) > 120 || labels[o.Label] {
					return invalid("step %d: options must be different and not empty", n)
				}
				labels[o.Label] = true
			}
		case StepHeading, StepInfo:
			if st.Required || st.Critical || st.NoteOnFail {
				return invalid("step %d: headings and instructions take no answer, so they can't be required or critical", n)
			}
		default:
			return invalid("step %d: unknown kind %q", n, st.Kind)
		}
		if !Inputless(st.Kind) {
			answers++
		}
		if (st.Critical || st.NoteOnFail) && !canFail(st) {
			return invalid("step %d: only steps that can fail (pass/fail, readings with limits, choices with failing options) can be critical or need a note on failing", n)
		}
		if c := st.ShowIf; c != nil {
			parent, ok := seen[c.Step]
			if !ok || Inputless(parent.Kind) {
				return invalid("step %d: it can only depend on an earlier step that takes an answer", n)
			}
			if blank(c.Equals) {
				return invalid("step %d: say which answer of the earlier step it depends on", n)
			}
			if parent.Kind == StepChoice || parent.Kind == StepMulti {
				found := false
				for _, o := range parent.Options {
					found = found || o.Label == c.Equals
				}
				if !found {
					return invalid("step %d depends on %q, which isn't one of step %q's options", n, c.Equals, parent.Text)
				}
			}
		}
		seen[st.ID] = st
	}
	if answers == 0 {
		return invalid("a checklist needs at least one step that takes an answer")
	}
	return nil
}

// canFail reports whether a step has a failing answer.
func canFail(st Step) bool {
	switch st.Kind {
	case StepCheck:
		return true
	case StepNumber:
		return st.Lower != "" || st.Upper != ""
	case StepChoice, StepMulti:
		for _, o := range st.Options {
			if o.Fails {
				return true
			}
		}
	}
	return false
}

func (ap *applier) procedurePublished(p *ProcedurePublished) error {
	if err := ap.require(RoleMidTier); err != nil {
		return err
	}
	if err := ap.newEntity("procedures"); err != nil {
		return err
	}
	if err := validateChecklist(*p); err != nil {
		return err
	}
	var latest int
	if err := ap.tx.QueryRowContext(ap.ctx, `SELECT coalesce(max(version), 0) FROM procedures WHERE name = ?`, p.Name).Scan(&latest); err != nil {
		return err
	}
	if p.Version != latest+1 {
		return store.Reject(FlagConflict, "%s is at version %d; this would publish version %d", p.Name, latest, p.Version)
	}
	steps, _ := json.Marshal(p.Steps)
	applies, _ := json.Marshal(nonNilMatches(p.AppliesTo))
	return ap.exec(`INSERT INTO procedures (id, name, version, steps, status, published_by, published_at, last_event_id, description, reference, category, applies_to)
		VALUES (?, ?, ?, ?, 'active', ?, ?, ?, ?, ?, ?, ?)`, ap.e.EntityID, p.Name, p.Version, string(steps), ap.actor.id, ap.wall(), ap.e.EventID,
		p.Description, p.Reference, p.Category, string(applies))
}

func (ap *applier) procedureRetired(p *ProcedureRetired) error {
	if err := ap.require(RoleMidTier); err != nil {
		return err
	}
	if blank(p.Reason) {
		return invalid("retiring a checklist requires a reason")
	}
	if ok, err := ap.exists(`SELECT 1 FROM procedures WHERE id = ? AND status = 'active'`, ap.e.EntityID); err != nil || !ok {
		return orInvalid(err, "no active checklist version %s", ap.e.EntityID)
	}
	return ap.exec(`UPDATE procedures SET status = 'retired', last_event_id = ? WHERE id = ?`, ap.e.EventID, ap.e.EntityID)
}

// activeProcedure checks a checklist version can be newly used.
func (ap *applier) activeProcedure(id string) error {
	if ok, err := ap.exists(`SELECT 1 FROM procedures WHERE id = ? AND status = 'active'`, id); err != nil || !ok {
		return orInvalid(err, "checklist %s is not an active version", id)
	}
	return nil
}

func (ap *applier) workOrderProcedureSet(p *WorkOrderProcedureSet) error {
	if err := ap.require(RoleMidTier); err != nil {
		return err
	}
	w, err := ap.targetWorkOrder()
	if err != nil {
		return err
	}
	if w.Status != WOOpen && w.Status != WOAssigned {
		return invalid("the checklist can only be chosen before work starts")
	}
	if p.ProcedureID != "" {
		if err := ap.activeProcedure(p.ProcedureID); err != nil {
			return err
		}
	}
	return ap.bumpWorkOrder(`procedure_id = ?`, p.ProcedureID)
}

// Procedure is a projected checklist version.
type Procedure struct {
	ID, Name, Status                 string
	Description, Reference, Category string
	Version                          int
	AppliesTo                        []EquipmentMatch
	Steps                            []Step
	PublishedAt                      time.Time
}

func nonNilMatches(m []EquipmentMatch) []EquipmentMatch {
	if m == nil {
		return []EquipmentMatch{}
	}
	return m
}

// GetProcedure returns a checklist version.
func GetProcedure(ctx context.Context, q Querier, id string) (Procedure, error) {
	var p Procedure
	var steps, at, applies string
	err := q.QueryRowContext(ctx, `SELECT id, name, version, steps, status, published_at, description, reference, category, applies_to FROM procedures WHERE id = ?`, id).
		Scan(&p.ID, &p.Name, &p.Version, &steps, &p.Status, &at, &p.Description, &p.Reference, &p.Category, &applies)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	if err != nil {
		return p, err
	}
	p.PublishedAt, _ = time.Parse(time.RFC3339, at)
	json.Unmarshal([]byte(applies), &p.AppliesTo)
	return p, json.Unmarshal([]byte(steps), &p.Steps)
}

func (ap *applier) workOrderStepRecorded(p *WorkOrderStepRecorded) error {
	w, err := ap.targetWorkOrder()
	if err != nil {
		return err
	}
	if w.Status != WOInProgress {
		return invalid("checklist steps are recorded while %s is in progress", w.Number)
	}
	if err := ap.checkLease(w); err != nil {
		return err
	}
	procID := p.ProcedureID
	if procID == "" {
		procID = w.ProcedureID
	}
	if procID == "" {
		return invalid("%s has no checklist", w.Number)
	}
	if on, err := ap.checklistOn(w, procID); err != nil || !on {
		return orInvalid(err, "that checklist isn't on %s", w.Number)
	}
	proc, err := GetProcedure(ap.ctx, ap.tx, procID)
	if err != nil {
		return err
	}
	var step *Step
	for i := range proc.Steps {
		if proc.Steps[i].ID == p.StepID {
			step = &proc.Steps[i]
		}
	}
	if step == nil {
		return invalid("checklist %s v%d has no step %q", proc.Name, proc.Version, p.StepID)
	}
	if Inputless(step.Kind) {
		return invalid("%q takes no answer", step.Text)
	}
	answers, err := ap.stepAnswers(w.ID, proc.ID)
	if err != nil {
		return err
	}
	if !StepApplies(*step, answers) {
		return invalid("%q only applies when %q is %q", step.Text, stepText(proc.Steps, step.ShowIf.Step), step.ShowIf.Equals)
	}
	pass, err := evaluateStep(*step, p.Value)
	if err != nil {
		return invalid("%q: %v", step.Text, err)
	}
	if p.Measured != "" && (step.Kind != StepCheck || len(p.Measured) > 60) {
		return invalid("%q: a measured value goes with a pass/fail step, in at most 60 characters", step.Text)
	}
	if pass != nil && !*pass && step.NoteOnFail && blank(p.Note) {
		return invalid("%q failed: say why in the note", step.Text)
	}
	if err := ap.exec(`INSERT INTO checklist_results (wo_id, procedure_id, step_id, value, pass, note, recorded_by, recorded_at, event_id, measured)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (wo_id, procedure_id, step_id) DO UPDATE SET value = excluded.value, pass = excluded.pass,
		note = excluded.note, recorded_by = excluded.recorded_by, recorded_at = excluded.recorded_at, event_id = excluded.event_id,
		measured = excluded.measured`,
		w.ID, proc.ID, step.ID, p.Value, pass, p.Note, ap.actor.id, ap.wall(), ap.e.EventID, strings.TrimSpace(p.Measured)); err != nil {
		return err
	}
	return ap.exec(`UPDATE work_orders SET version = version + 1, last_event_id = ? WHERE id = ?`, ap.e.EventID, w.ID)
}

// evaluateStep checks a value against its step and returns pass/fail
// (nil when the step has no pass/fail).
func evaluateStep(st Step, value string) (*bool, error) {
	yes, no := true, false
	switch st.Kind {
	case StepCheck:
		switch value {
		case "pass":
			return &yes, nil
		case "fail":
			return &no, nil
		case "n/a":
			if st.Required {
				return nil, errors.New("a required check can't be marked not applicable")
			}
			return nil, nil
		}
		return nil, errors.New(`a check is "pass", "fail" or "n/a"`)
	case StepNumber:
		v, err := parseDecimal(value)
		if err != nil {
			return nil, err
		}
		if st.Lower == "" && st.Upper == "" {
			return nil, nil
		}
		ok := true
		if st.Lower != "" {
			lo, _ := parseDecimal(st.Lower)
			ok = ok && v.Cmp(lo) >= 0
		}
		if st.Upper != "" {
			hi, _ := parseDecimal(st.Upper)
			ok = ok && v.Cmp(hi) <= 0
		}
		return &ok, nil
	case StepText, StepLongText:
		if blank(value) {
			return nil, errors.New("enter some text")
		}
		return nil, nil
	case StepDate:
		if _, err := time.Parse(dateLayout, value); err != nil {
			return nil, errors.New("enter a date")
		}
		return nil, nil
	case StepPhoto:
		if blank(value) {
			return nil, errors.New("add a photo")
		}
		return nil, nil
	case StepChoice:
		for _, o := range st.Options {
			if o.Label == value {
				if !canFail(st) {
					return nil, nil
				}
				ok := !o.Fails
				return &ok, nil
			}
		}
		return nil, errors.New("choose one of the options")
	case StepMulti:
		chosen, err := MultiValues(value)
		if err != nil || (st.Required && len(chosen) == 0) {
			return nil, errors.New("tick at least one option")
		}
		ok := true
		for _, c := range chosen {
			found := false
			for _, o := range st.Options {
				if o.Label == c {
					found, ok = true, ok && !o.Fails
				}
			}
			if !found {
				return nil, fmt.Errorf("%q isn't one of the options", c)
			}
		}
		if !canFail(st) {
			return nil, nil
		}
		return &ok, nil
	}
	return nil, errors.New("unknown step kind")
}

// MultiValues reads the answer to a multi step: a JSON list of options.
func MultiValues(value string) ([]string, error) {
	var out []string
	err := json.Unmarshal([]byte(value), &out)
	return out, err
}

// StepApplies reports whether a step applies given the answers so far.
func StepApplies(st Step, answers map[string]string) bool {
	c := st.ShowIf
	if c == nil {
		return true
	}
	v, ok := answers[c.Step]
	if !ok {
		return false
	}
	if v == c.Equals {
		return true
	}
	if chosen, err := MultiValues(v); err == nil {
		for _, x := range chosen {
			if x == c.Equals {
				return true
			}
		}
	}
	return false
}

func stepText(steps []Step, id string) string {
	for _, s := range steps {
		if s.ID == id {
			return s.Text
		}
	}
	return id
}

// stepAnswers are a work order's recorded answers to one checklist, by step.
func (ap *applier) stepAnswers(woID, procID string) (map[string]string, error) {
	rows, err := ap.tx.QueryContext(ap.ctx, `SELECT step_id, value FROM checklist_results WHERE wo_id = ? AND procedure_id = ?`, woID, procID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, v string
		if err := rows.Scan(&id, &v); err != nil {
			return nil, err
		}
		out[id] = v
	}
	return out, rows.Err()
}

// checkChecklist requires, in every checklist on the work order, each
// required step that applies, and that equipment which failed a critical
// step is out of service.
func (ap *applier) checkChecklist(w WorkOrder) error {
	ids, err := checklistIDs(ap.ctx, ap.tx, w)
	if err != nil {
		return err
	}
	for _, id := range ids {
		proc, err := GetProcedure(ap.ctx, ap.tx, id)
		if err != nil {
			return err
		}
		answers, err := ap.stepAnswers(w.ID, id)
		if err != nil {
			return err
		}
		for _, st := range proc.Steps {
			if Inputless(st.Kind) || !StepApplies(st, answers) {
				continue
			}
			if _, done := answers[st.ID]; st.Required && !done {
				return invalid("required checklist step %q (%s) is not recorded", st.Text, proc.Name)
			}
			if !st.Critical {
				continue
			}
			var failed bool
			err := ap.tx.QueryRowContext(ap.ctx, `SELECT pass = 0 FROM checklist_results WHERE wo_id = ? AND procedure_id = ? AND step_id = ? AND pass IS NOT NULL`,
				w.ID, id, st.ID).Scan(&failed)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if failed {
				var status string
				if err := ap.tx.QueryRowContext(ap.ctx, `SELECT status FROM assets WHERE id = ?`, w.AssetID).Scan(&status); err != nil {
					return err
				}
				if status == AssetInService {
					return invalid("critical step %q failed: take the equipment out of service before completing the work order", st.Text)
				}
			}
		}
	}
	return nil
}

// checklistIDs are the checklists on a work order: its first, then those
// added, in the order added.
func checklistIDs(ctx context.Context, q Querier, w WorkOrder) ([]string, error) {
	var out []string
	if w.ProcedureID != "" {
		out = append(out, w.ProcedureID)
	}
	rows, err := q.QueryContext(ctx, `SELECT procedure_id FROM work_order_checklists WHERE wo_id = ? ORDER BY added_at, procedure_id`, w.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (ap *applier) checklistOn(w WorkOrder, procID string) (bool, error) {
	ids, err := checklistIDs(ap.ctx, ap.tx, w)
	return slices.Contains(ids, procID), err
}

// checklistEditor checks the actor may add or remove checklists: the
// person doing the work (holding its lease), or a mid-tier user, while
// the work isn't finished.
func (ap *applier) checklistEditor(w WorkOrder) error {
	switch w.Status {
	case WOOpen, WOAssigned, WOInProgress:
	default:
		return invalid("checklists can't be changed once %s is %s", w.Number, w.Status)
	}
	if ap.actor.atLeast(RoleMidTier) {
		return nil
	}
	if w.AssignedTo != ap.actor.id {
		return store.Reject(FlagNotAuthorized, "only the person doing %s, or a mid-tier user, can change its checklists", w.Number)
	}
	return ap.checkLease(w)
}

func (ap *applier) workOrderChecklistAdded(p *WorkOrderChecklistAdded) error {
	w, err := ap.targetWorkOrder()
	if err != nil {
		return err
	}
	if err := ap.checklistEditor(w); err != nil {
		return err
	}
	if err := ap.activeProcedure(p.ProcedureID); err != nil {
		return err
	}
	if on, err := ap.checklistOn(w, p.ProcedureID); err != nil || on {
		return orConflict(err, "that checklist is already on %s", w.Number)
	}
	if w.ProcedureID == "" {
		return ap.bumpWorkOrder(`procedure_id = ?`, p.ProcedureID)
	}
	if err := ap.exec(`INSERT INTO work_order_checklists (wo_id, procedure_id, added_by, added_at, last_event_id) VALUES (?, ?, ?, ?, ?)`,
		w.ID, p.ProcedureID, ap.actor.id, ap.wall(), ap.e.EventID); err != nil {
		return err
	}
	return ap.exec(`UPDATE work_orders SET version = version + 1, last_event_id = ? WHERE id = ?`, ap.e.EventID, w.ID)
}

func (ap *applier) workOrderChecklistRemoved(p *WorkOrderChecklistRemoved) error {
	w, err := ap.targetWorkOrder()
	if err != nil {
		return err
	}
	if err := ap.checklistEditor(w); err != nil {
		return err
	}
	if on, err := ap.checklistOn(w, p.ProcedureID); err != nil || !on {
		return orInvalid(err, "that checklist isn't on %s", w.Number)
	}
	if ok, err := ap.exists(`SELECT 1 FROM checklist_results WHERE wo_id = ? AND procedure_id = ?`, w.ID, p.ProcedureID); err != nil || ok {
		return orInvalid(err, "answers are already recorded in that checklist, so it stays")
	}
	if p.ProcedureID == w.ProcedureID {
		return ap.bumpWorkOrder(`procedure_id = ''`)
	}
	if err := ap.exec(`DELETE FROM work_order_checklists WHERE wo_id = ? AND procedure_id = ?`, w.ID, p.ProcedureID); err != nil {
		return err
	}
	return ap.exec(`UPDATE work_orders SET version = version + 1, last_event_id = ? WHERE id = ?`, ap.e.EventID, w.ID)
}

func (ap *applier) laborLogged(p *LaborLogged) error {
	if err := ap.require(RoleUser); err != nil {
		return err
	}
	if err := ap.newEntity("labor_entries"); err != nil {
		return err
	}
	w, err := ap.workOrderFor(p.WorkOrderID)
	if err != nil {
		return err
	}
	if w.Status == WOCancelled {
		return invalid("%s is cancelled", w.Number)
	}
	if p.Minutes < 1 || p.Minutes > 24*60 {
		return invalid("minutes must be between 1 and 1440")
	}
	d, err := time.Parse(dateLayout, p.Date)
	if err != nil {
		return invalid("date %q must be YYYY-MM-DD", p.Date)
	}
	if d.After(ap.e.WallTime.Add(24 * time.Hour)) {
		return invalid("time can't be logged for a future date")
	}
	return ap.exec(`INSERT INTO labor_entries (id, wo_id, user_id, minutes, work_date, note, status, reverse_reason, last_event_id)
		VALUES (?, ?, ?, ?, ?, ?, 'logged', '', ?)`, ap.e.EntityID, w.ID, ap.actor.id, p.Minutes, p.Date, p.Note, ap.e.EventID)
}

func (ap *applier) laborReversed(p *LaborReversed) error {
	var userID, status string
	err := ap.tx.QueryRowContext(ap.ctx, `SELECT user_id, status FROM labor_entries WHERE id = ?`, ap.e.EntityID).Scan(&userID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return invalid("labour entry %s not found", ap.e.EntityID)
	}
	if err != nil {
		return err
	}
	if userID != ap.actor.id && !ap.actor.atLeast(RoleMidTier) {
		return store.Reject(FlagNotAuthorized, "only the person who logged time, or a mid-tier user, can reverse it")
	}
	if status != "logged" || blank(p.Reason) {
		return invalid("reversing needs a reason, once")
	}
	return ap.exec(`UPDATE labor_entries SET status = 'reversed', reverse_reason = ?, last_event_id = ? WHERE id = ?`,
		p.Reason, ap.e.EventID, ap.e.EntityID)
}

// StepResult is a checklist step with its latest result.
type StepResult struct {
	Step
	Value, Note, RecordedBy string
	Measured                string // for a pass/fail step, the value measured
	Pass                    *bool
	RecordedAt              time.Time
	Recorded                bool
	// Applies is false for a step that depends on an earlier answer it
	// didn't get; such a step is skipped.
	Applies bool
	// Photos added to the step.
	Photos []Attachment
}

// Display is the answer as people read it (a multi step's options joined).
func (r StepResult) Display() string {
	if r.Kind == StepMulti {
		if chosen, err := MultiValues(r.Value); err == nil {
			if len(chosen) == 0 {
				return "none"
			}
			return strings.Join(chosen, ", ")
		}
	}
	return r.Value
}

// Chosen reports whether a multi step's answer includes option o.
func (r StepResult) Chosen(o string) bool {
	chosen, _ := MultiValues(r.Value)
	return slices.Contains(chosen, o)
}

// Outcome is "pass", "fail", or "" when the step has no pass/fail or
// isn't recorded.
func (r StepResult) Outcome() string {
	switch {
	case r.Pass == nil:
		return ""
	case *r.Pass:
		return "pass"
	}
	return "fail"
}

// WorkOrderChecklist is one checklist on a work order, with its answers.
type WorkOrderChecklist struct {
	Procedure
	Steps    []StepResult
	First    bool   // the work order's first checklist
	Answered int    // steps with an answer
	Total    int    // steps that take an answer and apply
	AddedBy  string // who added it, for checklists added later
}

// Checklists returns the checklists on a work order with their answers:
// its first, then those added, in the order added.
func Checklists(ctx context.Context, q Querier, w WorkOrder) ([]WorkOrderChecklist, error) {
	ids, err := checklistIDs(ctx, q, w)
	if err != nil {
		return nil, err
	}
	var out []WorkOrderChecklist
	for _, id := range ids {
		proc, err := GetProcedure(ctx, q, id)
		if err != nil {
			return nil, err
		}
		c := WorkOrderChecklist{Procedure: proc, First: id == w.ProcedureID, Steps: make([]StepResult, len(proc.Steps))}
		q.QueryRowContext(ctx, `SELECT coalesce((SELECT legal_name FROM users WHERE id = added_by), added_by) FROM work_order_checklists
			WHERE wo_id = ? AND procedure_id = ?`, w.ID, id).Scan(&c.AddedBy)
		answers := map[string]string{}
		for i, st := range proc.Steps {
			c.Steps[i].Step = st
			var pass sql.NullBool
			var at string
			err := q.QueryRowContext(ctx, `SELECT value, pass, note, coalesce((SELECT legal_name FROM users WHERE id = recorded_by), recorded_by),
				recorded_at, measured FROM checklist_results WHERE wo_id = ? AND procedure_id = ? AND step_id = ?`, w.ID, id, st.ID).
				Scan(&c.Steps[i].Value, &pass, &c.Steps[i].Note, &c.Steps[i].RecordedBy, &at, &c.Steps[i].Measured)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return nil, err
			}
			c.Steps[i].Recorded = true
			c.Answered++
			answers[st.ID] = c.Steps[i].Value
			if pass.Valid {
				b := pass.Bool
				c.Steps[i].Pass = &b
			}
			c.Steps[i].RecordedAt, _ = time.Parse(time.RFC3339, at)
		}
		for i := range c.Steps {
			c.Steps[i].Applies = Inputless(c.Steps[i].Kind) || StepApplies(c.Steps[i].Step, answers)
			if c.Steps[i].Applies && !Inputless(c.Steps[i].Kind) {
				c.Total++
			}
		}
		photos, err := ListAttachments(ctx, q, EntityWorkOrder, w.ID)
		if err != nil {
			return nil, err
		}
		for _, ph := range photos {
			if ph.ProcedureID != id || ph.Status != "attached" {
				continue
			}
			for i := range c.Steps {
				if c.Steps[i].ID == ph.StepID {
					c.Steps[i].Photos = append(c.Steps[i].Photos, ph)
				}
			}
		}
		out = append(out, c)
	}
	return out, nil
}

// Checklist returns a work order's first checklist with its answers, or
// nil.
func Checklist(ctx context.Context, q Querier, w WorkOrder) (*Procedure, []StepResult, error) {
	all, err := Checklists(ctx, q, w)
	if err != nil || len(all) == 0 || !all[0].First {
		return nil, nil, err
	}
	return &all[0].Procedure, all[0].Steps, nil
}

// ChecklistMatch is an active checklist with how closely it fits some
// equipment: "model", "manufacturer", "any" (for any equipment) or ""
// (for other equipment).
type ChecklistMatch struct {
	ID, Name, Category, Reference string
	Version                       int
	AppliesTo                     []EquipmentMatch
	Fit                           string
}

// ChecklistsFor lists the newest active version of every checklist,
// those for this manufacturer and model first, then the manufacturer's,
// then those for any equipment, then the rest.
func ChecklistsFor(ctx context.Context, q Querier, manufacturer, model string) ([]ChecklistMatch, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, name, category, reference, version, applies_to FROM procedures p
		WHERE status = 'active' AND version = (SELECT max(version) FROM procedures x WHERE x.name = p.name AND x.status = 'active')
		ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChecklistMatch
	for rows.Next() {
		var c ChecklistMatch
		var applies string
		if err := rows.Scan(&c.ID, &c.Name, &c.Category, &c.Reference, &c.Version, &applies); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(applies), &c.AppliesTo)
		if len(c.AppliesTo) == 0 {
			c.Fit = "any"
		}
		for _, m := range c.AppliesTo {
			if m.Matches(manufacturer, model) {
				if m.Model != "" {
					c.Fit = "model"
				} else if c.Fit == "" {
					c.Fit = "manufacturer"
				}
			}
		}
		out = append(out, c)
	}
	rank := map[string]int{"model": 0, "manufacturer": 1, "any": 2, "": 3}
	slices.SortStableFunc(out, func(a, b ChecklistMatch) int { return rank[a.Fit] - rank[b.Fit] })
	return out, rows.Err()
}

// LaborEntry is projected labour time.
type LaborEntry struct {
	ID, User, Date, Note, Status, ReverseReason, UserID string
	Minutes                                             int
}

// Labor returns a work order's labour entries and the total of those not
// reversed.
func Labor(ctx context.Context, q Querier, woID string) ([]LaborEntry, int, error) {
	rows, err := q.QueryContext(ctx, `SELECT l.id, coalesce(u.legal_name, l.user_id), l.user_id, l.work_date, l.note, l.status,
		l.reverse_reason, l.minutes FROM labor_entries l LEFT JOIN users u ON u.id = l.user_id WHERE l.wo_id = ? ORDER BY l.work_date, l.id`, woID)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []LaborEntry
	total := 0
	for rows.Next() {
		var e LaborEntry
		if err := rows.Scan(&e.ID, &e.User, &e.UserID, &e.Date, &e.Note, &e.Status, &e.ReverseReason, &e.Minutes); err != nil {
			return nil, 0, err
		}
		if e.Status == "logged" {
			total += e.Minutes
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}
