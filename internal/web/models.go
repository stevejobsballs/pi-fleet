package web

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"pi-fleet/internal/app"
	"pi-fleet/internal/domain"
)

// The make/model catalogue (Equipment › Models, on the master Pi): what
// each model is, its usual risk class, and the schedules its equipment
// should have, set up when the equipment is registered.

// modelAsset is a piece of equipment, for grouping by model.
type modelAsset struct {
	ID, Tag, Manufacturer, Model, Site, Location, Status string
}

// assetsByModel groups unmerged, unretired equipment by catalogue key.
func assetsByModel(ctx context.Context, q domain.Querier) (map[string][]modelAsset, error) {
	rows, err := q.QueryContext(ctx, `SELECT a.id, a.tag, a.manufacturer, a.model, s.path, l.name, a.status
		FROM assets a JOIN sites s ON s.id = a.site_id JOIN locations l ON l.id = a.location_id
		WHERE a.merged_into = '' AND a.status != 'retired' ORDER BY a.tag`)
	list, err := scanAll(rows, err, func(r *sql.Rows) (modelAsset, error) {
		var a modelAsset
		return a, r.Scan(&a.ID, &a.Tag, &a.Manufacturer, &a.Model, &a.Site, &a.Location, &a.Status)
	})
	out := map[string][]modelAsset{}
	for _, a := range list {
		k := domain.ModelKey(a.Manufacturer, a.Model)
		out[k] = append(out[k], a)
	}
	return out, err
}

type modelRow struct {
	M         domain.Model
	Equipment int
}

// uncatalogued is a manufacturer and model with equipment but no
// catalogue entry.
type uncatalogued struct {
	Manufacturer, Model string
	Equipment           int
}

func (u uncatalogued) AddHref() string {
	return "/models/new?manufacturer=" + url.QueryEscape(u.Manufacturer) + "&model=" + url.QueryEscape(u.Model)
}

type modelsData struct {
	Models       []modelRow
	Uncatalogued []uncatalogued
}

func (s *Server) modelList(w http.ResponseWriter, r *http.Request, sess *session) error {
	ctx, q := r.Context(), s.App.Store.DB()
	models, err := domain.Models(ctx, q)
	if err != nil {
		return err
	}
	byModel, err := assetsByModel(ctx, q)
	if err != nil {
		return err
	}
	var d modelsData
	known := map[string]bool{}
	for _, m := range models {
		known[m.Key] = true
		d.Models = append(d.Models, modelRow{m, len(byModel[m.Key])})
	}
	for k, list := range byModel {
		if !known[k] {
			d.Uncatalogued = append(d.Uncatalogued, uncatalogued{list[0].Manufacturer, list[0].Model, len(list)})
		}
	}
	sort.Slice(d.Uncatalogued, func(i, j int) bool {
		a, b := d.Uncatalogued[i], d.Uncatalogued[j]
		if a.Equipment != b.Equipment {
			return a.Equipment > b.Equipment
		}
		return domain.ModelKey(a.Manufacturer, a.Model) < domain.ModelKey(b.Manufacturer, b.Model)
	})
	return s.render(w, r, sess, "models", "Models", d)
}

// modelFormData is the add or change form.
type modelFormData struct {
	ID         string
	Version    int64
	M          domain.ModelDefined
	Types      []string
	Checklists []string // active checklist names
	Error      string
}

func (d modelFormData) BackLink() (string, string) {
	if d.ID != "" {
		return "/models/" + d.ID, d.M.Manufacturer + " " + d.M.Model
	}
	return "/models", "Models"
}

func (s *Server) renderModelForm(w http.ResponseWriter, r *http.Request, sess *session, d modelFormData) error {
	rows, err := s.App.Store.DB().QueryContext(r.Context(), `SELECT DISTINCT name FROM procedures WHERE status = 'active' ORDER BY name`)
	if d.Checklists, err = scanAll(rows, err, func(r *sql.Rows) (string, error) {
		var n string
		return n, r.Scan(&n)
	}); err != nil {
		return err
	}
	d.Types = []string{"pm", "calibration", "inspection"}
	title := "Add a model"
	if d.ID != "" {
		title = "Change " + d.M.Manufacturer + " " + d.M.Model
	}
	return s.render(w, r, sess, "model_form", title, d)
}

func (s *Server) modelNew(w http.ResponseWriter, r *http.Request, sess *session) error {
	q := r.URL.Query()
	return s.renderModelForm(w, r, sess, modelFormData{M: domain.ModelDefined{Manufacturer: q.Get("manufacturer"), Model: q.Get("model")}})
}

// modelFromForm reads the form: the model's details and its schedules,
// one row each (sched_N_*), skipping rows left empty or marked removed.
func modelFromForm(r *http.Request) domain.ModelDefined {
	f := func(k string) string { return strings.TrimSpace(r.PostFormValue(k)) }
	m := domain.ModelDefined{Manufacturer: f("manufacturer"), Model: f("model"), Kind: f("kind"), RiskClass: f("risk_class"), Notes: f("notes")}
	n, _ := strconv.Atoi(f("rows"))
	for i := range min(n, 20) {
		p := fmt.Sprintf("sched_%d_", i)
		if f(p+"title") == "" || f(p+"remove") == "yes" {
			continue
		}
		every, _ := strconv.Atoi(f(p + "every"))
		grace, _ := strconv.Atoi(f(p + "grace"))
		m.Schedules = append(m.Schedules, domain.ModelSchedule{WOType: f(p + "type"), Title: f(p + "title"),
			IntervalDays: every, GraceDays: grace, Checklist: f(p + "checklist")})
	}
	return m
}

func (s *Server) modelCreate(w http.ResponseWriter, r *http.Request, sess *session) error {
	m := modelFromForm(r)
	id, err := s.App.DefineModel(r.Context(), s.actor(sess), m)
	if err != nil {
		if msg := message(err); msg != "" {
			return s.renderModelForm(w, r, sess, modelFormData{M: m, Error: "Not saved: " + msg})
		}
		return err
	}
	return s.done(w, r, sess, "/models/"+id, "Added "+m.Manufacturer+" "+m.Model+" to the catalogue.")
}

func (s *Server) modelEdit(w http.ResponseWriter, r *http.Request, sess *session) error {
	m, err := domain.GetModel(r.Context(), s.App.Store.DB(), r.PathValue("id"))
	if err != nil {
		return err
	}
	return s.renderModelForm(w, r, sess, modelFormData{ID: m.ID, Version: m.Version, M: domain.ModelDefined{Manufacturer: m.Manufacturer,
		Model: m.Model, Kind: m.Kind, RiskClass: m.RiskClass, Notes: m.Notes, Schedules: m.Schedules}})
}

func (s *Server) modelChange(w http.ResponseWriter, r *http.Request, sess *session) error {
	id := r.PathValue("id")
	m := modelFromForm(r)
	version := formInt(r, "version")
	if err := s.App.ChangeModel(r.Context(), s.actor(sess), id, version, domain.ModelChanged(m)); err != nil {
		if msg := message(err); msg != "" {
			return s.renderModelForm(w, r, sess, modelFormData{ID: id, Version: version, M: m, Error: "Not saved: " + msg})
		}
		return err
	}
	return s.done(w, r, sess, "/models/"+id, "Saved. Equipment already registered keeps its schedules; use the button below to add new ones to it.")
}

type modelData struct {
	Model      domain.Model
	Equipment  []modelAsset
	Missing    []string // ids of equipment missing some of the model's schedules
	Checklists []domain.ChecklistMatch
}

func (d modelData) BackLink() (string, string) { return "/models", "Models" }

func (s *Server) modelView(w http.ResponseWriter, r *http.Request, sess *session) error {
	ctx, q := r.Context(), s.App.Store.DB()
	m, err := domain.GetModel(ctx, q, r.PathValue("id"))
	if err != nil {
		return err
	}
	d := modelData{Model: m}
	byModel, err := assetsByModel(ctx, q)
	if err != nil {
		return err
	}
	d.Equipment = byModel[m.Key]
	for _, a := range d.Equipment {
		missing, err := app.MissingModelSchedules(ctx, q, m, a.ID)
		if err != nil {
			return err
		}
		if len(missing) > 0 {
			d.Missing = append(d.Missing, a.ID)
		}
	}
	all, err := domain.ChecklistsFor(ctx, q, m.Manufacturer, m.Model)
	if err != nil {
		return err
	}
	for _, c := range all {
		if c.Fit == "model" || c.Fit == "manufacturer" {
			d.Checklists = append(d.Checklists, c)
		}
	}
	return s.render(w, r, sess, "model", m.Manufacturer+" "+m.Model, d)
}

// modelApply sets up the model's schedules on all its equipment that
// lacks them.
func (s *Server) modelApply(w http.ResponseWriter, r *http.Request, sess *session) error {
	ctx, q := r.Context(), s.App.Store.DB()
	id := r.PathValue("id")
	m, err := domain.GetModel(ctx, q, id)
	if err != nil {
		return err
	}
	byModel, err := assetsByModel(ctx, q)
	if err != nil {
		return err
	}
	var ids []string
	for _, a := range byModel[m.Key] {
		ids = append(ids, a.ID)
	}
	n, err := s.App.SetUpModelSchedules(ctx, s.actor(sess), id, ids...)
	if err != nil {
		return s.failed(w, r, sess, "/models/"+id, err)
	}
	return s.done(w, r, sess, "/models/"+id, fmt.Sprintf("Set up %d schedule%s.", n, plural(n)))
}

// assetModelSchedules sets up one piece of equipment's missing catalogue
// schedules, from its page.
func (s *Server) assetModelSchedules(w http.ResponseWriter, r *http.Request, sess *session) error {
	ctx, q := r.Context(), s.App.Store.DB()
	id := r.PathValue("id")
	a, err := domain.GetAsset(ctx, q, id)
	if err != nil {
		return err
	}
	m, err := domain.ModelOf(ctx, q, a.Manufacturer, a.Model)
	if err != nil {
		return err
	}
	n, err := s.App.SetUpModelSchedules(ctx, s.actor(sess), m.ID, id)
	if err != nil {
		return s.failed(w, r, sess, "/assets/"+id, err)
	}
	return s.done(w, r, sess, "/assets/"+id+"#schedules", fmt.Sprintf("Set up %d schedule%s from the catalogue.", n, plural(n)))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// findModels finds catalogued models by manufacturer, model or kind.
func findModels(ctx context.Context, q domain.Querier, text string) ([]domain.Model, error) {
	all, err := domain.Models(ctx, q)
	t := strings.ToLower(strings.Join(strings.Fields(text), " "))
	var out []domain.Model
	for _, m := range all {
		if strings.Contains(strings.ToLower(m.Manufacturer+" "+m.Model+" "+m.Kind), t) {
			out = append(out, m)
		}
	}
	return out, err
}
