package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"pi-fleet/internal/domain"
	"pi-fleet/internal/event"
	"pi-fleet/internal/export"
)

// Record copies for inspection (Part 11 §11.10(b)): an audit trail page,
// a printable record (save as PDF from the browser), and a JSON bundle of
// the signed events that pi-fleet verify-export checks independently.

type auditRow struct {
	When, Clock, Actor, Type, Summary, Node, Hash string
	Seq                                           int64
	Flags                                         []string
	Redacted                                      bool
}

func (s *Server) auditRows(ctx context.Context, evs []event.Event) ([]auditRow, error) {
	q := s.App.Store.DB()
	names := map[string]string{}
	rows, err := q.QueryContext(ctx, `SELECT id, legal_name || ' (' || username || ')' FROM users`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, n string
		rows.Scan(&id, &n)
		names[id] = n
	}
	rows.Close()
	out := make([]auditRow, 0, len(evs))
	for _, e := range evs {
		a := auditRow{
			When: e.WallTime.UTC().Format(time.RFC3339), Clock: string(e.ClockState), Type: e.Type,
			Node: e.NodeID, Seq: e.Seq, Hash: e.Hash.String(), Redacted: e.Redacted(),
		}
		a.Actor = names[e.ActorUserID]
		if a.Actor == "" {
			a.Actor = e.ActorUserID
		}
		if !e.Redacted() {
			var buf bytes.Buffer
			if json.Indent(&buf, e.Payload, "", "  ") == nil {
				a.Summary = buf.String()
			}
		}
		fr, err := q.QueryContext(ctx, `SELECT f.flag || ': ' || f.detail || coalesce(' — ' || r.resolution || ': ' || r.note, '')
			FROM event_flags f LEFT JOIN flag_resolutions r USING (event_id) WHERE f.event_id = ?`, e.EventID)
		if err != nil {
			return nil, err
		}
		for fr.Next() {
			var f string
			fr.Scan(&f)
			a.Flags = append(a.Flags, f)
		}
		fr.Close()
		out = append(out, a)
	}
	return out, nil
}

type auditData struct {
	Subject  export.Subject
	Back     string
	Rows     []auditRow
	Complete bool
}

func (s *Server) subject(r *http.Request, typ string) (export.Subject, string, error) {
	ctx, q, id := r.Context(), s.App.Store.DB(), r.PathValue("id")
	switch typ {
	case "work_order":
		w, err := domain.GetWorkOrder(ctx, q, id)
		return export.Subject{Type: typ, ID: id, Label: w.Number}, "/work-orders/" + id, err
	default:
		a, err := domain.GetAsset(ctx, q, id)
		return export.Subject{Type: typ, ID: id, Label: a.Tag}, "/assets/" + id, err
	}
}

func (s *Server) auditPage(typ string) handler {
	return func(w http.ResponseWriter, r *http.Request, sess *session) error {
		sub, back, err := s.subject(r, typ)
		if err != nil {
			return err
		}
		evs, err := export.Events(r.Context(), s.App.Store, typ, sub.ID)
		if err != nil {
			return err
		}
		rows, err := s.auditRows(r.Context(), evs)
		if err != nil {
			return err
		}
		return s.render(w, r, sess, "audit", "Audit trail: "+sub.Label, auditData{Subject: sub, Back: back, Rows: rows, Complete: s.Role == "central"})
	}
}

func (s *Server) exportJSON(typ string) handler {
	return func(w http.ResponseWriter, r *http.Request, sess *session) error {
		ctx := r.Context()
		sub, _, err := s.subject(r, typ)
		if err != nil {
			return err
		}
		evs, err := export.Events(ctx, s.App.Store, typ, sub.ID)
		if err != nil {
			return err
		}
		b, err := export.Build(ctx, s.App.Store, sub, evs, sess.User.Username,
			s.Role+" "+s.App.Author.NodeID, s.Role == "central", s.now())
		if err != nil {
			return err
		}
		body, err := json.MarshalIndent(b, "", "  ")
		if err != nil {
			return err
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.json"`, typ, sub.Label))
		_, err = w.Write(body)
		return err
	}
}

type printData struct {
	Title       string
	WO          *woData
	Asset       *assetData
	Audit       []auditRow
	GeneratedBy string
	GeneratedAt time.Time
	Source      string
	Complete    bool
}

func (s *Server) printPage(typ string) handler {
	return func(w http.ResponseWriter, r *http.Request, sess *session) error {
		ctx := r.Context()
		sub, _, err := s.subject(r, typ)
		if err != nil {
			return err
		}
		d := printData{Title: sub.Label, GeneratedBy: sess.User.LegalName + " (" + sess.User.Username + ")",
			GeneratedAt: s.now(), Source: s.Role + " " + s.App.Author.NodeID, Complete: s.Role == "central"}
		if typ == "work_order" {
			wo, err := s.loadWorkOrder(r, sess, sub.ID)
			if err != nil {
				return err
			}
			d.WO = &wo
		} else {
			a, err := s.loadAsset(r, sub.ID)
			if err != nil {
				return err
			}
			d.Asset = &a
		}
		evs, err := export.Events(ctx, s.App.Store, typ, sub.ID)
		if err != nil {
			return err
		}
		if d.Audit, err = s.auditRows(ctx, evs); err != nil {
			return err
		}
		var buf bytes.Buffer
		if err := s.pages["print"].ExecuteTemplate(&buf, "print", d); err != nil {
			return err
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, err = buf.WriteTo(w)
		return err
	}
}
