-- Media order (title, season, episode), copied onto each search so a batch that
-- shares one queue position leases show by show, from S01E01 upward, straight
-- from the lease index. Titles compare case-insensitively.
ALTER TABLE search_states ADD COLUMN sort_title TEXT NOT NULL DEFAULT '';
ALTER TABLE search_states ADD COLUMN sort_season INTEGER NOT NULL DEFAULT 0;
ALTER TABLE search_states ADD COLUMN sort_episode INTEGER NOT NULL DEFAULT 0;

UPDATE search_states
SET sort_title = COALESCE((SELECT lower(media.title) FROM media WHERE media.id = search_states.media_id), ''),
    sort_season = COALESCE((SELECT media.season FROM media WHERE media.id = search_states.media_id), 0),
    sort_episode = COALESCE((SELECT media.episode FROM media WHERE media.id = search_states.media_id), 0);

CREATE TRIGGER search_states_media_order AFTER INSERT ON search_states
BEGIN
    UPDATE search_states
    SET sort_title = COALESCE((SELECT lower(media.title) FROM media WHERE media.id = NEW.media_id), ''),
        sort_season = COALESCE((SELECT media.season FROM media WHERE media.id = NEW.media_id), 0),
        sort_episode = COALESCE((SELECT media.episode FROM media WHERE media.id = NEW.media_id), 0)
    WHERE id = NEW.id;
END;

CREATE TRIGGER media_search_order AFTER UPDATE OF title, season, episode ON media
WHEN OLD.title IS NOT NEW.title OR OLD.season IS NOT NEW.season OR OLD.episode IS NOT NEW.episode
BEGIN
    UPDATE search_states
    SET sort_title = lower(NEW.title), sort_season = NEW.season, sort_episode = NEW.episode
    WHERE media_id = NEW.id;
END;

DROP INDEX IF EXISTS search_due_idx;
CREATE INDEX search_due_idx ON search_states(state, priority DESC, instance_rank DESC, queue_order_ns, next_attempt_at_ns, sort_title, sort_season, sort_episode, lease_until_ns);
