ALTER TABLE search_states ADD COLUMN queue_order_ns INTEGER NOT NULL DEFAULT 0;

-- Queue position starts as the due time, so existing order is unchanged.
UPDATE search_states SET queue_order_ns = next_attempt_at_ns;

DROP INDEX IF EXISTS search_due_idx;
CREATE INDEX search_due_idx ON search_states(state, priority DESC, queue_order_ns, next_attempt_at_ns, lease_until_ns);
