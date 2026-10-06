-- Append-only event log (DESIGN.md §4.2).

CREATE TABLE events (
    chain_id         TEXT    NOT NULL,
    seq              INTEGER NOT NULL CHECK (seq >= 1),
    event_id         TEXT    NOT NULL UNIQUE,
    node_id          TEXT    NOT NULL,
    prev_hash        BLOB    NOT NULL CHECK (length(prev_hash) = 32),
    hlc              INTEGER NOT NULL,
    wall_time        TEXT    NOT NULL,
    clock_state      TEXT    NOT NULL CHECK (clock_state IN ('verified', 'unverified')),
    actor_user_id    TEXT    NOT NULL,
    actor_session_id TEXT    NOT NULL,
    type             TEXT    NOT NULL,
    entity_type      TEXT    NOT NULL,
    entity_id        TEXT    NOT NULL,
    base_version     INTEGER NOT NULL,
    lease_id         TEXT    NOT NULL,
    schema_version   INTEGER NOT NULL,
    payload          BLOB,
    payload_hash     BLOB    NOT NULL CHECK (length(payload_hash) = 32),
    hash             BLOB    NOT NULL UNIQUE CHECK (length(hash) = 32),
    sig              BLOB    NOT NULL CHECK (length(sig) = 64),
    key_id           TEXT    NOT NULL,
    PRIMARY KEY (chain_id, seq)
) WITHOUT ROWID;

CREATE INDEX events_entity ON events (entity_type, entity_id);
CREATE INDEX events_node ON events (node_id);

CREATE TRIGGER events_no_delete BEFORE DELETE ON events
BEGIN
    SELECT RAISE(ABORT, 'events are append-only');
END;

-- The only permitted update is clearing a payload that a payload.redacted
-- event already names (DESIGN.md §3.6). Every other column must be unchanged.
CREATE TRIGGER events_no_update BEFORE UPDATE ON events
WHEN NOT (
        OLD.payload IS NOT NULL AND NEW.payload IS NULL
    AND NEW.chain_id IS OLD.chain_id AND NEW.seq IS OLD.seq
    AND NEW.event_id IS OLD.event_id AND NEW.node_id IS OLD.node_id
    AND NEW.prev_hash IS OLD.prev_hash AND NEW.hlc IS OLD.hlc
    AND NEW.wall_time IS OLD.wall_time AND NEW.clock_state IS OLD.clock_state
    AND NEW.actor_user_id IS OLD.actor_user_id
    AND NEW.actor_session_id IS OLD.actor_session_id
    AND NEW.type IS OLD.type AND NEW.entity_type IS OLD.entity_type
    AND NEW.entity_id IS OLD.entity_id AND NEW.base_version IS OLD.base_version
    AND NEW.lease_id IS OLD.lease_id AND NEW.schema_version IS OLD.schema_version
    AND NEW.payload_hash IS OLD.payload_hash AND NEW.hash IS OLD.hash
    AND NEW.sig IS OLD.sig AND NEW.key_id IS OLD.key_id
    AND EXISTS (
        SELECT 1 FROM events r
        WHERE r.type = 'payload.redacted'
          AND r.entity_type = 'event'
          AND r.entity_id = OLD.event_id
    )
)
BEGIN
    SELECT RAISE(ABORT, 'events are append-only');
END;

-- Public keys trusted to sign events, per node (DESIGN.md §6).
CREATE TABLE node_keys (
    node_id    TEXT NOT NULL,
    key_id     TEXT NOT NULL,
    public_key BLOB NOT NULL CHECK (length(public_key) = 32),
    added_at   TEXT NOT NULL,
    PRIMARY KEY (node_id, key_id)
) WITHOUT ROWID;

-- This installation's own identity and current chain.
CREATE TABLE local_node (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    node_id   TEXT NOT NULL,
    chain_id  TEXT NOT NULL
);
