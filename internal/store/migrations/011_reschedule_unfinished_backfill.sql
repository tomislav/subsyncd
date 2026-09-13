-- Give unfinished first-install searches one immediate opportunity to adopt
-- provider-reset scheduling. Preserve attempt counters and rejection evidence.
UPDATE search_states
SET next_attempt_at_ns = 0
WHERE state = 'pending'
  AND priority = 200
  AND last_outcome = 'no_result'
  AND NOT EXISTS (
      SELECT 1
      FROM installations
      WHERE installations.media_id = search_states.media_id
        AND installations.language = search_states.language
  )
  AND EXISTS (
      SELECT 1
      FROM media
      WHERE media.id = search_states.media_id
        AND media.deleted = 0
  );
