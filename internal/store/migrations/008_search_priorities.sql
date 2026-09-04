ALTER TABLE search_states ADD COLUMN priority INTEGER NOT NULL DEFAULT 200;
ALTER TABLE search_states ADD COLUMN rerun_requested INTEGER NOT NULL DEFAULT 0 CHECK (rerun_requested IN (0, 1));

DROP INDEX search_due_idx;
CREATE INDEX search_due_idx
ON search_states(state, priority DESC, next_attempt_at_ns, lease_until_ns);
