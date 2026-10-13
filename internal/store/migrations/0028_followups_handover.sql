-- A work order opened as a follow-up of another (same equipment), and the
-- department's acknowledgement that the equipment was handed back: the
-- name the person there typed, a note, when, and who recorded it.
ALTER TABLE work_orders ADD COLUMN follow_up_of TEXT NOT NULL DEFAULT '';
ALTER TABLE work_orders ADD COLUMN handover_name TEXT NOT NULL DEFAULT '';
ALTER TABLE work_orders ADD COLUMN handover_note TEXT NOT NULL DEFAULT '';
ALTER TABLE work_orders ADD COLUMN handover_at TEXT NOT NULL DEFAULT '';  -- RFC 3339 UTC
ALTER TABLE work_orders ADD COLUMN handover_by TEXT NOT NULL DEFAULT '';  -- user id of the person who recorded it
