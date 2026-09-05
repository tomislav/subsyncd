-- Absence explicitly means no completed probe, including for pre-migration rows.
CREATE TABLE inventory_probes (
    media_id INTEGER PRIMARY KEY REFERENCES media(id) ON DELETE CASCADE,
    path TEXT NOT NULL,
    file_id INTEGER NOT NULL,
    size INTEGER NOT NULL,
    mod_time_ns INTEGER NOT NULL
);

ALTER TABLE media ADD COLUMN deleted INTEGER NOT NULL DEFAULT 0 CHECK (deleted IN (0, 1));

-- Preserve positively known deletions from the supported pre-003 lineage.
-- A stale removed-language outcome alone is insufficient: every search must
-- agree, the latest retained applied lifecycle event (in commit order) must be
-- a delete, and no newer direct upsert/inventory update may supersede it.
-- Missing/pruned audit evidence remains ambiguous and is not inferred.
WITH latest_lifecycle AS (
    SELECT media_id, MAX(id) AS event_id FROM events
    WHERE outcome='applied' AND event_type IN ('import', 'rename', 'delete')
    GROUP BY media_id
)
UPDATE media SET deleted=1
WHERE id IN (
    SELECT deletion.media_id FROM latest_lifecycle
    JOIN events AS deletion ON deletion.id=latest_lifecycle.event_id
    JOIN media AS snapshot ON snapshot.id=deletion.media_id
    WHERE deletion.event_type='delete'
      AND deletion.created_at_ns >= snapshot.updated_at_ns
)
  AND EXISTS (SELECT 1 FROM search_states WHERE media_id=media.id)
  AND NOT EXISTS (
      SELECT 1 FROM search_states
      WHERE media_id=media.id AND (state!='complete' OR last_outcome!='deleted')
  );

-- Catalog replacement, rename and deletion must never lend old tracks the
-- identity of a new file. Refresh publishes a new marker after its CAS update.
CREATE TRIGGER invalidate_inventory_probe
AFTER UPDATE OF path, file_id, size, mod_time_ns, deleted ON media
WHEN OLD.path != NEW.path OR OLD.file_id != NEW.file_id OR OLD.size != NEW.size
  OR OLD.mod_time_ns != NEW.mod_time_ns OR NEW.deleted = 1
BEGIN
    DELETE FROM inventory_probes WHERE media_id = NEW.id;
END;
