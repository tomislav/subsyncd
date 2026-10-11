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
