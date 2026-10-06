-- Nodes, sync state, and node-side retention (DESIGN.md §5–6).

-- Projection: every node central has heard of (DESIGN.md §4.3 `node`).
CREATE TABLE nodes (
    id            TEXT PRIMARY KEY,
    mode          TEXT NOT NULL CHECK (mode IN ('personal', 'kiosk')),
    bound_user_id TEXT NOT NULL,
    event_pub     BLOB NOT NULL CHECK (length(event_pub) = 32),
    transport_pub BLOB NOT NULL CHECK (length(transport_pub) = 32),
    transport_key_id TEXT NOT NULL UNIQUE,
    pairing_words TEXT NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('pending_confirmation', 'active', 'rejected', 'revoked')),
    activated_at  TEXT NOT NULL,
    confirmed_by  TEXT NOT NULL,
    pending_verifier TEXT NOT NULL,  -- the user's chosen password verifier, applied on confirmation
    revoked_hlc   INTEGER,           -- events at or after this HLC are refused once revoked
    keep_unsynced INTEGER NOT NULL DEFAULT 0,  -- accept a revoked node's earlier events
    version       INTEGER NOT NULL,
    last_event_id TEXT NOT NULL
);
CREATE INDEX nodes_user ON nodes (bound_user_id, status);

-- Local settings (not replicated): role, central URL and keys, clock state.
CREATE TABLE node_config (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Node only: the last working-set snapshot received from central.
CREATE TABLE ws_snapshot (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    body      BLOB NOT NULL,
    sig       BLOB NOT NULL,
    received_at TEXT NOT NULL
);

-- Node only: retention checkpoints (DESIGN.md §5.7). Events at or below a
-- chain's checkpoint may be purged; the checkpoint keeps the chain
-- verifiable from that point.
CREATE TABLE chain_checkpoints (
    chain_id TEXT PRIMARY KEY,
    seq      INTEGER NOT NULL,
    hash     BLOB NOT NULL CHECK (length(hash) = 32)
);

-- Single-use activation challenges (central).
CREATE TABLE activation_nonces (
    nonce      TEXT PRIMARY KEY,
    username   TEXT NOT NULL,
    expires_at TEXT NOT NULL
);

-- Seen request-signature nonces, for replay protection (central).
CREATE TABLE request_nonces (
    nonce      TEXT PRIMARY KEY,
    expires_at TEXT NOT NULL
);

DROP TRIGGER events_no_delete;
CREATE TRIGGER events_no_delete BEFORE DELETE ON events
WHEN NOT EXISTS (
    SELECT 1 FROM chain_checkpoints c
    WHERE c.chain_id = OLD.chain_id AND OLD.seq <= c.seq
)
BEGIN
    SELECT RAISE(ABORT, 'events are append-only');
END;
