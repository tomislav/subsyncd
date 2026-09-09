-- Movie selection now deduplicates eligible normalized content. Old rejection
-- rows lack selection rules, so reconsider movie pack-selection failures once.
-- Preserve episode/content/LAPSE failures and all search/installation state.
DELETE FROM candidate_rejections
WHERE reason_code = 'pack_selection'
  AND media_id IN (SELECT id FROM media WHERE kind = 'movie');
