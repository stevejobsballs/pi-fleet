-- Meter readings and meter-triggered schedules (DESIGN.md §4.3).

CREATE TABLE meter_readings (
    id            TEXT PRIMARY KEY,
    asset_id      TEXT NOT NULL REFERENCES assets (id),
    meter         TEXT NOT NULL,
    value         TEXT NOT NULL,  -- decimal, as read
    read_at       TEXT NOT NULL,  -- RFC 3339 UTC, when it was read
    reset         INTEGER NOT NULL CHECK (reset IN (0, 1)),  -- meter replaced or reset before this reading
    total         TEXT NOT NULL,  -- cumulative usage up to this reading (derived)
    recorded_by   TEXT NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('recorded', 'voided')),
    void_reason   TEXT NOT NULL,
    last_event_id TEXT NOT NULL
);
CREATE INDEX meter_readings_asset ON meter_readings (asset_id, meter, read_at);

-- A schedule may also trigger on usage: whichever of the calendar
-- interval and the meter interval comes first.
ALTER TABLE pm_schedules ADD COLUMN meter TEXT NOT NULL DEFAULT '';
ALTER TABLE pm_schedules ADD COLUMN meter_interval TEXT NOT NULL DEFAULT '';
ALTER TABLE pm_schedules ADD COLUMN meter_lead TEXT NOT NULL DEFAULT '';
ALTER TABLE pm_schedules ADD COLUMN meter_baseline TEXT NOT NULL DEFAULT '';
