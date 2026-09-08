ALTER TABLE installations ADD COLUMN fallback INTEGER NOT NULL DEFAULT 0 CHECK (fallback IN (0, 1));
