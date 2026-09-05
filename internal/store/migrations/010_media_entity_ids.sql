ALTER TABLE media ADD COLUMN entity_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE events ADD COLUMN entity_id INTEGER NOT NULL DEFAULT 0;
CREATE UNIQUE INDEX media_entity_identity_idx
    ON media(instance, kind, entity_id)
    WHERE entity_id > 0;
