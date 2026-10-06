-- Backup records (DESIGN.md §8). Projections of backup.* events.

CREATE TABLE backups (
    id           TEXT PRIMARY KEY,   -- entity id of the backup record
    kind         TEXT NOT NULL CHECK (kind IN ('snapshot', 'offsite')),
    disk_id      TEXT NOT NULL,      -- offsite only
    disk_label   TEXT NOT NULL,
    file         TEXT NOT NULL,
    sha256       TEXT NOT NULL,      -- of the encrypted file
    local_order  INTEGER NOT NULL,   -- events up to here are in the backup
    heads        TEXT NOT NULL,      -- JSON: chain id -> highest seq included
    written_at   TEXT NOT NULL,
    confirmed_at TEXT NOT NULL,      -- offsite: when a person confirmed it left the building
    confirmed_by TEXT NOT NULL,
    last_event_id TEXT NOT NULL
);

-- Per chain, the highest seq held in a verified backup that is confirmed
-- off-site: the durable watermark nodes may purge below (§5.7, §8.2).
CREATE TABLE durable_heads (
    chain_id TEXT PRIMARY KEY,
    seq      INTEGER NOT NULL
);
