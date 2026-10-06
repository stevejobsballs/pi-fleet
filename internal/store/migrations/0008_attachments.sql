-- Attachments (DESIGN.md §5.10). Files live in the blob store by hash.
CREATE TABLE attachments (
    id            TEXT PRIMARY KEY,
    sha256        TEXT NOT NULL,
    mime          TEXT NOT NULL,
    size          INTEGER NOT NULL,
    filename      TEXT NOT NULL,
    description   TEXT NOT NULL,
    target_type   TEXT NOT NULL CHECK (target_type IN ('work_order', 'asset')),
    target_id     TEXT NOT NULL,
    added_by      TEXT NOT NULL,
    added_at      TEXT NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('attached', 'detached', 'purged')),
    detach_reason TEXT NOT NULL,
    version       INTEGER NOT NULL,
    last_event_id TEXT NOT NULL
);
CREATE INDEX attachments_target ON attachments (target_type, target_id);
CREATE INDEX attachments_sha ON attachments (sha256);

-- Node only: files added here that central doesn't have yet.
CREATE TABLE blob_uploads (
    sha256   TEXT PRIMARY KEY,
    added_at TEXT NOT NULL
);
