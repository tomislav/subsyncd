ALTER TABLE search_states ADD COLUMN resume_providers_json TEXT NOT NULL DEFAULT '[]';
ALTER TABLE search_states ADD COLUMN resume_route_signature TEXT NOT NULL DEFAULT '';

UPDATE search_states
SET next_attempt_at_ns = 0
WHERE state='pending' AND priority=200 AND last_outcome='throttled'
  AND NOT EXISTS (
    SELECT 1 FROM installations
    WHERE installations.media_id=search_states.media_id
      AND installations.language=search_states.language
  )
  AND EXISTS (
    SELECT 1 FROM media
    WHERE media.id=search_states.media_id AND media.deleted=0
  );
