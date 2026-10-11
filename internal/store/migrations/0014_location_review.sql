-- Locations proposed by employees while registering equipment wait for a
-- super user's review. Locations made before this, and by super users,
-- are approved.

ALTER TABLE locations ADD COLUMN status TEXT NOT NULL DEFAULT 'approved' CHECK (status IN ('approved', 'pending', 'rejected'));
ALTER TABLE locations ADD COLUMN details TEXT NOT NULL DEFAULT '{}';  -- JSON LocationDetails
ALTER TABLE locations ADD COLUMN proposed_by TEXT NOT NULL DEFAULT '';
ALTER TABLE locations ADD COLUMN proposed_at TEXT NOT NULL DEFAULT '';  -- RFC 3339 UTC
ALTER TABLE locations ADD COLUMN reviewed_by TEXT NOT NULL DEFAULT '';
ALTER TABLE locations ADD COLUMN review_note TEXT NOT NULL DEFAULT '';
CREATE INDEX locations_status ON locations (status);
