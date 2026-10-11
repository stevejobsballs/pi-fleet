package web

import (
	"context"
	"database/sql"
	"net/http"

	"pi-fleet/internal/domain"
)

// People: everyone can look up colleagues, read-only, to see who someone
// is and what work they have. Only what's needed for that is shown: name,
// role and home sites. Usernames, emails and account details stay on the
// super users' User accounts page.

type personRow struct {
	ID, Name, Role, Sites string
	Open                  int // work orders assigned and not yet closed
}

// listPeople lists active people, or the one with this id.
func listPeople(ctx context.Context, q domain.Querier, id string) ([]personRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT u.id, u.legal_name, u.role,
			coalesce((SELECT group_concat(s.name, ', ') FROM json_each(u.home_sites) h JOIN sites s ON s.id = h.value), ''),
			(SELECT count(*) FROM work_orders w WHERE w.assigned_to = u.id AND w.status NOT IN ('closed', 'cancelled'))
		FROM users u WHERE u.status = 'active' AND (? = '' OR u.id = ?) ORDER BY u.legal_name`, id, id)
	return scanAll(rows, err, func(r *sql.Rows) (personRow, error) {
		var p personRow
		return p, r.Scan(&p.ID, &p.Name, &p.Role, &p.Sites, &p.Open)
	})
}

func (s *Server) peopleList(w http.ResponseWriter, r *http.Request, sess *session) error {
	people, err := listPeople(r.Context(), s.App.Store.DB(), "")
	if err != nil {
		return err
	}
	return s.render(w, r, sess, "people", "People", people)
}

type personData struct {
	Person personRow
	Work   []woRow
}

func (d personData) BackLink() (string, string) { return "/people", "People" }

func (s *Server) personView(w http.ResponseWriter, r *http.Request, sess *session) error {
	ctx, q := r.Context(), s.App.Store.DB()
	people, err := listPeople(ctx, q, r.PathValue("id"))
	if err != nil {
		return err
	}
	if len(people) != 1 {
		return domain.ErrNotFound
	}
	d := personData{Person: people[0]}
	if d.Work, err = listWorkOrders(ctx, q, "mine", d.Person.ID); err != nil {
		return err
	}
	return s.render(w, r, sess, "person", d.Person.Name, d)
}
