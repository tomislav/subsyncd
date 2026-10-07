-- Finished notifications (delivered or terminally rejected) no longer hold
-- their dedupe key, so the same subtitle published again is delivered again.
-- Completion now releases the key; this releases it for existing rows.
UPDATE notifications SET dedupe_key = NULL WHERE next_attempt_at_ns = 0;
