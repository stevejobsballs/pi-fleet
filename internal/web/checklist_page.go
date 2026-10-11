package web

import (
	"fmt"
	"net/http"
	"slices"

	"pi-fleet/internal/domain"
)

// checklistPageData is one checklist of a work order, on its own page to
// follow during the service visit.
type checklistPageData struct {
	WO        domain.WorkOrder
	Asset     domain.Asset
	Place     string
	Checklist domain.WorkOrderChecklist
	Edit      bool // the person doing the work, while it is in progress
	Total     int  // steps that take an answer and apply
	Answered  int
	Failed    int
}

func (d checklistPageData) BackLink() (string, string) {
	return "/work-orders/" + d.WO.ID + "#checklist", "work order " + d.WO.Number
}

// workOrderChecklistPage shows one of a work order's checklists.
func (s *Server) workOrderChecklistPage(w http.ResponseWriter, r *http.Request, sess *session) error {
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
	d := checklistPageData{WO: wo, Checklist: all[i], Edit: wo.AssignedTo == sess.User.ID && wo.LeaseID != "" && wo.Status == domain.WOInProgress}
	if d.Asset, err = domain.GetAsset(ctx, q, wo.AssetID); err != nil {
		return err
	}
	q.QueryRowContext(ctx, `SELECT s.path || ' › ' || l.name FROM locations l JOIN sites s ON s.id = l.site_id WHERE l.id = ?`, d.Asset.LocationID).Scan(&d.Place)
	for _, st := range d.Checklist.Steps {
		if domain.Inputless(st.Kind) || !st.Applies {
			continue
		}
		d.Total++
		if st.Recorded {
			d.Answered++
		}
		if st.Outcome() == "fail" {
			d.Failed++
		}
	}
	return s.render(w, r, sess, "checklist", fmt.Sprintf("%s v%d", d.Checklist.Name, d.Checklist.Version), d)
}

// nextUnanswered is where to go after saving: the first step that applies
// and has no answer yet, or "" when all are answered.
func nextUnanswered(steps []domain.StepResult) string {
	for _, st := range steps {
		if !domain.Inputless(st.Kind) && st.Applies && !st.Recorded {
			return st.ID
		}
	}
	return ""
}
