-- When deferred history first held this instance's reconciliation cursor
-- (RFC 3339; empty while the cursor is advancing normally).
ALTER TABLE instances ADD COLUMN reconciliation_deferred_since TEXT NOT NULL DEFAULT '';
