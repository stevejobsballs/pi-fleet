-- Calibration records, PM schedules, inventory and account lockout
-- (DESIGN.md §4.3, §6.5). All projections except login_failures.

ALTER TABLE users ADD COLUMN locked_until TEXT NOT NULL DEFAULT '';  -- RFC 3339; empty when not locked

CREATE TABLE user_lockouts (
    user_id  TEXT NOT NULL,
    event_id TEXT NOT NULL,
    at       TEXT NOT NULL,
    PRIMARY KEY (user_id, event_id)
) WITHOUT ROWID;

-- Local operational state, not derived from events and not replicated:
-- failed logins on this device since the last success or lockout.
CREATE TABLE login_failures (
    username        TEXT PRIMARY KEY,
    failures        INTEGER NOT NULL,
    last_failure_at TEXT NOT NULL
);

CREATE TABLE pm_schedules (
    id            TEXT PRIMARY KEY,
    asset_id      TEXT NOT NULL REFERENCES assets (id),
    wo_type       TEXT NOT NULL CHECK (wo_type IN ('pm', 'calibration', 'inspection')),
    title         TEXT NOT NULL,
    procedure     TEXT NOT NULL,
    interval_days INTEGER NOT NULL CHECK (interval_days > 0),
    grace_days    INTEGER NOT NULL CHECK (grace_days >= 0),
    next_due      TEXT NOT NULL,  -- YYYY-MM-DD in the asset's site time zone
    status        TEXT NOT NULL CHECK (status IN ('active', 'ended')),
    open_wo_id    TEXT NOT NULL,  -- generated work order not yet completed; empty when none
    version       INTEGER NOT NULL,
    last_event_id TEXT NOT NULL
);
CREATE INDEX pm_schedules_due ON pm_schedules (status, next_due);
CREATE INDEX pm_schedules_asset ON pm_schedules (asset_id);

ALTER TABLE work_orders ADD COLUMN schedule_id TEXT NOT NULL DEFAULT '';

CREATE TABLE calibration_records (
    id                     TEXT PRIMARY KEY,
    wo_id                  TEXT NOT NULL REFERENCES work_orders (id),
    asset_id               TEXT NOT NULL REFERENCES assets (id),
    procedure              TEXT NOT NULL,
    performed_by           TEXT NOT NULL,
    performed_at           TEXT NOT NULL,
    temperature            TEXT NOT NULL,
    humidity               TEXT NOT NULL,
    adjusted               INTEGER NOT NULL CHECK (adjusted IN (0, 1)),
    as_found_result        TEXT NOT NULL CHECK (as_found_result IN ('pass', 'fail')),  -- computed here
    as_left_result         TEXT NOT NULL CHECK (as_left_result IN ('pass', 'fail')),   -- computed here
    node_as_found_result   TEXT NOT NULL,  -- as stated by the authoring node
    node_as_left_result    TEXT NOT NULL,
    certificate_sha256     TEXT NOT NULL,
    status                 TEXT NOT NULL CHECK (status IN ('valid', 'voided')),
    void_reason            TEXT NOT NULL,
    version                INTEGER NOT NULL,
    last_event_id          TEXT NOT NULL
);
CREATE INDEX calibration_records_wo ON calibration_records (wo_id);
CREATE INDEX calibration_records_asset ON calibration_records (asset_id);

CREATE TABLE cal_points (
    record_id      TEXT NOT NULL REFERENCES calibration_records (id),
    idx            INTEGER NOT NULL,
    parameter      TEXT NOT NULL,
    unit           TEXT NOT NULL,
    nominal        TEXT NOT NULL,
    tolerance      TEXT NOT NULL,  -- JSON
    lower_limit    TEXT NOT NULL,  -- computed acceptance limits
    upper_limit    TEXT NOT NULL,
    as_found       TEXT NOT NULL,
    as_left        TEXT NOT NULL,
    as_found_pass  INTEGER NOT NULL,
    as_left_pass   INTEGER NOT NULL,
    PRIMARY KEY (record_id, idx)
) WITHOUT ROWID;

-- Reference standards used, with their due date as it stood at the time
-- of use (DESIGN.md §4.3 traceability).
CREATE TABLE cal_standards (
    record_id          TEXT NOT NULL REFERENCES calibration_records (id),
    standard_asset_id  TEXT NOT NULL REFERENCES assets (id),
    due_at_time_of_use TEXT NOT NULL,  -- empty when untracked
    PRIMARY KEY (record_id, standard_asset_id)
) WITHOUT ROWID;

CREATE TABLE parts (
    id            TEXT PRIMARY KEY,
    part_no       TEXT NOT NULL UNIQUE,
    description   TEXT NOT NULL,
    unit          TEXT NOT NULL,
    version       INTEGER NOT NULL,
    last_event_id TEXT NOT NULL
);

CREATE TABLE stock_locations (
    id            TEXT PRIMARY KEY,
    site_id       TEXT NOT NULL REFERENCES sites (id),
    name          TEXT NOT NULL,
    owner_user_id TEXT NOT NULL,  -- empty for a shared stockroom
    version       INTEGER NOT NULL,
    last_event_id TEXT NOT NULL
);

-- Ledger rows. A transfer writes two rows with the same txn_id.
CREATE TABLE stock_txns (
    txn_id       TEXT NOT NULL,
    location_id  TEXT NOT NULL REFERENCES stock_locations (id),
    part_id      TEXT NOT NULL REFERENCES parts (id),
    delta        INTEGER NOT NULL,
    kind         TEXT NOT NULL,
    wo_id        TEXT NOT NULL,
    reason       TEXT NOT NULL,
    observed_qty INTEGER,  -- counts only
    computed_qty INTEGER,  -- counts only: ledger quantity when counted
    event_id     TEXT NOT NULL,
    reversed_by  TEXT NOT NULL,  -- reversal event id; empty when not reversed
    PRIMARY KEY (txn_id, location_id)
) WITHOUT ROWID;

CREATE TABLE stock_levels (
    part_id     TEXT NOT NULL,
    location_id TEXT NOT NULL,
    qty         INTEGER NOT NULL,
    PRIMARY KEY (part_id, location_id)
) WITHOUT ROWID;
