-- Absence explicitly means no completed probe, including for pre-migration rows.
CREATE TABLE inventory_probes (
    media_id INTEGER PRIMARY KEY REFERENCES media(id) ON DELETE CASCADE,
    path TEXT NOT NULL,
    file_id INTEGER NOT NULL,
    size INTEGER NOT NULL,
    mod_time_ns INTEGER NOT NULL
);

ALTER TABLE media ADD COLUMN deleted INTEGER NOT NULL DEFAULT 0 CHECK (deleted IN (0, 1));

-- Catalog replacement, rename and deletion must never lend old tracks the
-- identity of a new file. Refresh publishes a new marker after its CAS update.
CREATE TRIGGER invalidate_inventory_probe
AFTER UPDATE OF path, file_id, size, mod_time_ns, deleted ON media
WHEN OLD.path != NEW.path OR OLD.file_id != NEW.file_id OR OLD.size != NEW.size
  OR OLD.mod_time_ns != NEW.mod_time_ns OR NEW.deleted = 1
BEGIN
    DELETE FROM inventory_probes WHERE media_id = NEW.id;
END;
