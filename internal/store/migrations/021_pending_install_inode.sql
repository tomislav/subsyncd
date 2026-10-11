-- The inode of the subtitle file a pending install publishes (the staged
-- file, which linking or renaming keeps). Startup undoes a publication only
-- while the destination is still that file. 0 (rows saved before this
-- migration) falls back to comparing the checksum alone.
ALTER TABLE pending_installs ADD COLUMN published_inode INTEGER NOT NULL DEFAULT 0;
