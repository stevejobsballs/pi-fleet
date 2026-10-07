-- Checklists (versioned procedures) and labour time (DESIGN.md §4.3).

CREATE TABLE procedures (
    id            TEXT PRIMARY KEY,   -- one row per published version
    name          TEXT NOT NULL,
    version       INTEGER NOT NULL,
    steps         TEXT NOT NULL,      -- JSON array of steps
    status        TEXT NOT NULL CHECK (status IN ('active', 'retired')),
    published_by  TEXT NOT NULL,
    published_at  TEXT NOT NULL,
    last_event_id TEXT NOT NULL,
    UNIQUE (name, version)
);

ALTER TABLE work_orders ADD COLUMN procedure_id TEXT NOT NULL DEFAULT '';
ALTER TABLE pm_schedules ADD COLUMN procedure_id TEXT NOT NULL DEFAULT '';

-- The latest result of each checklist step; earlier ones are in the events.
CREATE TABLE checklist_results (
    wo_id       TEXT NOT NULL REFERENCES work_orders (id),
    step_id     TEXT NOT NULL,
    value       TEXT NOT NULL,
    pass        INTEGER,            -- NULL when the step has no pass/fail
    note        TEXT NOT NULL,
    recorded_by TEXT NOT NULL,
    recorded_at TEXT NOT NULL,
    event_id    TEXT NOT NULL,
    PRIMARY KEY (wo_id, step_id)
) WITHOUT ROWID;

CREATE TABLE labor_entries (
    id             TEXT PRIMARY KEY,
    wo_id          TEXT NOT NULL REFERENCES work_orders (id),
    user_id        TEXT NOT NULL,
    minutes        INTEGER NOT NULL CHECK (minutes > 0),
    work_date      TEXT NOT NULL,
    note           TEXT NOT NULL,
    status         TEXT NOT NULL CHECK (status IN ('logged', 'reversed')),
    reverse_reason TEXT NOT NULL,
    last_event_id  TEXT NOT NULL
);
CREATE INDEX labor_entries_wo ON labor_entries (wo_id);
