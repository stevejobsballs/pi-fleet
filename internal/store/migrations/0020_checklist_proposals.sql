-- Checklists built by people who can't publish them: they wait for a
-- mid-tier or super user, who approves (publishing the next version) or
-- rejects them with a reason.
CREATE TABLE procedure_proposals (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    description   TEXT NOT NULL,
    reference     TEXT NOT NULL,
    category      TEXT NOT NULL,
    applies_to    TEXT NOT NULL,   -- JSON, as procedures.applies_to
    steps         TEXT NOT NULL,   -- JSON, as procedures.steps
    based_on      TEXT NOT NULL,   -- the version it was started from, if any
    proposed_by   TEXT NOT NULL,
    proposed_at   TEXT NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('pending', 'approved', 'rejected')),
    reviewed_by   TEXT NOT NULL DEFAULT '',
    review_note   TEXT NOT NULL DEFAULT '',
    published_id  TEXT NOT NULL DEFAULT '',  -- the version approval published
    last_event_id TEXT NOT NULL
);
CREATE INDEX procedure_proposals_status ON procedure_proposals (status, proposed_at);
