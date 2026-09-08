ALTER TABLE instances ADD COLUMN library_scope TEXT NOT NULL DEFAULT '';
ALTER TABLE instances ADD COLUMN event_revision INTEGER NOT NULL DEFAULT 0;

-- Include unknown-entity and whole-series deletes, even without a media row.
CREATE TRIGGER instances_event_revision AFTER INSERT ON events
BEGIN
    UPDATE instances SET event_revision = event_revision + 1 WHERE name = NEW.instance;
END;
