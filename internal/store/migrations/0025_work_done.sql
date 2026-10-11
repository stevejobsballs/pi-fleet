-- A work order's description of the work done, and the test equipment
-- (reference standards) used to verify readings, with when each was due.
ALTER TABLE work_orders ADD COLUMN work_done TEXT NOT NULL DEFAULT '';
CREATE TABLE wo_test_equipment (
    wo_id              TEXT NOT NULL,
    asset_id           TEXT NOT NULL,
    due_at_time_of_use TEXT NOT NULL,  -- '' when it had no calibration schedule
    added_by           TEXT NOT NULL,
    added_at           TEXT NOT NULL,  -- RFC 3339 UTC
    event_id           TEXT NOT NULL,
    PRIMARY KEY (wo_id, asset_id)
) WITHOUT ROWID;
