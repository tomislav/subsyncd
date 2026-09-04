ALTER TABLE notifications ADD COLUMN dedupe_key TEXT;
CREATE UNIQUE INDEX notifications_dedupe_idx ON notifications(dedupe_key) WHERE dedupe_key IS NOT NULL;
CREATE INDEX notifications_due_idx ON notifications(next_attempt_at_ns, lease_until_ns);
