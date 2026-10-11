package domain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"pi-fleet/internal/store"
)

// The make/model catalogue. A model says what a manufacturer's model is,
// its usual risk class, and the service schedules its equipment should
// have. Equipment belongs to a model by its manufacturer and model names
// (ignoring case and spacing), the same way checklists are matched, so
// existing equipment needs no change. Registering equipment of a
// catalogued model sets up its schedules; anyone may do that, as the
// schedules were set by a mid-tier user when defining the model.
//
// The catalogue lives on the master Pi; employee Pis don't receive it.

const (
	TypeModelDefined = "model.defined"
	TypeModelChanged = "model.changed"
	EntityModel      = "model"
)

// ModelSchedule is a schedule every piece of a model's equipment should
// have. Checklist names a checklist; generated work follows its newest
// version.
type ModelSchedule struct {
	WOType       string `json:"wo_type"` // pm, calibration or inspection
	Title        string `json:"title"`
	IntervalDays int    `json:"interval_days"`
	GraceDays    int    `json:"grace_days,omitempty"`
	Checklist    string `json:"checklist,omitempty"`
}

// ModelDefined adds a model to the catalogue.
type ModelDefined struct {
	Manufacturer string          `json:"manufacturer"`
	Model        string          `json:"model"`
	Kind         string          `json:"kind"` // what it is, e.g. "Infusion pump"
	RiskClass    string          `json:"risk_class,omitempty"`
	Notes        string          `json:"notes,omitempty"`
	Schedules    []ModelSchedule `json:"schedules,omitempty"`
}

// ModelChanged replaces a model's details. Its equipment's existing
// schedules don't change.
type ModelChanged ModelDefined

func init() {
	payloadTypes[TypeModelDefined] = struct {
		entity string
		new    func() any
	}{EntityModel, func() any { return &ModelDefined{} }}
	payloadTypes[TypeModelChanged] = struct {
		entity string
		new    func() any
	}{EntityModel, func() any { return &ModelChanged{} }}
}

// ModelKey is how equipment is matched to a model.
func ModelKey(manufacturer, model string) string {
	return normName(manufacturer) + "|" + normName(model)
}

var riskClasses = map[string]bool{"": true, "low": true, "medium": true, "high": true}

func (ap *applier) checkModel(p *ModelDefined) error {
	if blank(p.Manufacturer) || blank(p.Model) {
		return invalid("manufacturer and model are required")
	}
	if blank(p.Kind) {
		return invalid("say what kind of equipment it is, e.g. infusion pump")
	}
	if !riskClasses[p.RiskClass] {
		return invalid("risk class must be low, medium or high, not %q", p.RiskClass)
	}
	for i, s := range p.Schedules {
		if !scheduleTypes[s.WOType] {
			return invalid("schedule %d: schedules generate pm, calibration or inspection work orders, not %q", i+1, s.WOType)
		}
		if blank(s.Title) {
			return invalid("schedule %d: a title is required", i+1)
		}
		if err := checkInterval(s.IntervalDays, s.GraceDays); err != nil {
			return invalid("schedule %d: %v", i+1, err)
		}
		if s.Checklist != "" {
			if _, err := NewestProcedure(ap.ctx, ap.tx, s.Checklist); errors.Is(err, ErrNotFound) {
				return invalid("schedule %d: no active checklist is called %q", i+1, s.Checklist)
			} else if err != nil {
				return err
			}
		}
	}
	return nil
}

func (ap *applier) modelDefined(p *ModelDefined) error {
	if err := ap.require(RoleMidTier); err != nil {
		return err
	}
	if err := ap.newEntity("models"); err != nil {
		return err
	}
	if err := ap.checkModel(p); err != nil {
		return err
	}
	key := ModelKey(p.Manufacturer, p.Model)
	if ok, err := ap.exists(`SELECT 1 FROM models WHERE match_key = ?`, key); err != nil || ok {
		return orConflict(err, "%s %s is already in the catalogue", p.Manufacturer, p.Model)
	}
	sched := schedulesJSON(p.Schedules)
	return ap.exec(`INSERT INTO models (id, match_key, manufacturer, model, kind, risk_class, notes, schedules, version, last_event_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?)`,
		ap.e.EntityID, key, p.Manufacturer, p.Model, p.Kind, p.RiskClass, p.Notes, sched, ap.e.EventID)
}

func (ap *applier) modelChanged(p *ModelChanged) error {
	if err := ap.require(RoleMidTier); err != nil {
		return err
	}
	m, err := GetModel(ap.ctx, ap.tx, ap.e.EntityID)
	if errors.Is(err, ErrNotFound) {
		return invalid("model %s not found", ap.e.EntityID)
	}
	if err != nil {
		return err
	}
	if ap.e.BaseVersion != 0 && ap.e.BaseVersion != m.Version {
		return store.Reject(FlagStaleBase, "%s %s was changed by someone else since version %d", m.Manufacturer, m.Model, ap.e.BaseVersion)
	}
	d := (*ModelDefined)(p)
	if err := ap.checkModel(d); err != nil {
		return err
	}
	key := ModelKey(p.Manufacturer, p.Model)
	if ok, err := ap.exists(`SELECT 1 FROM models WHERE match_key = ? AND id != ?`, key, m.ID); err != nil || ok {
		return orConflict(err, "%s %s is already in the catalogue", p.Manufacturer, p.Model)
	}
	sched := schedulesJSON(p.Schedules)
	return ap.exec(`UPDATE models SET match_key = ?, manufacturer = ?, model = ?, kind = ?, risk_class = ?, notes = ?, schedules = ?,
			version = version + 1, last_event_id = ? WHERE id = ?`,
		key, p.Manufacturer, p.Model, p.Kind, p.RiskClass, p.Notes, sched, ap.e.EventID, m.ID)
}

func schedulesJSON(s []ModelSchedule) string {
	if s == nil {
		s = []ModelSchedule{}
	}
	b, _ := json.Marshal(s)
	return string(b)
}

// fromModel allows a user who isn't mid-tier to set up a schedule that
// the catalogue says the equipment should have, exactly as it says.
func (ap *applier) fromModel(p *PMScheduleCreated, a Asset) error {
	m, err := GetModel(ap.ctx, ap.tx, p.FromModel)
	if errors.Is(err, ErrNotFound) {
		return invalid("model %s not found", p.FromModel)
	}
	if err != nil {
		return err
	}
	if m.Key != ModelKey(a.Manufacturer, a.Model) {
		return invalid("%s is not a %s %s", a.Tag, m.Manufacturer, m.Model)
	}
	if p.Meter != "" {
		return store.Reject(FlagNotAuthorized, "a usage trigger needs role %s", RoleMidTier)
	}
	for _, s := range m.Schedules {
		want := ""
		if s.Checklist != "" {
			if want, err = NewestProcedure(ap.ctx, ap.tx, s.Checklist); err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
		}
		if s.WOType == p.WOType && s.Title == p.Title && s.IntervalDays == p.IntervalDays && s.GraceDays == p.GraceDays && want == p.ProcedureID {
			return nil
		}
	}
	return store.Reject(FlagNotAuthorized, "this schedule differs from %s %s's in the catalogue, so it needs role %s", m.Manufacturer, m.Model, RoleMidTier)
}

// Model is a catalogued model.
type Model struct {
	ID, Key, Manufacturer, Model, Kind, RiskClass, Notes string
	Schedules                                            []ModelSchedule
	Version                                              int64
}

const modelColumns = `id, match_key, manufacturer, model, kind, risk_class, notes, schedules, version`

func scanModel(r interface{ Scan(...any) error }) (Model, error) {
	var m Model
	var sched string
	if err := r.Scan(&m.ID, &m.Key, &m.Manufacturer, &m.Model, &m.Kind, &m.RiskClass, &m.Notes, &sched, &m.Version); err != nil {
		return m, err
	}
	json.Unmarshal([]byte(sched), &m.Schedules)
	return m, nil
}

func GetModel(ctx context.Context, q Querier, id string) (Model, error) {
	m, err := scanModel(q.QueryRowContext(ctx, `SELECT `+modelColumns+` FROM models WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	return m, err
}

// ModelOf is the catalogued model of equipment with these names.
func ModelOf(ctx context.Context, q Querier, manufacturer, model string) (Model, error) {
	m, err := scanModel(q.QueryRowContext(ctx, `SELECT `+modelColumns+` FROM models WHERE match_key = ?`, ModelKey(manufacturer, model)))
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	return m, err
}

// Models lists the catalogue by manufacturer and model.
func Models(ctx context.Context, q Querier) ([]Model, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+modelColumns+` FROM models ORDER BY match_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Model
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// NewestProcedure is the id of the newest active version of the
// checklist called name.
func NewestProcedure(ctx context.Context, q Querier, name string) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, `SELECT id FROM procedures WHERE name = ? AND status = 'active' ORDER BY version DESC LIMIT 1`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}
