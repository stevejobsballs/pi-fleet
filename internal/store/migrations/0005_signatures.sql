-- Part 11 electronic signatures (DESIGN.md §7) and web sessions.

-- Projection: one row per signature.applied event.
CREATE TABLE signatures (
    id                TEXT PRIMARY KEY,  -- the signature.applied event id
    target_type       TEXT NOT NULL,
    target_id         TEXT NOT NULL,
    meaning           TEXT NOT NULL CHECK (meaning IN ('performed', 'reviewed', 'approved')),
    signer_user_id    TEXT NOT NULL,
    signer_legal_name TEXT NOT NULL,     -- as it was when signed (§11.50)
    signer_username   TEXT NOT NULL,
    signed_at         TEXT NOT NULL,     -- RFC 3339 UTC, the signing node's clock
    clock_state       TEXT NOT NULL,
    content_hash      TEXT NOT NULL,     -- hash of the signed record (§11.70)
    sign_round        INTEGER NOT NULL,  -- work order's signing round when signed
    status            TEXT NOT NULL CHECK (status IN ('valid', 'withdrawn')),
    withdraw_reason   TEXT NOT NULL
);
CREATE INDEX signatures_target ON signatures (target_type, target_id);

-- Incremented when a work order is reopened, so earlier signatures no
-- longer count towards completion, review or approval.
ALTER TABLE work_orders ADD COLUMN sign_round INTEGER NOT NULL DEFAULT 0;

-- Local web sessions (not replicated). The id is stored hashed.
CREATE TABLE sessions (
    id_hash       TEXT PRIMARY KEY,
    session_id    TEXT NOT NULL UNIQUE,  -- recorded on events as actor_session_id
    user_id       TEXT NOT NULL,
    csrf_token    TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    last_seen_at  TEXT NOT NULL,
    password_only INTEGER NOT NULL  -- 1: may only change the password
);
