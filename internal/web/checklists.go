package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"pi-fleet/internal/blobs"
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

// libraryGroup is a manufacturer's checklists in the library, by model.
type libraryGroup struct {
	Manufacturer string
	Models       []libraryModel
}

type libraryModel struct {
	Model      string // "" for every model
	Checklists []domain.ChecklistMatch
}

type libraryData struct {
	Waiting  []domain.Proposal // for mid-tier and super users: everyone's; for others: their own
	Mine     []domain.Proposal // this person's recently decided ones
	Search   string
	Sections []librarySection // generic templates, then custom checklists
	All      []procedureRow   // every version, when asked for
	ShowAll  bool
}

// librarySection is the generic templates or the custom checklists:
// those for any equipment, then by manufacturer and model.
type librarySection struct {
	Title, Hint string
	Custom      bool
	Any         []domain.ChecklistMatch
	Makers      []libraryGroup
}

// procedureList is the checklist library: the newest active version of
// each checklist, generic templates first, then custom checklists, each
// grouped by the equipment they are for. ?all=1 lists every version, old
// and retired too.
func (s *Server) procedureList(w http.ResponseWriter, r *http.Request, sess *session) error {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	d := libraryData{Search: q, ShowAll: r.URL.Query().Get("all") != ""}
	if d.ShowAll {
		rows, err := listProcedures(r, s, false)
		if err != nil {
			return err
		}
		d.All = rows
		return s.render(w, r, sess, "procedures", "Checklists", d)
	}
	all, err := domain.ChecklistsFor(r.Context(), s.App.Store.DB(), "", "")
	if err != nil {
		return err
	}
	who := sess.User.ID
	if roleRank[sess.User.Role] >= roleRank[domain.RoleMidTier] {
		who = ""
	}
	if d.Waiting, err = domain.ListProposals(r.Context(), s.App.Store.DB(), domain.ProposalPending, who); err != nil {
		return err
	}
	for _, st := range []string{domain.ProposalRejected, domain.ProposalApproved} {
		mine, err := domain.ListProposals(r.Context(), s.App.Store.DB(), st, sess.User.ID)
		if err != nil {
			return err
		}
		d.Mine = append(d.Mine, mine...)
	}
	for _, custom := range []bool{false, true} {
		var some []domain.ChecklistMatch
		for _, c := range all {
			if c.Custom == custom {
				some = append(some, c)
			}
		}
		sec := librarySection{Title: "Generic templates", Custom: custom,
			Hint: "Checklists to start from. Use one as it is, configure a custom checklist from it, or suggest an amendment."}
		if custom {
			sec.Title, sec.Hint = "Custom checklists", "Checklists configured for particular equipment or ways of working, often from a generic template."
		}
		sec.Any, sec.Makers = groupLibrary(some, q)
		d.Sections = append(d.Sections, sec)
	}
	return s.render(w, r, sess, "procedures", "Checklists", d)
}

// groupLibrary groups checklists matching search: those for any
// equipment, then by manufacturer and model.
func groupLibrary(all []domain.ChecklistMatch, q string) ([]domain.ChecklistMatch, []libraryGroup) {
	lq := strings.ToLower(q)
	hit := func(parts ...string) bool {
		if lq == "" {
			return true
		}
		for _, p := range parts {
			if strings.Contains(strings.ToLower(p), lq) {
				return true
			}
		}
		return false
	}
	var anyEquipment []domain.ChecklistMatch
	makers := map[string]*libraryGroup{}
	var order []string
	for _, c := range all {
		if len(c.AppliesTo) == 0 {
			if hit(c.Name, c.Category, c.Reference) {
				anyEquipment = append(anyEquipment, c)
			}
			continue
		}
		for _, m := range c.AppliesTo {
			if !hit(c.Name, c.Category, c.Reference, m.Manufacturer, m.Model) {
				continue
			}
			key := strings.ToLower(strings.TrimSpace(m.Manufacturer))
			g := makers[key]
			if g == nil {
				g = &libraryGroup{Manufacturer: strings.TrimSpace(m.Manufacturer)}
				makers[key] = g
				order = append(order, key)
			}
			placed := false
			for i := range g.Models {
				if strings.EqualFold(g.Models[i].Model, strings.TrimSpace(m.Model)) {
					g.Models[i].Checklists = append(g.Models[i].Checklists, c)
					placed = true
				}
			}
			if !placed {
				g.Models = append(g.Models, libraryModel{Model: strings.TrimSpace(m.Model), Checklists: []domain.ChecklistMatch{c}})
			}
		}
	}
	slices.Sort(order)
	var groups []libraryGroup
	for _, k := range order {
		g := *makers[k]
		slices.SortFunc(g.Models, func(a, b libraryModel) int {
			return strings.Compare(strings.ToLower(a.Model), strings.ToLower(b.Model))
		})
		groups = append(groups, g)
	}
	return anyEquipment, groups
}

// procedureData is a published checklist version, on its page, with the
// equipment it can be filled in on (that it is for first).
type procedureData struct {
	domain.Procedure
	Equipment    []choiceGroup
	TemplateName string // for a custom checklist, the template it came from
}

func (procedureData) BackLink() (string, string) { return "/procedures", "checklists" }

func (s *Server) procedureView(w http.ResponseWriter, r *http.Request, sess *session) error {
	p, err := domain.GetProcedure(r.Context(), s.App.Store.DB(), r.PathValue("id"))
	if err != nil {
		return err
	}
	d := procedureData{Procedure: p}
	if p.Template != "" {
		if t, err := domain.GetProcedure(r.Context(), s.App.Store.DB(), p.Template); err == nil {
			d.TemplateName = t.Name
		}
	}
	if p.Status == "active" {
		if d.Equipment, err = fillChoices(r.Context(), s.App.Store.DB(), p); err != nil {
			return err
		}
	}
	return s.render(w, r, sess, "procedure", fmt.Sprintf("%s v%d", p.Name, p.Version), d)
}

// fillChoices groups the equipment a checklist can be filled in on.
func fillChoices(ctx context.Context, q domain.Querier, p domain.Procedure) ([]choiceGroup, error) {
	rows, err := q.QueryContext(ctx, `SELECT a.id, a.tag || ' · ' || a.manufacturer || ' ' || a.model || ' · ' || s.path || ' › ' || l.name, a.manufacturer, a.model
		FROM assets a JOIN sites s ON s.id = a.site_id JOIN locations l ON l.id = a.location_id
		WHERE a.merged_into = '' AND a.status != 'retired' ORDER BY a.tag LIMIT 2000`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := []choiceGroup{{Label: "Equipment this checklist is for"}, {Label: "Other equipment"}}
	for rows.Next() {
		var o option
		var mfr, model string
		if err := rows.Scan(&o.ID, &o.Label, &mfr, &model); err != nil {
			return nil, err
		}
		g := 1
		for _, m := range p.AppliesTo {
			if m.Matches(mfr, model) {
				g = 0
			}
		}
		groups[g].Items = append(groups[g].Items, o)
	}
	var out []choiceGroup
	for _, g := range groups {
		if len(g.Items) > 0 {
			out = append(out, g)
		}
	}
	return out, rows.Err()
}

// procedureFill starts filling in a checklist on a piece of equipment.
func (s *Server) procedureFill(w http.ResponseWriter, r *http.Request, sess *session) error {
	procID := r.PathValue("id")
	wo, err := s.App.StartChecklist(r.Context(), s.actor(sess), r.PostFormValue("asset"), procID)
	if err != nil {
		return s.failed(w, r, sess, "/procedures/"+procID, err)
	}
	return s.done(w, r, sess, "/work-orders/"+wo+"/checklists/"+procID, "Started: a work order was opened for this checklist and taken by you. Fill it in below.")
}

// proposalData is a checklist waiting for approval, on its page.
type proposalData struct {
	domain.Proposal
	CanReview bool
}

func (proposalData) BackLink() (string, string) { return "/procedures", "checklists" }

func (s *Server) proposalView(w http.ResponseWriter, r *http.Request, sess *session) error {
	p, err := domain.GetProposal(r.Context(), s.App.Store.DB(), r.PathValue("id"))
	if err != nil {
		return err
	}
	isMid := roleRank[sess.User.Role] >= roleRank[domain.RoleMidTier]
	if !isMid && p.ProposedByID != sess.User.ID {
		return domain.ErrNotFound
	}
	return s.render(w, r, sess, "proposal", p.Name, proposalData{Proposal: p, CanReview: isMid && p.Status == domain.ProposalPending})
}

func (s *Server) proposalReview(w http.ResponseWriter, r *http.Request, sess *session) error {
	id := r.PathValue("id")
	approve := r.PostFormValue("decision") == "approve"
	pub, err := s.App.ReviewChecklist(r.Context(), s.actor(sess), id, approve, strings.TrimSpace(r.PostFormValue("note")))
	if err != nil {
		return s.failed(w, r, sess, "/checklist-proposals/"+id, err)
	}
	if approve {
		return s.done(w, r, sess, "/procedures/"+pub, "Approved and published.")
	}
	return s.done(w, r, sess, "/checklist-proposals/"+id, "Rejected. The person who made it sees the reason.")
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
	AppliesTo                              []domain.EquipmentMatch // rows, some empty
	Manufacturers, Models                  []string                // suggestions, from the equipment registered
	Steps                                  []formStep
	Kinds, Categories                      []string
	From                                   string // the version this starts from, if any
	Error, Notice                          string
	CanPublish                             bool // mid-tier and super users; others send it for approval
	// Custom: a custom checklist (configured from Template, if set);
	// otherwise a generic template. Mode: "new", "amend" (the next version
	// of From) or "custom" (configured from the template From).
	Custom       bool
	Template     string
	TemplateName string
	Mode         string
}

func (builderData) BackLink() (string, string) { return "/procedures", "checklists" }

// kindLabels are what each kind of step is called in the builder.
var kindLabels = map[string]string{
	domain.StepCheck: "Pass / fail", domain.StepNumber: "Reading (a number)", domain.StepChoice: "Choose one",
	domain.StepMulti: "Tick all that apply", domain.StepText: "Short answer", domain.StepLongText: "Notes (several lines)",
	domain.StepDate: "Date", domain.StepPhoto: "Photo", domain.StepHeading: "Section heading", domain.StepInfo: "Instruction or warning (no answer)",
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

// insertStep adds an empty step after step n (0: before the first).
func insertStep(steps []formStep, n int) []formStep {
	n = min(max(n, 0), len(steps))
	out := slices.Insert(slices.Clone(steps), n, formStep{Num: 0, Kind: domain.StepCheck})
	return renumberSteps(out, func(old int) int {
		if old > n {
			return old + 1
		}
		return old
	}, nil)
}

// removeStep takes out step n. A step asked only when step n had a
// certain answer is then always asked; the note says which.
func removeStep(steps []formStep, n int) ([]formStep, string) {
	if n < 1 || n > len(steps) {
		return steps, ""
	}
	out := slices.Delete(slices.Clone(steps), n-1, n)
	var always []string
	out = renumberSteps(out, func(old int) int {
		switch {
		case old == n:
			return 0
		case old > n:
			return old - 1
		}
		return old
	}, func(f formStep) { always = append(always, strconv.Itoa(f.Num)) })
	switch len(always) {
	case 0:
		return out, ""
	case 1:
		return out, fmt.Sprintf("Step %d removed. Step %s was asked only when it had a certain answer, so it is now always asked; change that if needed.", n, always[0])
	}
	return out, fmt.Sprintf("Step %d removed. Steps %s and %s were asked only when it had a certain answer, so they are now always asked; change that if needed.",
		n, strings.Join(always[:len(always)-1], ", "), always[len(always)-1])
}

// renumberSteps numbers the steps 1, 2, … and moves each "only ask when
// step N" to N's new number (0: N is gone, so cleared, and lost is told).
func renumberSteps(steps []formStep, moved func(old int) int, lost func(formStep)) []formStep {
	for i := range steps {
		steps[i].Num = i + 1
	}
	for i, f := range steps {
		old, err := strconv.Atoi(f.ShowIfStep)
		if err != nil {
			continue // empty, or not a number: left as typed
		}
		if n := moved(old); n > 0 {
			steps[i].ShowIfStep = strconv.Itoa(n)
		} else {
			steps[i].ShowIfStep, steps[i].ShowIfValue = "", ""
			if lost != nil {
				lost(steps[i])
			}
		}
	}
	return steps
}

func (s *Server) renderBuilder(w http.ResponseWriter, r *http.Request, sess *session, d builderData) error {
	d.Kinds, d.Categories = domain.StepKinds, domain.CategoryOptions
	d.CanPublish = roleRank[sess.User.Role] >= roleRank[domain.RoleMidTier]
	for len(d.AppliesTo) < 3 || d.AppliesTo[len(d.AppliesTo)-1].Manufacturer != "" {
		d.AppliesTo = append(d.AppliesTo, domain.EquipmentMatch{})
	}
	q := s.App.Store.DB()
	for col, dst := range map[string]*[]string{"manufacturer": &d.Manufacturers, "model": &d.Models} {
		rows, err := q.QueryContext(r.Context(), `SELECT DISTINCT `+col+` FROM assets WHERE `+col+` != '' ORDER BY 1 LIMIT 500`)
		if *dst, err = scanAll(rows, err, func(r *sql.Rows) (string, error) {
			var v string
			return v, r.Scan(&v)
		}); err != nil {
			return err
		}
	}
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
	d := builderData{Mode: "new", Custom: roleRank[sess.User.Role] < roleRank[domain.RoleMidTier]}
	if from := r.URL.Query().Get("from"); from != "" {
		p, err := domain.GetProcedure(r.Context(), s.App.Store.DB(), from)
		if err != nil {
			return err
		}
		d = builderData{Name: p.Name, Description: p.Description, Reference: p.Reference, Category: p.Category, AppliesTo: p.AppliesTo,
			Steps: stepsToForm(p.Steps), From: from, Mode: "amend", Custom: p.Custom, Template: p.Template}
		if r.URL.Query().Get("mode") == "custom" && !p.Custom {
			// A custom checklist configured from this generic template.
			d.Mode, d.Custom, d.Template, d.From = "custom", true, p.ID, ""
			d.Name = p.Name + " (custom)"
		}
		if d.Template != "" {
			if t, err := domain.GetProcedure(r.Context(), s.App.Store.DB(), d.Template); err == nil {
				d.TemplateName = t.Name
			}
		}
	}
	blank := 0
	if d.From == "" && d.Template == "" {
		blank = 1 // a new checklist starts with one step to fill in
	}
	d.Steps = withBlankSteps(d.Steps, min(max(more, 0), 50), blank)
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
		Reference: strings.TrimSpace(f.Get("reference")), Category: strings.TrimSpace(f.Get("category")), From: f.Get("from"),
		Custom: f.Get("custom") == "yes", Template: f.Get("template"), TemplateName: f.Get("template_name"), Mode: f.Get("mode")}
	acount, _ := strconv.Atoi(f.Get("acount"))
	for i := 0; i < min(max(acount, 0), 100); i++ {
		m := domain.EquipmentMatch{Manufacturer: get("mfr", i), Model: get("model", i)}
		if m.Manufacturer == "" && m.Model != "" {
			return d, nil, userErr("equipment row %d: give the manufacturer of %q", i+1, m.Model)
		}
		d.AppliesTo = append(d.AppliesTo, m)
	}
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
		if st.Kind == domain.StepCheck {
			st.Unit = fs.Unit // of the measured value
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
	// Adding or removing a step (or Enter in a field) shows the builder
	// again, as typed, with that change.
	if err == nil && (r.PostForm.Has("add") || r.PostForm.Has("remove") || r.PostForm.Has("stay")) {
		if n, err := strconv.Atoi(r.PostFormValue("add")); err == nil {
			d.Steps = insertStep(d.Steps, n)
		}
		if n, err := strconv.Atoi(r.PostFormValue("remove")); err == nil {
			d.Steps, d.Notice = removeStep(d.Steps, n)
		}
		return s.renderBuilder(w, r, sess, d)
	}
	var id string
	var version int
	canPublish := roleRank[sess.User.Role] >= roleRank[domain.RoleMidTier]
	if err == nil {
		var applies []domain.EquipmentMatch
		for _, m := range d.AppliesTo {
			if m.Manufacturer != "" {
				applies = append(applies, m)
			}
		}
		if !canPublish {
			d.Custom = d.Custom || d.Mode != "amend" // others make custom checklists, or amend what exists
		}
		p := domain.ProcedurePublished{Name: d.Name, Description: d.Description, Reference: d.Reference, Category: d.Category, AppliesTo: applies, Steps: steps,
			Custom: d.Custom, Template: d.Template}
		if !canPublish {
			// Others' checklists wait for a mid-tier or super user.
			basedOn := d.From
			if basedOn == "" {
				basedOn = d.Template
			}
			if id, err = s.App.ProposeChecklist(r.Context(), s.actor(sess), p, basedOn); err == nil {
				return s.done(w, r, sess, "/checklist-proposals/"+id, "Sent for approval. A mid-tier or super user approves it on the master Pi; until then it can't be used.")
			}
		} else {
			id, version, err = s.App.PublishChecklist(r.Context(), s.actor(sess), p)
		}
	}
	if err != nil {
		msg := message(err)
		if msg == "" {
			return err
		}
		d.Error = "Not saved: " + msg
		w.WriteHeader(http.StatusUnprocessableEntity)
		return s.renderBuilder(w, r, sess, d)
	}
	return s.done(w, r, sess, "/procedures/"+id, fmt.Sprintf("Published %s version %d.", d.Name, version))
}

// checklistFile is a checklist as downloaded, to copy to another pi-fleet.
type checklistFile struct {
	Format      string                  `json:"format"` // "pi-fleet checklist"
	Name        string                  `json:"name"`
	Description string                  `json:"description,omitempty"`
	Reference   string                  `json:"reference,omitempty"`
	Category    string                  `json:"category,omitempty"`
	AppliesTo   []domain.EquipmentMatch `json:"applies_to,omitempty"`
	Steps       []domain.Step           `json:"steps"`
}

const checklistFormat = "pi-fleet checklist"

func (s *Server) procedureExport(w http.ResponseWriter, r *http.Request, sess *session) error {
	p, err := domain.GetProcedure(r.Context(), s.App.Store.DB(), r.PathValue("id"))
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(checklistFile{Format: checklistFormat, Name: p.Name, Description: p.Description, Reference: p.Reference,
		Category: p.Category, AppliesTo: p.AppliesTo, Steps: p.Steps}, "", "  ")
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
	d := builderData{Name: c.Name, Description: c.Description, Reference: c.Reference, Category: c.Category, AppliesTo: c.AppliesTo, Steps: stepsToForm(c.Steps)}
	return s.renderBuilder(w, r, sess, d)
}

func (s *Server) procedureRetire(w http.ResponseWriter, r *http.Request, sess *session) error {
	id := r.PathValue("id")
	if err := s.App.RetireProcedure(r.Context(), s.actor(sess), id, strings.TrimSpace(r.PostFormValue("reason"))); err != nil {
		return s.failed(w, r, sess, "/procedures/"+id, err)
	}
	return s.done(w, r, sess, "/procedures/"+id, "Retired. Work already using it keeps it.")
}

// choiceGroup is a labelled group of options (an optgroup).
type choiceGroup struct {
	Label string
	Items []option
}

// checklistChoices groups the checklists that can be added to a work
// order on this equipment: those for its model, its manufacturer, any
// equipment, then all others; those already on it are left out.
func checklistChoices(ctx context.Context, q domain.Querier, a domain.Asset, on []domain.WorkOrderChecklist) ([]choiceGroup, error) {
	all, err := domain.ChecklistsFor(ctx, q, a.Manufacturer, a.Model)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, c := range on {
		have[c.Name] = true
	}
	groups := []choiceGroup{{Label: "For " + a.Manufacturer + " " + a.Model}, {Label: "For all " + a.Manufacturer + " equipment"},
		{Label: "For any equipment"}, {Label: "Other checklists"}}
	idx := map[string]int{"model": 0, "manufacturer": 1, "any": 2, "": 3}
	for _, c := range all {
		if have[c.Name] {
			continue
		}
		g := &groups[idx[c.Fit]]
		kind := "generic template"
		if c.Custom {
			kind = "custom"
		}
		g.Items = append(g.Items, option{ID: c.ID, Label: fmt.Sprintf("%s (v%d, %s)", c.Name, c.Version, kind)})
	}
	var out []choiceGroup
	for _, g := range groups {
		if len(g.Items) > 0 {
			out = append(out, g)
		}
	}
	return out, nil
}

func (s *Server) workOrderAddChecklist(w http.ResponseWriter, r *http.Request, sess *session) error {
	id := r.PathValue("id")
	if err := s.App.AddChecklist(r.Context(), s.actor(sess), id, r.PostFormValue("procedure")); err != nil {
		return s.failed(w, r, sess, "/work-orders/"+id+"#checklist", err)
	}
	return s.done(w, r, sess, "/work-orders/"+id+"#checklist", "Checklist added.")
}

func (s *Server) workOrderRemoveChecklist(w http.ResponseWriter, r *http.Request, sess *session) error {
	id := r.PathValue("id")
	if err := s.App.RemoveChecklist(r.Context(), s.actor(sess), id, r.PostFormValue("procedure")); err != nil {
		return s.failed(w, r, sess, "/work-orders/"+id+"#checklist", err)
	}
	return s.done(w, r, sess, "/work-orders/"+id+"#checklist", "Checklist removed.")
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

// workOrderChecklist saves a checklist's page: every answer that is new
// or changed, in order (so a step that depends on an earlier one sees its
// new answer), and any photos added to steps. Steps that can't be saved
// are listed; the others are kept. It then goes back to the checklist at
// the next step to answer (or, asked, to the work order).
func (s *Server) workOrderChecklist(w http.ResponseWriter, r *http.Request, sess *session) error {
	ctx := r.Context()
	id := r.PathValue("id")
	wo, err := domain.GetWorkOrder(ctx, s.App.Store.DB(), id)
	if err != nil {
		return err
	}
	all, err := domain.Checklists(ctx, s.App.Store.DB(), wo)
	if err != nil {
		return err
	}
	procID := r.PostFormValue("procedure")
	var steps []domain.StepResult
	for _, c := range all {
		if c.ID == procID || (procID == "" && c.First) {
			steps, procID = c.Steps, c.ID
		}
	}
	saved, photos := 0, 0
	var problems []string
	problem := func(err error) error {
		msg := message(err)
		if msg == "" {
			return err
		}
		problems = append(problems, msg)
		return nil
	}
	for _, st := range steps {
		if domain.Inputless(st.Kind) {
			continue
		}
		// Photos first, so a photo step is answered by them.
		if r.MultipartForm != nil {
			for _, h := range r.MultipartForm.File["p_"+st.ID] {
				if err := s.addStepPhoto(r, sess, id, procID, st, h); err != nil {
					if err := problem(err); err != nil {
						return err
					}
					continue
				}
				photos++
			}
		}
		if st.Kind == domain.StepPhoto {
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
		measured := strings.TrimSpace(r.PostFormValue("m_" + st.ID))
		if value == "" || (st.Recorded && value == st.Value && note == st.Note && measured == st.Measured) {
			continue
		}
		err := s.phiCheck(r, value, note, measured)
		if err == nil {
			err = s.App.RecordAnswer(ctx, s.actor(sess), id, procID, st.ID, value, note, measured)
		}
		if err != nil {
			if err := problem(err); err != nil {
				return err
			}
			continue
		}
		saved++
	}
	var parts []string
	if saved > 0 {
		parts = append(parts, fmt.Sprintf("Saved %d answer%s", saved, map[bool]string{true: "", false: "s"}[saved == 1]))
	}
	if photos > 0 {
		parts = append(parts, fmt.Sprintf("added %d photo%s", photos, map[bool]string{true: "", false: "s"}[photos == 1]))
	}
	msg := strings.Join(parts, " and ") + "."
	if len(parts) == 0 && len(problems) == 0 {
		msg = "Nothing new to save."
	}
	if len(problems) > 0 {
		msg = strings.TrimSuffix(msg, ".") + ". Not saved: " + strings.Join(problems, "; ") + "."
		msg = strings.TrimPrefix(msg, ". ")
	}
	if r.PostFormValue("after") == "back" {
		return s.done(w, r, sess, "/work-orders/"+id+"#checklist", msg)
	}
	// Back to the checklist, at the next step to answer.
	anchor := "#done"
	if now, err := domain.GetWorkOrder(ctx, s.App.Store.DB(), id); err == nil {
		if cs, err := domain.Checklists(ctx, s.App.Store.DB(), now); err == nil {
			for _, c := range cs {
				if c.ID == procID {
					if next := nextUnanswered(c.Steps); next != "" {
						anchor = "#step-" + next
					} else if len(problems) == 0 {
						msg += " Every step is answered: go back to the work order to finish it."
					}
				}
			}
		}
	}
	return s.done(w, r, sess, "/work-orders/"+id+"/checklists/"+procID+anchor, msg)
}

// addStepPhoto attaches an uploaded photo to a checklist step.
func (s *Server) addStepPhoto(r *http.Request, sess *session, woID, procID string, st domain.StepResult, hdr *multipart.FileHeader) error {
	f, err := hdr.Open()
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, blobs.MaxSize+1))
	if err != nil {
		return err
	}
	if err := s.phiCheck(r, hdr.Filename); err != nil {
		return err
	}
	_, err = s.App.AddStepPhoto(r.Context(), s.actor(sess), woID, procID, st.ID, hdr.Filename, "Checklist photo: "+st.Text, data)
	if errors.Is(err, blobs.ErrType) || errors.Is(err, blobs.ErrTooLarge) || errors.Is(err, blobs.ErrMalformed) || errors.Is(err, blobs.ErrHEIC) {
		return userErr("photo %s: %s", hdr.Filename, strings.TrimPrefix(err.Error(), "blobs: "))
	}
	return err
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
