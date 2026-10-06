-- Projections derived from events (DESIGN.md §4.3). Every table here can
-- be emptied and rebuilt from the event log; none is a source of truth.
-- `version` counts the events applied to a row; `last_event_id` traces it
-- back to its history.

CREATE TABLE users (
    id                    TEXT PRIMARY KEY,
    username              TEXT NOT NULL UNIQUE,
    legal_name            TEXT NOT NULL,
    email                 TEXT NOT NULL,
    role                  TEXT NOT NULL CHECK (role IN ('user', 'mid_tier', 'super_user')),
    status                TEXT NOT NULL CHECK (status IN ('pending_activation', 'active', 'disabled')),
    home_sites            TEXT NOT NULL,  -- JSON array of site ids
    created_by            TEXT NOT NULL,
    identity_verified_by  TEXT NOT NULL,
    identity_verification TEXT NOT NULL,
    verifier              TEXT NOT NULL,  -- PHC-encoded Argon2id
    must_change_password  INTEGER NOT NULL CHECK (must_change_password IN (0, 1)),
    password_changed_at   TEXT NOT NULL,
    password_expires_at   TEXT NOT NULL,
    version               INTEGER NOT NULL,
    last_event_id         TEXT NOT NULL
);

-- Previous verifiers, newest first, for the reuse rule (DESIGN.md §6.5).
CREATE TABLE user_password_history (
    user_id    TEXT NOT NULL,
    event_id   TEXT NOT NULL,
    verifier   TEXT NOT NULL,
    changed_at TEXT NOT NULL,
    PRIMARY KEY (user_id, event_id)
) WITHOUT ROWID;

CREATE TABLE sites (
    id            TEXT PRIMARY KEY,
    code          TEXT NOT NULL UNIQUE,
    name          TEXT NOT NULL,
    timezone      TEXT NOT NULL,
    version       INTEGER NOT NULL,
    last_event_id TEXT NOT NULL
);

CREATE TABLE locations (
    id            TEXT PRIMARY KEY,
    site_id       TEXT NOT NULL REFERENCES sites (id),
    parent_id     TEXT REFERENCES locations (id),
    name          TEXT NOT NULL,
    kind          TEXT NOT NULL,
    version       INTEGER NOT NULL,
    last_event_id TEXT NOT NULL
);
CREATE INDEX locations_site ON locations (site_id);

CREATE TABLE assets (
    id                    TEXT PRIMARY KEY,
    tag                   TEXT NOT NULL UNIQUE,
    site_id               TEXT NOT NULL REFERENCES sites (id),
    location_id           TEXT NOT NULL REFERENCES locations (id),
    manufacturer          TEXT NOT NULL,
    model                 TEXT NOT NULL,
    serial                TEXT NOT NULL,
    status                TEXT NOT NULL CHECK (status IN ('in_service', 'out_of_service', 'missing', 'retired')),
    risk_class            TEXT NOT NULL,
    is_reference_standard INTEGER NOT NULL CHECK (is_reference_standard IN (0, 1)),
    custom_fields         TEXT NOT NULL,  -- JSON object of strings
    field_versions        TEXT NOT NULL,  -- JSON: field -> version that last changed it
    version               INTEGER NOT NULL,
    last_event_id         TEXT NOT NULL
);
CREATE INDEX assets_site ON assets (site_id);
CREATE INDEX assets_serial ON assets (manufacturer, model, serial);

CREATE TABLE work_orders (
    id            TEXT PRIMARY KEY,
    number        TEXT NOT NULL UNIQUE,
    type          TEXT NOT NULL CHECK (type IN ('pm', 'corrective', 'calibration', 'inspection', 'install', 'retire')),
    asset_id      TEXT NOT NULL REFERENCES assets (id),
    priority      TEXT NOT NULL CHECK (priority IN ('low', 'normal', 'high', 'urgent')),
    status        TEXT NOT NULL CHECK (status IN ('open', 'assigned', 'in_progress', 'on_hold', 'completed', 'reviewed', 'closed', 'cancelled')),
    title         TEXT NOT NULL,
    problem       TEXT NOT NULL,
    due_at        TEXT NOT NULL,  -- empty when none
    opened_by     TEXT NOT NULL,
    assigned_to   TEXT NOT NULL,  -- empty when unassigned
    lease_id      TEXT NOT NULL,  -- active lease, empty when none
    version       INTEGER NOT NULL,
    last_event_id TEXT NOT NULL
);
CREATE INDEX work_orders_asset ON work_orders (asset_id);
CREATE INDEX work_orders_assigned ON work_orders (assigned_to, status);

-- Every lease ever granted, so events made under a lease that has since
-- ended can get offline grace (DESIGN.md §5.4).
CREATE TABLE wo_leases (
    lease_id      TEXT PRIMARY KEY,
    wo_id         TEXT NOT NULL REFERENCES work_orders (id),
    user_id       TEXT NOT NULL,
    granted_event TEXT NOT NULL,
    ended_hlc     INTEGER  -- NULL while active
);
CREATE INDEX wo_leases_wo ON wo_leases (wo_id);
