-- Decisions on flagged records (DESIGN.md §5.5).
CREATE TABLE flag_resolutions (
    event_id      TEXT PRIMARY KEY,  -- the flagged event
    resolution    TEXT NOT NULL CHECK (resolution IN ('acknowledged', 'accepted', 'rejected')),
    note          TEXT NOT NULL,
    resolved_by   TEXT NOT NULL,
    resolved_at   TEXT NOT NULL,
    last_event_id TEXT NOT NULL
);
