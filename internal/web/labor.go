package web

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"pi-fleet/internal/app"
	"pi-fleet/internal/domain"
)

// Time on a work order: Start work runs a clock for labour or travel
// until Stop work, which records it; time can also be typed in, and any
// entry changed afterwards.

// clockView is a running clock as a page shows it.
type clockView struct {
	domain.LaborClock
	Since   string // started, local "15:04"
	Elapsed string // h:mm so far
	Long    bool   // running for 10 hours or more: maybe forgotten
	Here    bool   // on the work order being shown
}

func (s *Server) clockView(c domain.LaborClock, woID string) clockView {
	d := s.now().Sub(c.Started)
	return clockView{LaborClock: c, Since: c.Started.Local().Format("15:04"), Elapsed: elapsed(d), Long: d >= 10*time.Hour, Here: c.WorkOrderID == woID}
}

// elapsed is h:mm.
func elapsed(d time.Duration) string {
	m := max(int(d/time.Minute), 0)
	return fmt.Sprintf("%d:%02d", m/60, m%60)
}

// kindName is what a kind of time is called.
func kindName(k string) string {
	if k == domain.LaborKindTravel {
		return "Travel"
	}
	return "Labour"
}

// stoppedText says what a stopped clock recorded.
func stoppedText(st app.Stopped) string {
	if st.Minutes < 1 {
		return "Clock stopped after less than a minute: nothing recorded."
	}
	return fmt.Sprintf("Recorded %s of %s on %s.", elapsed(time.Duration(st.Minutes)*time.Minute), strings.ToLower(kindName(st.Kind)), st.Number)
}

func (s *Server) clockStart(w http.ResponseWriter, r *http.Request, sess *session) error {
	id := r.PathValue("id")
	back := "/work-orders/" + id
	kind := r.PostFormValue("kind")
	if kind != domain.LaborKindLabor && kind != domain.LaborKindTravel {
		return s.done(w, r, sess, back, "Not started: choose Labour or Travel.")
	}
	stopped, err := s.App.StartClock(r.Context(), s.actor(sess), id, kind)
	if err != nil {
		return s.failed(w, r, sess, back, err)
	}
	msg := kindName(kind) + " clock started."
	if stopped.Running {
		msg = stoppedText(stopped) + " " + msg
	}
	return s.done(w, r, sess, back, msg) // the running clock is at the top
}

func (s *Server) clockStop(w http.ResponseWriter, r *http.Request, sess *session) error {
	back := localBack(r.PostFormValue("back"))
	note := strings.TrimSpace(r.PostFormValue("note"))
	if err := s.phiCheck(r, note); err != nil {
		return s.failed(w, r, sess, back, err)
	}
	stopped, err := s.App.StopClock(r.Context(), s.actor(sess), note)
	if err != nil {
		return s.failed(w, r, sess, back, err)
	}
	if !stopped.Running {
		return s.done(w, r, sess, back, "Your clock wasn't running.")
	}
	if strings.HasPrefix(back, "/work-orders/"+stopped.WorkOrderID) {
		back = "/work-orders/" + stopped.WorkOrderID + "#time"
	}
	return s.done(w, r, sess, back, stoppedText(stopped))
}

// clockStopOther stops someone's clock left running (mid-tier users).
func (s *Server) clockStopOther(w http.ResponseWriter, r *http.Request, sess *session) error {
	back := "/work-orders/" + r.PathValue("id") + "#time"
	stopped, err := s.App.StopClockOf(r.Context(), s.actor(sess), r.PostFormValue("user"))
	if err != nil {
		return s.failed(w, r, sess, back, err)
	}
	if !stopped.Running {
		return s.done(w, r, sess, back, "That clock had already stopped.")
	}
	return s.done(w, r, sess, back, stoppedText(stopped))
}

// laborLog records time typed in.
func (s *Server) laborLog(w http.ResponseWriter, r *http.Request, sess *session) error {
	id := r.PathValue("id")
	back := "/work-orders/" + id + "#time"
	minutes, err := parseDuration(r.PostFormValue("time"))
	if err != nil {
		return s.done(w, r, sess, back, "Not saved: "+err.Error())
	}
	note := strings.TrimSpace(r.PostFormValue("note"))
	if err := s.phiCheck(r, note); err != nil {
		return s.failed(w, r, sess, back, err)
	}
	kind := r.PostFormValue("kind")
	if kind == "" {
		kind = domain.LaborKindLabor
	}
	if _, err := s.App.LogTime(r.Context(), s.actor(sess), id, kind, minutes, r.PostFormValue("date"), note); err != nil {
		return s.failed(w, r, sess, back, err)
	}
	return s.done(w, r, sess, back, kindName(kind)+" time added.")
}

// laborEdit changes a time entry.
func (s *Server) laborEdit(w http.ResponseWriter, r *http.Request, sess *session) error {
	var woID string
	if err := s.App.Store.DB().QueryRowContext(r.Context(), `SELECT wo_id FROM labor_entries WHERE id = ?`, r.PathValue("id")).Scan(&woID); err != nil {
		return domain.ErrNotFound
	}
	back := "/work-orders/" + woID + "#time"
	minutes, err := parseDuration(r.PostFormValue("time"))
	if err != nil {
		return s.done(w, r, sess, back, "Not saved: "+err.Error())
	}
	note := strings.TrimSpace(r.PostFormValue("note"))
	if err := s.phiCheck(r, note); err != nil {
		return s.failed(w, r, sess, back, err)
	}
	if err := s.App.CorrectLabor(r.Context(), s.actor(sess), r.PathValue("id"), domain.LaborCorrected{
		Minutes: minutes, Date: r.PostFormValue("date"), Kind: r.PostFormValue("kind"), Note: note}); err != nil {
		return s.failed(w, r, sess, back, err)
	}
	return s.done(w, r, sess, back, "Time changed.")
}

// timeRow is a time entry as the work order shows it.
type timeRow struct {
	domain.LaborEntry
	From, To string // local "15:04", when recorded with the clock
	Time     string // the time as typed: h:mm
	Mine     bool   // may change it
}

func (s *Server) timeRows(entries []domain.LaborEntry, sess *session) (rows []timeRow, labour, travel int) {
	isMid := roleRank[sess.User.Role] >= roleRank[domain.RoleMidTier]
	for _, e := range entries {
		t := timeRow{LaborEntry: e, Time: elapsed(time.Duration(e.Minutes) * time.Minute), Mine: isMid || e.UserID == sess.User.ID}
		if !e.Started.IsZero() {
			t.From, t.To = e.Started.Local().Format("15:04"), e.Ended.Local().Format("15:04")
		}
		if e.Status == "logged" {
			if e.Kind == domain.LaborKindTravel {
				travel += e.Minutes
			} else {
				labour += e.Minutes
			}
		}
		rows = append(rows, t)
	}
	return rows, labour, travel
}
