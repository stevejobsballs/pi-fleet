-- Whether a merged record's service history joins the kept record's
-- timeline (the user chooses when merging), and when each work order was
-- opened, so a timeline can be put in date order.

ALTER TABLE assets ADD COLUMN history_integrated INTEGER NOT NULL DEFAULT 0 CHECK (history_integrated IN (0, 1));
-- Merges made before the choice existed always showed both histories.
UPDATE assets SET history_integrated = 1 WHERE merged_into != '';

ALTER TABLE work_orders ADD COLUMN opened_at TEXT NOT NULL DEFAULT '';  -- RFC 3339 UTC
UPDATE work_orders SET opened_at = coalesce((SELECT substr(e.wall_time, 1, 19) || 'Z' FROM events e
    WHERE e.entity_id = work_orders.id AND e.type = 'workorder.opened' ORDER BY e.local_order LIMIT 1), '');
CREATE INDEX work_orders_asset_opened ON work_orders (asset_id, opened_at);
