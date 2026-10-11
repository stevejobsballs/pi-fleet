-- The make/model catalogue: each model's device type, default risk class
-- and default service schedules, applied when its equipment is registered.
CREATE TABLE models (
    id            TEXT PRIMARY KEY,
    match_key     TEXT NOT NULL UNIQUE,  -- manufacturer and model, normalised, for matching equipment
    manufacturer  TEXT NOT NULL,
    model         TEXT NOT NULL,
    kind          TEXT NOT NULL,         -- what it is, e.g. "Infusion pump"
    risk_class    TEXT NOT NULL,
    notes         TEXT NOT NULL,
    schedules     TEXT NOT NULL,         -- JSON array of ModelSchedule
    version       INTEGER NOT NULL,
    last_event_id TEXT NOT NULL
);
