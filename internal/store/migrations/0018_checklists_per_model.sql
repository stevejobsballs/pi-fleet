-- Which equipment a checklist is for, and several checklists per work order.

-- JSON list of {"manufacturer", "model"}; an empty model means every model
-- of that manufacturer, an empty list any equipment.
ALTER TABLE procedures ADD COLUMN applies_to TEXT NOT NULL DEFAULT '[]';

-- Checklists added to a work order besides its first (work_orders.procedure_id).
CREATE TABLE work_order_checklists (
    wo_id         TEXT NOT NULL REFERENCES work_orders (id),
    procedure_id  TEXT NOT NULL REFERENCES procedures (id),
    added_by      TEXT NOT NULL,
    added_at      TEXT NOT NULL,
    last_event_id TEXT NOT NULL,
    PRIMARY KEY (wo_id, procedure_id)
) WITHOUT ROWID;

-- Answers are now kept per checklist, as two checklists may share step ids.
CREATE TABLE checklist_results_new (
    wo_id        TEXT NOT NULL REFERENCES work_orders (id),
    procedure_id TEXT NOT NULL,
    step_id      TEXT NOT NULL,
    value        TEXT NOT NULL,
    pass         INTEGER,
    note         TEXT NOT NULL,
    recorded_by  TEXT NOT NULL,
    recorded_at  TEXT NOT NULL,
    event_id     TEXT NOT NULL,
    PRIMARY KEY (wo_id, procedure_id, step_id)
) WITHOUT ROWID;
INSERT INTO checklist_results_new (wo_id, procedure_id, step_id, value, pass, note, recorded_by, recorded_at, event_id)
    SELECT r.wo_id, w.procedure_id, r.step_id, r.value, r.pass, r.note, r.recorded_by, r.recorded_at, r.event_id
    FROM checklist_results r JOIN work_orders w ON w.id = r.wo_id;
DROP TABLE checklist_results;
ALTER TABLE checklist_results_new RENAME TO checklist_results;
