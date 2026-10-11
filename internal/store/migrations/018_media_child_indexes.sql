-- Deleting a media row (forget-instance) runs the candidates cascade and the
-- events SET NULL action once per row; without these indexes each one scans
-- the whole child table.
CREATE INDEX candidates_media_idx ON candidates(media_id);
CREATE INDEX events_media_idx ON events(media_id);
