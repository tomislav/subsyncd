ALTER TABLE search_states ADD COLUMN queue_order_ns INTEGER NOT NULL DEFAULT 0;

-- Queue position starts as the due time, so existing order is unchanged.
UPDATE search_states SET queue_order_ns = next_attempt_at_ns;

-- Configured instance rank (instances[].queue_priority), refreshed at startup and
-- copied onto each search so the lease order can be read from one index.
ALTER TABLE instances ADD COLUMN queue_priority INTEGER NOT NULL DEFAULT 0;
ALTER TABLE search_states ADD COLUMN instance_rank INTEGER NOT NULL DEFAULT 0;

CREATE TRIGGER search_states_instance_rank AFTER INSERT ON search_states
BEGIN
    UPDATE search_states
    SET instance_rank = COALESCE((
        SELECT instances.queue_priority FROM media JOIN instances ON instances.name = media.instance
        WHERE media.id = NEW.media_id), 0)
    WHERE id = NEW.id;
END;

DROP INDEX IF EXISTS search_due_idx;
CREATE INDEX search_due_idx ON search_states(state, priority DESC, instance_rank DESC, queue_order_ns, next_attempt_at_ns, lease_until_ns);
