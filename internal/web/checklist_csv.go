package web

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"pi-fleet/internal/domain"
)

// Checklists as spreadsheets (CSV), to print and fill in by hand, or to
// keep as filled in: a few lines about the visit, then one row per step
// with what passes and columns for the measured value, result, note and
// initials.

var csvColumns = []string{"No.", "Section", "Step", "How to do it", "Kind of answer", "Passes if", "Required",
	"Measured value / answer", "Result", "Note", "Initials"}

// passesIf says in words what passes a step.
func passesIf(st domain.Step) string {
	switch st.Kind {
	case domain.StepCheck:
		return "Pass"
	case domain.StepNumber:
		switch {
		case st.Lower != "" && st.Upper != "":
			return fmt.Sprintf("%s to %s %s", st.Lower, st.Upper, st.Unit)
		case st.Lower != "":
			return fmt.Sprintf("at least %s %s", st.Lower, st.Unit)
		case st.Upper != "":
			return fmt.Sprintf("at most %s %s", st.Upper, st.Unit)
		}
	case domain.StepChoice, domain.StepMulti:
		var ok, all []string
		for _, o := range st.Options {
			all = append(all, o.Label)
			if !o.Fails {
				ok = append(ok, o.Label)
			}
		}
		if len(ok) < len(all) {
			return "One of: " + strings.Join(ok, " / ")
		}
		return "Choices: " + strings.Join(all, " / ")
	}
	return ""
}

func writeChecklistCSV(w http.ResponseWriter, filename string, head [][]string, p domain.Procedure, results map[string]domain.StepResult) error {
	var buf bytes.Buffer
	buf.WriteString("\xef\xbb\xbf") // so spreadsheet programs read the text as UTF-8
	cw := csv.NewWriter(&buf)
	cw.UseCRLF = true
	for _, h := range head {
		cw.Write(h)
	}
	cw.Write(nil)
	cw.Write(csvColumns)
	section, n := "", 0
	for _, st := range p.Steps {
		switch st.Kind {
		case domain.StepHeading:
			section = st.Text
			continue
		case domain.StepInfo:
			cw.Write([]string{"", section, "NOTE: " + st.Text})
			continue
		}
		n++
		kind := kindLabels[st.Kind]
		if st.Kind == domain.StepNumber && st.Unit != "" {
			kind += " (" + st.Unit + ")"
		}
		if st.Kind == domain.StepCheck && st.Unit != "" {
			kind += ", measured in " + st.Unit
		}
		if c := st.ShowIf; c != nil {
			kind += fmt.Sprintf("; only when %q is %q", stepTextOf(p.Steps, c.Step), c.Equals)
		}
		required := ""
		if st.Required {
			required = "yes"
		}
		if st.Critical {
			required = strings.TrimPrefix(required+", critical", ", ")
		}
		row := []string{fmt.Sprint(n), section, st.Text, st.Help, kind, passesIf(st), required, "", "", "", ""}
		if r, ok := results[st.ID]; ok {
			switch {
			case !r.Applies:
				row[8] = "not needed"
			case r.Recorded:
				answer := r.Display()
				if r.Kind == domain.StepCheck {
					answer = r.Measured
				}
				result := r.Outcome()
				if r.Kind == domain.StepCheck {
					result = r.Value
				}
				row[7], row[8], row[9] = answer, result, r.Note
				row[10] = initials(r.RecordedBy)
			}
		}
		cw.Write(row)
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return err
	}
	h := w.Header()
	h.Set("Content-Type", "text/csv; charset=utf-8")
	h.Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(filename))
	_, err := w.Write(buf.Bytes())
	return err
}

func stepTextOf(steps []domain.Step, id string) string {
	for _, s := range steps {
		if s.ID == id {
			return s.Text
		}
	}
	return id
}

// initials of a name: "Tess Tech" → "TT".
func initials(name string) string {
	var b strings.Builder
	for _, f := range strings.Fields(name) {
		b.WriteString(strings.ToUpper(string([]rune(f)[:1])))
	}
	return b.String()
}

// procedureCSV is a blank checklist to print and fill in by hand.
func (s *Server) procedureCSV(w http.ResponseWriter, r *http.Request, sess *session) error {
	p, err := domain.GetProcedure(r.Context(), s.App.Store.DB(), r.PathValue("id"))
	if err != nil {
		return err
	}
	head := [][]string{
		{fmt.Sprintf("%s (version %d)", p.Name, p.Version)},
		{"Follows", p.Reference},
		{"Equipment (asset tag)", ""}, {"Manufacturer and model", ""}, {"Serial number", ""}, {"Location", ""},
		{"Date", ""}, {"Technician", ""}, {"Signature", ""},
	}
	return writeChecklistCSV(w, fmt.Sprintf("%s v%d.csv", p.Name, p.Version), head, p, nil)
}

// workOrderChecklistCSV is a work order's checklist as filled in.
func (s *Server) workOrderChecklistCSV(w http.ResponseWriter, r *http.Request, sess *session) error {
	ctx, q := r.Context(), s.App.Store.DB()
	wo, err := domain.GetWorkOrder(ctx, q, r.PathValue("id"))
	if err != nil {
		return err
	}
	all, err := domain.Checklists(ctx, q, wo)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(all, func(c domain.WorkOrderChecklist) bool { return c.ID == r.PathValue("proc") })
	if i < 0 {
		return domain.ErrNotFound
	}
	c := all[i]
	a, err := domain.GetAsset(ctx, q, wo.AssetID)
	if err != nil {
		return err
	}
	var place string
	q.QueryRowContext(ctx, `SELECT s.path || ' › ' || l.name FROM locations l JOIN sites s ON s.id = l.site_id WHERE l.id = ?`, a.LocationID).Scan(&place)
	results := map[string]domain.StepResult{}
	var last string
	for _, st := range c.Steps {
		results[st.ID] = st
		if st.Recorded && st.RecordedAt.Format("2006-01-02") > last {
			last = st.RecordedAt.Format("2006-01-02")
		}
	}
	head := [][]string{
		{fmt.Sprintf("%s (version %d)", c.Name, c.Version)},
		{"Follows", c.Reference},
		{"Work order", wo.Number + ": " + wo.Title},
		{"Equipment (asset tag)", a.Tag}, {"Manufacturer and model", a.Manufacturer + " " + a.Model}, {"Serial number", a.Serial}, {"Location", place},
		{"Date", last}, {"Technician", ""}, {"Signature", ""},
	}
	return writeChecklistCSV(w, fmt.Sprintf("%s %s.csv", wo.Number, c.Name), head, c.Procedure, results)
}
