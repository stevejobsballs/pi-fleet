-- Problems reported by anyone on the hospital network, usually by scanning
-- the QR label on a piece of equipment. Kept on the master Pi only: someone
-- there turns each into a work order or closes it.

CREATE TABLE service_requests (
    id                   TEXT PRIMARY KEY,
    number               TEXT NOT NULL UNIQUE,  -- R-00001, for the person who reported it
    asset_id             TEXT NOT NULL REFERENCES assets (id),
    category             TEXT NOT NULL,
    description          TEXT NOT NULL,
    requester_name       TEXT NOT NULL,
    requester_department TEXT NOT NULL,
    requester_phone      TEXT NOT NULL,
    submitted_at         TEXT NOT NULL,         -- RFC 3339 UTC
    status               TEXT NOT NULL CHECK (status IN ('new', 'converted', 'closed')),
    work_order_id        TEXT NOT NULL DEFAULT '',
    closed_reason        TEXT NOT NULL DEFAULT '',
    handled_by           TEXT NOT NULL DEFAULT '',
    version              INTEGER NOT NULL,
    last_event_id        TEXT NOT NULL
);
CREATE INDEX service_requests_status ON service_requests (status, submitted_at);
CREATE INDEX service_requests_asset ON service_requests (asset_id);
