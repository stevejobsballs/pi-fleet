-- Time on a work order is labour or travel. Time recorded with the clock
-- (Start work / Stop work) keeps when it started and ended; a running
-- clock is kept in labor_clocks until it is stopped (one per person).
ALTER TABLE labor_entries ADD COLUMN kind TEXT NOT NULL DEFAULT 'labor';
ALTER TABLE labor_entries ADD COLUMN started_at TEXT NOT NULL DEFAULT '';  -- RFC 3339 UTC, '' when typed in
ALTER TABLE labor_entries ADD COLUMN ended_at TEXT NOT NULL DEFAULT '';
CREATE TABLE labor_clocks (
    id         TEXT PRIMARY KEY,  -- becomes the labour entry's id
    wo_id      TEXT NOT NULL,
    user_id    TEXT NOT NULL UNIQUE,
    kind       TEXT NOT NULL,
    started_at TEXT NOT NULL,     -- RFC 3339 UTC
    event_id   TEXT NOT NULL
);
