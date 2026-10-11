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
	TypeProcedurePublished    = "procedure.published"
	TypeProcedureRetired      = "procedure.retired"
	TypeWorkOrderProcedureSet = "workorder.procedure_set"
	TypeWorkOrderStepRecorded = "workorder.step_recorded"
	TypeLaborLogged           = "labor.logged"
	TypeLaborReversed         = "labor.reversed"
	EntityProcedure           = "procedure_version"
	EntityLabor               = "labor_entry"
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
	StepHeading  = "heading"  // starts a section
	StepInfo     = "info"     // an instruction or warning to read
)

// StepKinds are the kinds of step, in the order offered.
var StepKinds = []string{StepCheck, StepNumber, StepChoice, StepMulti, StepText, StepLongText, StepDate, StepHeading, StepInfo}

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
	Unit  string `json:"unit,omitempty"`  // number
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
	Steps       []Step `json:"steps"`
}

type ProcedureRetired struct {
	Reason string `json:"reason"`
}

// WorkOrderProcedureSet chooses the checklist for a work order before
// work starts.
type WorkOrderProcedureSet struct {
	ProcedureID string `json:"procedure_id"`
}

// WorkOrderStepRecorded records (or re-records) one checklist step.
type WorkOrderStepRecorded struct {
	StepID string `json:"step_id"`
	Value  string `json:"value"`
	Note   string `json:"note,omitempty"`
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
		if st.Kind != StepNumber && (st.Unit != "" || st.Lower != "" || st.Upper != "") {
			return invalid("step %d: only reading steps have units and limits", n)
		}
		if st.Kind != StepChoice && st.Kind != StepMulti && len(st.Options) > 0 {
			return invalid("step %d: only choice steps have options", n)
		}
		switch st.Kind {
		case StepCheck, StepText, StepLongText, StepDate:
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
	if blank(p.Name) || len(p.Name) > 120 {
		return invalid("a checklist needs a name of at most 120 characters")
	}
	if len(p.Description) > 4000 || len(p.Reference) > 300 || len(p.Category) > 80 {
		return invalid("the description, reference or category is too long")
	}
	if err := validateSteps(p.Steps); err != nil {
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
	return ap.exec(`INSERT INTO procedures (id, name, version, steps, status, published_by, published_at, last_event_id, description, reference, category)
		VALUES (?, ?, ?, ?, 'active', ?, ?, ?, ?, ?, ?)`, ap.e.EntityID, p.Name, p.Version, string(steps), ap.actor.id, ap.wall(), ap.e.EventID,
		p.Description, p.Reference, p.Category)
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
	Steps                            []Step
	PublishedAt                      time.Time
}

// GetProcedure returns a checklist version.
func GetProcedure(ctx context.Context, q Querier, id string) (Procedure, error) {
	var p Procedure
	var steps, at string
	err := q.QueryRowContext(ctx, `SELECT id, name, version, steps, status, published_at, description, reference, category FROM procedures WHERE id = ?`, id).
		Scan(&p.ID, &p.Name, &p.Version, &steps, &p.Status, &at, &p.Description, &p.Reference, &p.Category)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	if err != nil {
		return p, err
	}
	p.PublishedAt, _ = time.Parse(time.RFC3339, at)
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
	if w.ProcedureID == "" {
		return invalid("%s has no checklist", w.Number)
	}
	proc, err := GetProcedure(ap.ctx, ap.tx, w.ProcedureID)
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
	answers, err := ap.stepAnswers(w.ID)
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
	if pass != nil && !*pass && step.NoteOnFail && blank(p.Note) {
		return invalid("%q failed: say why in the note", step.Text)
	}
	if err := ap.exec(`INSERT INTO checklist_results (wo_id, step_id, value, pass, note, recorded_by, recorded_at, event_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (wo_id, step_id) DO UPDATE SET value = excluded.value, pass = excluded.pass,
		note = excluded.note, recorded_by = excluded.recorded_by, recorded_at = excluded.recorded_at, event_id = excluded.event_id`,
		w.ID, step.ID, p.Value, pass, p.Note, ap.actor.id, ap.wall(), ap.e.EventID); err != nil {
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

// stepAnswers are a work order's recorded answers, by step.
func (ap *applier) stepAnswers(woID string) (map[string]string, error) {
	rows, err := ap.tx.QueryContext(ap.ctx, `SELECT step_id, value FROM checklist_results WHERE wo_id = ?`, woID)
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

// checkChecklist requires every required step that applies before
// completion, and that equipment which failed a critical step is out of
// service.
func (ap *applier) checkChecklist(w WorkOrder) error {
	if w.ProcedureID == "" {
		return nil
	}
	proc, err := GetProcedure(ap.ctx, ap.tx, w.ProcedureID)
	if err != nil {
		return err
	}
	answers, err := ap.stepAnswers(w.ID)
	if err != nil {
		return err
	}
	for _, st := range proc.Steps {
		if Inputless(st.Kind) || !StepApplies(st, answers) {
			continue
		}
		if _, done := answers[st.ID]; st.Required && !done {
			return invalid("required checklist step %q is not recorded", st.Text)
		}
		if !st.Critical {
			continue
		}
		var failed bool
		err := ap.tx.QueryRowContext(ap.ctx, `SELECT pass = 0 FROM checklist_results WHERE wo_id = ? AND step_id = ? AND pass IS NOT NULL`, w.ID, st.ID).Scan(&failed)
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
	return nil
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
	Pass                    *bool
	RecordedAt              time.Time
	Recorded                bool
	// Applies is false for a step that depends on an earlier answer it
	// didn't get; such a step is skipped.
	Applies bool
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

// Checklist returns a work order's checklist with results, or nil.
func Checklist(ctx context.Context, q Querier, w WorkOrder) (*Procedure, []StepResult, error) {
	if w.ProcedureID == "" {
		return nil, nil, nil
	}
	proc, err := GetProcedure(ctx, q, w.ProcedureID)
	if err != nil {
		return nil, nil, err
	}
	out := make([]StepResult, len(proc.Steps))
	for i, st := range proc.Steps {
		out[i].Step = st
		var pass sql.NullBool
		var at string
		err := q.QueryRowContext(ctx, `SELECT value, pass, note, coalesce((SELECT legal_name FROM users WHERE id = recorded_by), recorded_by),
			recorded_at FROM checklist_results WHERE wo_id = ? AND step_id = ?`, w.ID, st.ID).Scan(&out[i].Value, &pass, &out[i].Note, &out[i].RecordedBy, &at)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		out[i].Recorded = true
		if pass.Valid {
			b := pass.Bool
			out[i].Pass = &b
		}
		out[i].RecordedAt, _ = time.Parse(time.RFC3339, at)
	}
	answers := map[string]string{}
	for _, r := range out {
		if r.Recorded {
			answers[r.ID] = r.Value
		}
	}
	for i := range out {
		out[i].Applies = Inputless(out[i].Kind) || StepApplies(out[i].Step, answers)
	}
	return &proc, out, nil
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
