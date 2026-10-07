-- Last episode of a multi-episode file (0 = single episode; -1 = a legacy
-- unsupported multi-episode file that was re-checked and is still unsupported).
ALTER TABLE media ADD COLUMN episode_end INTEGER NOT NULL DEFAULT 0;
ALTER TABLE media ADD COLUMN absolute_episode_end INTEGER NOT NULL DEFAULT 0;
