-- Calibration as checklist modules: a calibration point step records an
-- as-found reading (value) and, when adjusted, an as-left one; pass is the
-- as-left result and found_pass the as-found one. A reference standards
-- step records the standards used, with when each was due then.
ALTER TABLE checklist_results ADD COLUMN as_left TEXT NOT NULL DEFAULT '';
ALTER TABLE checklist_results ADD COLUMN found_pass INTEGER;
CREATE TABLE checklist_standards (
    wo_id              TEXT NOT NULL,
    procedure_id       TEXT NOT NULL,
    step_id            TEXT NOT NULL,
    standard_asset_id  TEXT NOT NULL,
    due_at_time_of_use TEXT NOT NULL,  -- '' when it had no calibration schedule
    PRIMARY KEY (wo_id, procedure_id, step_id, standard_asset_id)
) WITHOUT ROWID;
