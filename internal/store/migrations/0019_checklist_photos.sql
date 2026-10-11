-- A photo can belong to a checklist step on its work order.
ALTER TABLE attachments ADD COLUMN procedure_id TEXT NOT NULL DEFAULT '';
ALTER TABLE attachments ADD COLUMN step_id TEXT NOT NULL DEFAULT '';
