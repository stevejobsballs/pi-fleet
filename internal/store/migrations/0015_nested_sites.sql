-- Sites inside sites: a satellite clinic inside its hospital, at any depth.
-- Equipment at a satellite also belongs to every site above it.
--   path:    the site codes from the top, e.g. "MAIN › NORTH"
--   lineage: the site ids from the top, e.g. "/<main id>/<north id>/",
--            so "this site and everything inside it" is a LIKE match.

ALTER TABLE sites ADD COLUMN parent_id TEXT REFERENCES sites (id);
ALTER TABLE sites ADD COLUMN path TEXT NOT NULL DEFAULT '';
ALTER TABLE sites ADD COLUMN lineage TEXT NOT NULL DEFAULT '';
UPDATE sites SET path = code, lineage = '/' || id || '/';
CREATE INDEX sites_parent ON sites (parent_id);
