-- A pass/fail step can record the value that was measured to decide it.
ALTER TABLE checklist_results ADD COLUMN measured TEXT NOT NULL DEFAULT '';
