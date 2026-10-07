package web

import (
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"pi-fleet/internal/domain"
)

type procedureRow struct {
	ID, Name, Status, PublishedAt string
	Version, Steps                int
}

func listProcedures(r *http.Request, s *Server, activeOnly bool) ([]procedureRow, error) {
	rows, err := s.App.Store.DB().QueryContext(r.Context(), `SELECT id, name, status, published_at, version, json_array_length(steps)
		FROM procedures WHERE ? = 0 OR status = 'active' ORDER BY name, version DESC`, activeOnly)
	return scanAll(rows, err, func(r *sql.Rows) (procedureRow, error) {
		var p procedureRow
		return p, r.Scan(&p.ID, &p.Name, &p.Status, &p.PublishedAt, &p.Version, &p.Steps)
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

type proceduresData struct {
	Rows     []procedureRow
	FormRows int
	Name     string
}

func (s *Server) procedureList(w http.ResponseWriter, r *http.Request, sess *session) error {
	rows, err := listProcedures(r, s, false)
	if err != nil {
		return err
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("rows"))
	return s.render(w, r, sess, "procedures", "Checklists", proceduresData{Rows: rows, FormRows: min(max(n, 6), 100), Name: r.URL.Query().Get("name")})
}

func (s *Server) procedureView(w http.ResponseWriter, r *http.Request, sess *session) error {
	p, err := domain.GetProcedure(r.Context(), s.App.Store.DB(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return s.render(w, r, sess, "procedure", fmt.Sprintf("%s v%d", p.Name, p.Version), p)
}

func (s *Server) procedurePublish(w http.ResponseWriter, r *http.Request, sess *session) error {
	f := r.PostForm
	get := func(name string, i int) string {
		if v := f[name]; i < len(v) {
			return strings.TrimSpace(v[i])
		}
		return ""
	}
	var steps []domain.Step
	for i := range f["text"] {
		text := get("text", i)
		if text == "" {
			continue
		}
		st := domain.Step{ID: fmt.Sprintf("s%d", len(steps)+1), Text: text, Kind: get("kind", i), Required: get("required", i) == "yes"}
		if st.Kind == domain.StepNumber {
			st.Unit, st.Lower, st.Upper = get("unit", i), get("lower", i), get("upper", i)
		}
		steps = append(steps, st)
	}
	name := strings.TrimSpace(f.Get("name"))
	id, version, err := s.App.PublishProcedure(r.Context(), s.actor(sess), name, steps)
	if err != nil {
		return s.failed(w, r, sess, "/procedures", err)
	}
	return s.done(w, r, sess, "/procedures/"+id, fmt.Sprintf("Published %s version %d.", name, version))
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
