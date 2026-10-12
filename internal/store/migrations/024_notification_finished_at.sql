-- Finished notifications (delivered, skipped, refused or permanently failed)
-- are deleted NotificationRetention after they finish. Completion now records
-- when; rows that finished before this migration count as finished now. The
-- partial index serves the bounded prune, which never touches pending,
-- retry-scheduled or leased rows (next_attempt_at_ns > 0); its leading
-- column lets the planner prefer it over notifications_due_idx.
ALTER TABLE notifications ADD COLUMN finished_at_ns INTEGER;
UPDATE notifications SET finished_at_ns = CAST(strftime('%s', 'now') AS INTEGER) * 1000000000 WHERE next_attempt_at_ns = 0;
CREATE INDEX notifications_finished_idx ON notifications(next_attempt_at_ns, finished_at_ns) WHERE next_attempt_at_ns = 0;
