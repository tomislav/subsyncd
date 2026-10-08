-- Media order, copied onto each search so a batch that shares one queue position
-- (a discovery, a scan, a newly configured language) leases straight from the
-- lease index in a stable order:
--   sort_title   lower(media.title); SQLite's lower() folds ASCII letters only
--   sort_series  media.series_id (0 for movies), so same-titled shows stay apart
--   sort_season  movies: release year; episodes: season, with specials (season 0)
--                after every regular season
--   sort_episode media.episode
ALTER TABLE search_states ADD COLUMN sort_title TEXT NOT NULL DEFAULT '';
ALTER TABLE search_states ADD COLUMN sort_series INTEGER NOT NULL DEFAULT 0;
ALTER TABLE search_states ADD COLUMN sort_season INTEGER NOT NULL DEFAULT 0;
ALTER TABLE search_states ADD COLUMN sort_episode INTEGER NOT NULL DEFAULT 0;

UPDATE search_states
SET sort_title = lower(media.title),
    sort_series = media.series_id,
    sort_season = CASE WHEN media.kind = 'movie' THEN media.year WHEN media.season = 0 THEN 1000000 ELSE media.season END,
    sort_episode = media.episode
FROM media
WHERE media.id = search_states.media_id;

-- One insert trigger fills both copied fields: the instance rank (from 014) and
-- the media order.
DROP TRIGGER IF EXISTS search_states_instance_rank;
CREATE TRIGGER search_states_queue_order AFTER INSERT ON search_states
BEGIN
    UPDATE search_states
    SET (instance_rank, sort_title, sort_series, sort_season, sort_episode) = (
        SELECT COALESCE(instances.queue_priority, 0),
               lower(media.title),
               media.series_id,
               CASE WHEN media.kind = 'movie' THEN media.year WHEN media.season = 0 THEN 1000000 ELSE media.season END,
               media.episode
        FROM media LEFT JOIN instances ON instances.name = media.instance
        WHERE media.id = NEW.media_id)
    WHERE id = NEW.id AND EXISTS (SELECT 1 FROM media WHERE media.id = NEW.media_id);
END;

CREATE TRIGGER media_search_order AFTER UPDATE OF title, series_id, year, season, episode ON media
WHEN OLD.title IS NOT NEW.title OR OLD.series_id IS NOT NEW.series_id OR OLD.year IS NOT NEW.year
  OR OLD.season IS NOT NEW.season OR OLD.episode IS NOT NEW.episode
BEGIN
    UPDATE search_states
    SET sort_title = lower(NEW.title),
        sort_series = NEW.series_id,
        sort_season = CASE WHEN NEW.kind = 'movie' THEN NEW.year WHEN NEW.season = 0 THEN 1000000 ELSE NEW.season END,
        sort_episode = NEW.episode
    WHERE media_id = NEW.id;
END;

DROP INDEX IF EXISTS search_due_idx;
CREATE INDEX search_due_idx ON search_states(state, priority DESC, instance_rank DESC, queue_order_ns, next_attempt_at_ns, sort_title, sort_series, sort_season, sort_episode, lease_until_ns);
