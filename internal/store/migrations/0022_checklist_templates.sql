-- Generic templates and the custom checklists configured from them. Every
-- checklist made before this is a generic template.
ALTER TABLE procedures ADD COLUMN custom INTEGER NOT NULL DEFAULT 0 CHECK (custom IN (0, 1));
ALTER TABLE procedures ADD COLUMN template_id TEXT NOT NULL DEFAULT '';  -- the template a custom checklist came from
ALTER TABLE procedure_proposals ADD COLUMN custom INTEGER NOT NULL DEFAULT 0 CHECK (custom IN (0, 1));
ALTER TABLE procedure_proposals ADD COLUMN template_id TEXT NOT NULL DEFAULT '';
