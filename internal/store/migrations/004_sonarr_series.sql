-- Existing episode rows acquire their series identity on ordinary hydration.
-- Startup remains offline; unknown identities are never inferred from titles.
ALTER TABLE media ADD COLUMN series_id INTEGER NOT NULL DEFAULT 0 CHECK (series_id >= 0);
ALTER TABLE events ADD COLUMN series_id INTEGER NOT NULL DEFAULT 0 CHECK (series_id >= 0);
CREATE INDEX media_series_idx ON media(instance, kind, series_id) WHERE series_id > 0;
