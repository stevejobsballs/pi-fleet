package web

import (
	"database/sql"
	"net/http"
	"strings"
)

type kioskRow struct {
	ID, Name, Site, PiStatus string
	ActivationPending        bool
	Members                  []option
}

type kiosksData struct {
	Kiosks []kioskRow
	Sites  []option
	Users  []option
}

func (s *Server) kioskList(w http.ResponseWriter, r *http.Request, sess *session) error {
	ctx, q := r.Context(), s.App.Store.DB()
	rows, err := q.QueryContext(ctx, `SELECT k.id, k.name, s.code || ' · ' || s.name, k.activation_verifier != '',
			coalesce((SELECT status FROM nodes n WHERE n.kiosk_id = k.id ORDER BY activated_at DESC LIMIT 1), 'not activated')
		FROM kiosks k JOIN sites s ON s.id = k.site_id ORDER BY k.name`)
	kiosks, err := scanAll(rows, err, func(r *sql.Rows) (kioskRow, error) {
		var k kioskRow
		return k, r.Scan(&k.ID, &k.Name, &k.Site, &k.ActivationPending, &k.PiStatus)
	})
	if err != nil {
		return err
	}
	for i := range kiosks {
		rows, err := q.QueryContext(ctx, `SELECT u.id, u.legal_name || ' (' || u.username || ')' FROM kiosk_members m
			JOIN users u ON u.id = m.user_id WHERE m.kiosk_id = ? AND m.removed_hlc IS NULL ORDER BY u.legal_name`, kiosks[i].ID)
		if kiosks[i].Members, err = scanAll(rows, err, func(r *sql.Rows) (option, error) {
			var o option
			return o, r.Scan(&o.ID, &o.Label)
		}); err != nil {
			return err
		}
	}
	d := kiosksData{Kiosks: kiosks}
	if d.Sites, err = siteOptions(ctx, q); err != nil {
		return err
	}
	if d.Users, err = kioskUserOptions(ctx, q); err != nil {
		return err
	}
	return s.render(w, r, sess, "kiosks", "Kiosks", d)
}

func (s *Server) kioskCreate(w http.ResponseWriter, r *http.Request, sess *session) error {
	name := strings.ToLower(strings.TrimSpace(r.PostFormValue("name")))
	_, pw, err := s.App.CreateKiosk(r.Context(), s.actor(sess), r.PostFormValue("site"), name)
	if err != nil {
		return s.failed(w, r, sess, "/admin/kiosks", err)
	}
	return s.render(w, r, sess, "onetime", "Kiosk activation password", oneTimeData{Username: name, Password: pw, What: "kiosk"})
}

func (s *Server) kioskReset(w http.ResponseWriter, r *http.Request, sess *session) error {
	var name string
	s.App.Store.DB().QueryRowContext(r.Context(), `SELECT name FROM kiosks WHERE id = ?`, r.PathValue("id")).Scan(&name)
	pw, err := s.App.ResetKioskActivation(r.Context(), s.actor(sess), r.PathValue("id"))
	if err != nil {
		return s.failed(w, r, sess, "/admin/kiosks", err)
	}
	return s.render(w, r, sess, "onetime", "Kiosk activation password", oneTimeData{Username: name, Password: pw, What: "kiosk"})
}

func (s *Server) kioskMember(add bool) handler {
	return func(w http.ResponseWriter, r *http.Request, sess *session) error {
		var err error
		if add {
			err = s.App.AddKioskMember(r.Context(), s.actor(sess), r.PathValue("id"), r.PostFormValue("user"))
		} else {
			err = s.App.RemoveKioskMember(r.Context(), s.actor(sess), r.PathValue("id"), r.PostFormValue("user"))
		}
		if err != nil {
			return s.failed(w, r, sess, "/admin/kiosks", err)
		}
		if add {
			return s.done(w, r, sess, "/admin/kiosks", "Member added. They can sign in on the kiosk after its next sync.")
		}
		return s.done(w, r, sess, "/admin/kiosks", "Member removed. They lose access to the kiosk at its next sync.")
	}
}
