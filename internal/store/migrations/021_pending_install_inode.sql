-- The inode of the subtitle file a pending install publishes (the staged
-- file, which linking or renaming keeps). Startup undoes a publication only
-- while the destination is still that file. 0 (rows saved before this
-- migration) falls back to comparing the checksum alone.
ALTER TABLE pending_installs ADD COLUMN published_inode INTEGER NOT NULL DEFAULT 0;

-- When a delete event marked the media row deleted. A late import or rename
-- that happened before it does not revive the row. updated_at_ns cannot
-- serve, because inventory and other writes also move it. Rows deleted
-- before this migration take their last update time, the best record left.
ALTER TABLE media ADD COLUMN deleted_at_ns INTEGER NOT NULL DEFAULT 0;
UPDATE media SET deleted_at_ns=updated_at_ns WHERE deleted=1;

-- When a delete of the whole movie or episode (no file id: MovieDelete,
-- SeriesDelete, or history finding no current file) marked the row deleted.
-- A delete of one file only stops an import of that same file; an import of
-- a newer file (an upgrade whose old file's delete arrived first) still
-- revives the row. Rows deleted before this migration keep 0: which kind of
-- delete marked them is unknown, and production revived them on any import.
ALTER TABLE media ADD COLUMN entity_deleted_at_ns INTEGER NOT NULL DEFAULT 0;

-- Sonarr's series type ("standard", "daily", "anime"), which decides whether
-- a name like "Show - 607" numbers episode 607 absolutely (anime) or S06E07.
-- Rows saved before this migration keep '' (read as not anime) until library
-- discovery (scope library-v3) or the next change reads them from Sonarr again.
ALTER TABLE media ADD COLUMN series_type TEXT NOT NULL DEFAULT '';
