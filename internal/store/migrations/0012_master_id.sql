-- MasterID and merging duplicate equipment records (DESIGN.md §4.2, §5.5).
-- Records with the same MasterID are the same equipment. A merge keeps
-- one record and points the other at it; nothing already recorded
-- against the merged record is rewritten, so its signatures stay valid.

ALTER TABLE assets ADD COLUMN master_id TEXT NOT NULL DEFAULT '';
ALTER TABLE assets ADD COLUMN merged_into TEXT NOT NULL DEFAULT '';  -- id of the record kept, or ''
CREATE INDEX assets_master_id ON assets (master_id) WHERE master_id != '';
CREATE INDEX assets_merged_into ON assets (merged_into) WHERE merged_into != '';
