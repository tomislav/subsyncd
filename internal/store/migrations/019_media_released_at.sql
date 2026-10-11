-- When the episode aired or the movie was released digitally, from Sonarr and
-- Radarr. Missing-subtitle searches for media released long ago skip the
-- quick early retries. 0 means unknown and keeps the normal schedule.
ALTER TABLE media ADD COLUMN released_at_ns INTEGER NOT NULL DEFAULT 0;
