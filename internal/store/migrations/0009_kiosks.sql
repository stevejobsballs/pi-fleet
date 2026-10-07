-- Kiosk mode (DESIGN.md §6.3): a shared Pi bound to a group of users.
CREATE TABLE kiosks (
    id                    TEXT PRIMARY KEY,
    site_id               TEXT NOT NULL REFERENCES sites (id),
    name                  TEXT NOT NULL UNIQUE,
    activation_verifier   TEXT NOT NULL,  -- one-time activation password; empty once used
    activation_expires_at TEXT NOT NULL,
    version               INTEGER NOT NULL,
    last_event_id         TEXT NOT NULL
);

-- Members, with the HLC of removal so work done on the kiosk before a
-- removal still counts when it syncs later (offline grace).
CREATE TABLE kiosk_members (
    kiosk_id    TEXT NOT NULL REFERENCES kiosks (id),
    user_id     TEXT NOT NULL,
    removed_hlc INTEGER,
    PRIMARY KEY (kiosk_id, user_id)
) WITHOUT ROWID;

ALTER TABLE nodes ADD COLUMN kiosk_id TEXT NOT NULL DEFAULT '';
