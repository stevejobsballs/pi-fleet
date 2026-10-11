-- What a checklist is for: shown to whoever chooses or follows it.
ALTER TABLE procedures ADD COLUMN description TEXT NOT NULL DEFAULT '';
ALTER TABLE procedures ADD COLUMN reference TEXT NOT NULL DEFAULT '';  -- the standard or manual it follows
ALTER TABLE procedures ADD COLUMN category TEXT NOT NULL DEFAULT '';
