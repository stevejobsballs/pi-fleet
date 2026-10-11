package web

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"pi-fleet/internal/domain"
)

type procedureRow struct {
	ID, Name, Status, PublishedAt, Category, Reference string
	Version, Steps                                     int
}

func listProcedures(r *http.Request, s *Server, activeOnly bool) ([]procedureRow, error) {
	rows, err := s.App.Store.DB().QueryContext(r.Context(), `SELECT id, name, status, published_at, category, reference, version,
			(SELECT count(*) FROM json_each(steps) WHERE json_extract(value, '$.kind') NOT IN ('heading', 'info'))
		FROM procedures WHERE ? = 0 OR status = 'active' ORDER BY name, version DESC`, activeOnly)
	return scanAll(rows, err, func(r *sql.Rows) (procedureRow, error) {
		var p procedureRow
		return p, r.Scan(&p.ID, &p.Name, &p.Status, &p.PublishedAt, &p.Category, &p.Reference, &p.Version, &p.Steps)
	})
}

func procedureOptions(r *http.Request, s *Server) ([]option, error) {
	rows, err := listProcedures(r, s, true)
	out := make([]option, len(rows))
	for i, p := range rows {
		out[i] = option{ID: p.ID, Label: fmt.Sprintf("%s v%d (%d steps)", p.Name, p.Version, p.Steps)}
	}
	return out, err
}

func (s *Server) procedureList(w http.ResponseWriter, r *http.Request, sess *session) error {
	rows, err := listProcedures(r, s, false)
	if err != nil {
		return err
	}
	return s.render(w, r, sess, "procedures", "Checklists", rows)
}

func (s *Server) procedureView(w http.ResponseWriter, r *http.Request, sess *session) error {
	p, err := domain.GetProcedure(r.Context(), s.App.Store.DB(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return s.render(w, r, sess, "procedure", fmt.Sprintf("%s v%d", p.Name, p.Version), p)
}

// --- the checklist builder ---

// formStep is one step as the builder shows it: everything as typed.
type formStep struct {
	Num                                                      int // 1-based, as shown
	ID, Text, Kind, Help, Unit, Lower, Upper, Options, Fails string
	Required, Critical, NoteOnFail                           bool
	ShowIfStep, ShowIfValue                                  string // a step number, and its answer
}

type builderData struct {
	Name, Description, Reference, Category string
	Steps                                  []formStep
	Kinds, Categories                      []string
	From                                   string // the version this starts from, if any
	Error                                  string
}

// kindLabels are what each kind of step is called in the builder.
var kindLabels = map[string]string{
	domain.StepCheck: "Pass / fail", domain.StepNumber: "Reading (a number)", domain.StepChoice: "Choose one",
	domain.StepMulti: "Tick all that apply", domain.StepText: "Short answer", domain.StepLongText: "Notes (several lines)",
	domain.StepDate: "Date", domain.StepHeading: "Section heading", domain.StepInfo: "Instruction or warning (no answer)",
}

// stepsToForm turns a published checklist's steps into builder rows.
func stepsToForm(steps []domain.Step) []formStep {
	num := map[string]int{}
	out := make([]formStep, len(steps))
	for i, st := range steps {
		num[st.ID] = i + 1
		f := formStep{Num: i + 1, ID: st.ID, Text: st.Text, Kind: st.Kind, Help: st.Help, Unit: st.Unit, Lower: st.Lower, Upper: st.Upper,
			Required: st.Required, Critical: st.Critical, NoteOnFail: st.NoteOnFail}
		var opts, fails []string
		for _, o := range st.Options {
			opts = append(opts, o.Label)
			if o.Fails {
				fails = append(fails, o.Label)
			}
		}
		f.Options, f.Fails = strings.Join(opts, "\n"), strings.Join(fails, "\n")
		if c := st.ShowIf; c != nil {
			f.ShowIfStep, f.ShowIfValue = strconv.Itoa(num[c.Step]), c.Equals
		}
		out[i] = f
	}
	return out
}

// withBlankSteps adds empty rows to fill in, up to at least min rows.
func withBlankSteps(steps []formStep, extra, min int) []formStep {
	for len(steps) < min || extra > 0 {
		steps = append(steps, formStep{Num: len(steps) + 1, Kind: domain.StepCheck})
		extra--
	}
	return steps
}

func (s *Server) renderBuilder(w http.ResponseWriter, r *http.Request, sess *session, d builderData) error {
	d.Kinds, d.Categories = domain.StepKinds, domain.CategoryOptions
	title := "New checklist"
	if d.Name != "" {
		title = "Checklist: " + d.Name
	}
	return s.render(w, r, sess, "procedure_new", title, d)
}

// procedureNew is the builder: empty, or (?from=) starting from a version,
// to publish its next version or a new checklist based on it.
func (s *Server) procedureNew(w http.ResponseWriter, r *http.Request, sess *session) error {
	more, _ := strconv.Atoi(r.URL.Query().Get("more"))
	d := builderData{}
	if from := r.URL.Query().Get("from"); from != "" {
		p, err := domain.GetProcedure(r.Context(), s.App.Store.DB(), from)
		if err != nil {
			return err
		}
		d = builderData{Name: p.Name, Description: p.Description, Reference: p.Reference, Category: p.Category, Steps: stepsToForm(p.Steps), From: from}
	}
	d.Steps = withBlankSteps(d.Steps, min(max(more, 3), 50), 8)
	return s.renderBuilder(w, r, sess, d)
}

// readBuilder reads the builder form back into rows (as typed) and steps.
func readBuilder(f url.Values) (builderData, []domain.Step, error) {
	get := func(k string, i int) string { return strings.TrimSpace(f.Get(fmt.Sprintf("%s_%d", k, i))) }
	yes := func(k string, i int) bool { return get(k, i) == "yes" }
	lines := func(v string) []string {
		var out []string
		for _, l := range strings.Split(v, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				out = append(out, l)
			}
		}
		return out
	}
	d := builderData{Name: strings.TrimSpace(f.Get("name")), Description: strings.TrimSpace(f.Get("description")),
		Reference: strings.TrimSpace(f.Get("reference")), Category: strings.TrimSpace(f.Get("category")), From: f.Get("from")}
	count, _ := strconv.Atoi(f.Get("count"))
	count = min(max(count, 0), 400)
	for i := 0; i < count; i++ {
		d.Steps = append(d.Steps, formStep{Num: i + 1, ID: get("id", i), Text: get("text", i), Kind: get("kind", i), Help: get("help", i),
			Unit: get("unit", i), Lower: get("lower", i), Upper: get("upper", i), Options: get("options", i), Fails: get("fails", i),
			Required: yes("required", i), Critical: yes("critical", i), NoteOnFail: yes("notefail", i),
			ShowIfStep: get("showif_step", i), ShowIfValue: get("showif_value", i)})
	}
	// Steps: rows with text, ids kept from the version started from where
	// they are still unique, the others numbered.
	ids := map[int]string{}
	used := map[string]bool{}
	var steps []domain.Step
	for _, fs := range d.Steps {
		if fs.Text == "" {
			continue
		}
		id := fs.ID
		if id == "" || used[id] {
			for n := len(steps) + 1; ; n++ {
				if id = fmt.Sprintf("s%d", n); !used[id] {
					break
				}
			}
		}
		used[id], ids[fs.Num] = true, id
		st := domain.Step{ID: id, Text: fs.Text, Kind: fs.Kind, Help: fs.Help, Required: fs.Required, Critical: fs.Critical, NoteOnFail: fs.NoteOnFail}
		if st.Kind == domain.StepNumber {
			st.Unit, st.Lower, st.Upper = fs.Unit, fs.Lower, fs.Upper
		}
		if st.Kind == domain.StepChoice || st.Kind == domain.StepMulti {
			fails := map[string]bool{}
			for _, l := range lines(fs.Fails) {
				fails[l] = true
			}
			for _, l := range lines(fs.Options) {
				st.Options = append(st.Options, domain.StepOption{Label: l, Fails: fails[l]})
				delete(fails, l)
			}
			for l := range fails {
				return d, nil, userErr("step %d: %q counts as a fail but isn't one of its options", fs.Num, l)
			}
		}
		if fs.ShowIfStep != "" {
			n, err := strconv.Atoi(fs.ShowIfStep)
			if err != nil || ids[n] == "" {
				return d, nil, userErr("step %d: “only when step %s” must name an earlier step with text", fs.Num, fs.ShowIfStep)
			}
			st.ShowIf = &domain.StepCondition{Step: ids[n], Equals: fs.ShowIfValue}
		}
		steps = append(steps, st)
	}
	return d, steps, nil
}

// procedurePublish publishes the builder's checklist. If it can't, the
// builder comes back as typed, with what to fix.
func (s *Server) procedurePublish(w http.ResponseWriter, r *http.Request, sess *session) error {
	d, steps, err := readBuilder(r.PostForm)
	if err == nil && r.PostFormValue("more") != "" {
		d.Steps = withBlankSteps(d.Steps, 5, 0) // "Add 5 more steps"
		return s.renderBuilder(w, r, sess, d)
	}
	var id string
	var version int
	if err == nil {
		id, version, err = s.App.PublishChecklist(r.Context(), s.actor(sess), domain.ProcedurePublished{Name: d.Name, Description: d.Description,
			Reference: d.Reference, Category: d.Category, Steps: steps})
	}
	if err != nil {
		msg := message(err)
		if msg == "" {
			return err
		}
		d.Error = "Not published: " + msg
		w.WriteHeader(http.StatusUnprocessableEntity)
		return s.renderBuilder(w, r, sess, d)
	}
	return s.done(w, r, sess, "/procedures/"+id, fmt.Sprintf("Published %s version %d.", d.Name, version))
}

// checklistFile is a checklist as downloaded, to copy to another pi-fleet.
type checklistFile struct {
	Format      string        `json:"format"` // "pi-fleet checklist"
	Name        string        `json:"name"`
	Description string        `json:"description,omitempty"`
	Reference   string        `json:"reference,omitempty"`
	Category    string        `json:"category,omitempty"`
	Steps       []domain.Step `json:"steps"`
}

const checklistFormat = "pi-fleet checklist"

func (s *Server) procedureExport(w http.ResponseWriter, r *http.Request, sess *session) error {
	p, err := domain.GetProcedure(r.Context(), s.App.Store.DB(), r.PathValue("id"))
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(checklistFile{Format: checklistFormat, Name: p.Name, Description: p.Description, Reference: p.Reference,
		Category: p.Category, Steps: p.Steps}, "", "  ")
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(fmt.Sprintf("%s v%d.checklist.json", p.Name, p.Version)))
	_, err = w.Write(b)
	return err
}

// procedureImport opens a downloaded checklist in the builder, to check
// and publish.
func (s *Server) procedureImport(w http.ResponseWriter, r *http.Request, sess *session) error {
	var c checklistFile
	data := r.PostFormValue("checklist")
	if f, _, err := r.FormFile("file"); err == nil {
		b, _ := io.ReadAll(io.LimitReader(f, 1<<20))
		f.Close()
		data = string(b)
	}
	if err := json.Unmarshal([]byte(data), &c); err != nil || c.Format != checklistFormat {
		return s.done(w, r, sess, "/procedures", "Not opened: that isn't a checklist downloaded from pi-fleet.")
	}
	d := builderData{Name: c.Name, Description: c.Description, Reference: c.Reference, Category: c.Category, Steps: stepsToForm(c.Steps)}
	d.Steps = withBlankSteps(d.Steps, 3, 0)
	return s.renderBuilder(w, r, sess, d)
}

func (s *Server) procedureRetire(w http.ResponseWriter, r *http.Request, sess *session) error {
	id := r.PathValue("id")
	if err := s.App.RetireProcedure(r.Context(), s.actor(sess), id, strings.TrimSpace(r.PostFormValue("reason"))); err != nil {
		return s.failed(w, r, sess, "/procedures/"+id, err)
	}
	return s.done(w, r, sess, "/procedures/"+id, "Retired. Work already using it keeps it.")
}

func (s *Server) workOrderSetProcedure(w http.ResponseWriter, r *http.Request, sess *session) error {
	id := r.PathValue("id")
	if err := s.App.SetWorkOrderProcedure(r.Context(), s.actor(sess), id, r.PostFormValue("procedure")); err != nil {
		return s.failed(w, r, sess, "/work-orders/"+id, err)
	}
	return s.done(w, r, sess, "/work-orders/"+id, "Checklist set.")
}

func (s *Server) workOrderStep(w http.ResponseWriter, r *http.Request, sess *session) error {
	id := r.PathValue("id")
	back := "/work-orders/" + id
	note := strings.TrimSpace(r.PostFormValue("note"))
	value := strings.TrimSpace(r.PostFormValue("value"))
	if err := s.phiCheck(r, note, value); err != nil {
		return s.failed(w, r, sess, back, err)
	}
	if err := s.App.RecordStep(r.Context(), s.actor(sess), id, r.PostFormValue("step"), value, note); err != nil {
		return s.failed(w, r, sess, back, err)
	}
	return s.done(w, r, sess, back+"#checklist", "Step recorded.")
}

// workOrderChecklist saves the checklist form: every answer that is new
// or changed, in order (so a step that depends on an earlier one sees its
// new answer). Steps that can't be saved are listed; the others are kept.
func (s *Server) workOrderChecklist(w http.ResponseWriter, r *http.Request, sess *session) error {
	ctx := r.Context()
	id := r.PathValue("id")
	back := "/work-orders/" + id
	wo, err := domain.GetWorkOrder(ctx, s.App.Store.DB(), id)
	if err != nil {
		return err
	}
	_, steps, err := domain.Checklist(ctx, s.App.Store.DB(), wo)
	if err != nil {
		return err
	}
	saved := 0
	var problems []string
	for _, st := range steps {
		if domain.Inputless(st.Kind) {
			continue
		}
		var value string
		if st.Kind == domain.StepMulti {
			chosen := r.PostForm["v_"+st.ID]
			if len(chosen) == 0 && !st.Recorded {
				continue
			}
			if chosen == nil {
				chosen = []string{}
			}
			b, _ := json.Marshal(chosen)
			value = string(b)
		} else {
			value = strings.TrimSpace(r.PostFormValue("v_" + st.ID))
		}
		note := strings.TrimSpace(r.PostFormValue("n_" + st.ID))
		if value == "" || (st.Recorded && value == st.Value && note == st.Note) {
			continue
		}
		err := s.phiCheck(r, value, note)
		if err == nil {
			err = s.App.RecordStep(ctx, s.actor(sess), id, st.ID, value, note)
		}
		if err != nil {
			msg := message(err)
			if msg == "" {
				return err
			}
			problems = append(problems, msg)
			continue
		}
		saved++
	}
	msg := fmt.Sprintf("Saved %d answer%s.", saved, map[bool]string{true: "", false: "s"}[saved == 1])
	if saved == 0 && len(problems) == 0 {
		msg = "Nothing new to save."
	}
	if len(problems) > 0 {
		msg += " Not saved: " + strings.Join(problems, "; ") + "."
	}
	return s.done(w, r, sess, back+"#checklist", msg)
}

func (s *Server) laborLog(w http.ResponseWriter, r *http.Request, sess *session) error {
	id := r.PathValue("id")
	back := "/work-orders/" + id
	minutes, err := parseDuration(r.PostFormValue("time"))
	if err != nil {
		return s.done(w, r, sess, back, "Not saved: "+err.Error())
	}
	note := strings.TrimSpace(r.PostFormValue("note"))
	if err := s.phiCheck(r, note); err != nil {
		return s.failed(w, r, sess, back, err)
	}
	if _, err := s.App.LogLabor(r.Context(), s.actor(sess), id, minutes, r.PostFormValue("date"), note); err != nil {
		return s.failed(w, r, sess, back, err)
	}
	return s.done(w, r, sess, back+"#labour", "Time logged.")
}

// parseDuration accepts minutes ("45"), hours and minutes ("1:30") or
// decimal hours ("1.5h").
func parseDuration(s string) (int, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	bad := fmt.Errorf("enter time as minutes (45), h:mm (1:30) or hours (1.5h)")
	if h, m, ok := strings.Cut(s, ":"); ok {
		hh, err1 := strconv.Atoi(h)
		mm, err2 := strconv.Atoi(m)
		if err1 != nil || err2 != nil || mm >= 60 || hh < 0 || mm < 0 {
			return 0, bad
		}
		return hh*60 + mm, nil
	}
	if h, ok := strings.CutSuffix(s, "h"); ok {
		f, err := strconv.ParseFloat(strings.TrimSpace(h), 64)
		if err != nil || f <= 0 {
			return 0, bad
		}
		return int(f*60 + 0.5), nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, bad
	}
	return n, nil
}

func (s *Server) laborReverse(w http.ResponseWriter, r *http.Request, sess *session) error {
	var woID string
	if err := s.App.Store.DB().QueryRowContext(r.Context(), `SELECT wo_id FROM labor_entries WHERE id = ?`, r.PathValue("id")).Scan(&woID); err != nil {
		return domain.ErrNotFound
	}
	back := "/work-orders/" + woID
	if err := s.App.ReverseLabor(r.Context(), s.actor(sess), r.PathValue("id"), strings.TrimSpace(r.PostFormValue("reason"))); err != nil {
		return s.failed(w, r, sess, back, err)
	}
	return s.done(w, r, sess, back+"#labour", "Entry reversed.")
}

func hhmm(minutes int) string { return fmt.Sprintf("%d:%02d", minutes/60, minutes%60) }

func today(now time.Time) string { return now.Format("2006-01-02") }
